package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const teamOrg = "00000000-0000-4000-8000-000000000001"

func teamID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

type fakeTeamAdmin struct {
	mu                       sync.Mutex
	seats                    []claudeTeamSeat
	writes                   []string
	readErr                  error
	failTarget, loseResponse bool
	entered                  chan string
}

func (f *fakeTeamAdmin) Members(context.Context) ([]claudeTeamSeat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seats), f.readErr
}
func (f *fakeTeamAdmin) SetTier(_ context.Context, id, tier string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, id+":"+tier)
	if f.failTarget && id == teamID(3) && tier == "team_tier_1" {
		return errors.New("fixture refusal")
	}
	for i := range f.seats {
		if f.seats[i].AccountID == id {
			f.seats[i].SeatTier = tier
		}
	}
	if f.entered != nil {
		select {
		case f.entered <- id:
		default:
		}
	}
	if f.loseResponse {
		return errors.New("fixture response lost after commit")
	}
	return nil
}

func teamFixture(t *testing.T) (*claudeTeamController, *fakeTeamAdmin, ClaudeTeamConfig) {
	t.Helper()
	isolate(t)
	// Real captures also warm the generic allowance cache. Fixtures reuse
	// emails, so no previous test's observation may seed this pool on save.
	claudeUsage.Lock()
	oldUsage := claudeUsage.m
	claudeUsage.m = map[string]claudeUsageEntry{}
	claudeUsage.Unlock()
	t.Cleanup(func() { claudeUsage.Lock(); claudeUsage.m = oldUsage; claudeUsage.Unlock() })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	before, factory := claudeTeam, newClaudeTeamAdmin
	t.Cleanup(func() { claudeTeam, newClaudeTeamAdmin = before, factory })
	tm := newClaudeTeamController()
	claudeTeam = tm
	f := &fakeTeamAdmin{seats: []claudeTeamSeat{{teamID(2), "team_tier_1"}, {teamID(3), "unassigned"}, {teamID(4), "team_tier_1"}, {teamID(9), "team_tier_1"}}}
	newClaudeTeamAdmin = func(ClaudeTeamConfig) claudeTeamAdmin { return f }
	var ls []savedLogin
	for _, n := range []int{2, 3, 4} {
		profile, _ := json.Marshal(map[string]string{"emailAddress": fmt.Sprintf("u%d@example.com", n), "accountUuid": teamID(n), "organizationUuid": teamOrg})
		ls = append(ls, savedLogin{Agent: "claude", User: fmt.Sprintf("u%d@example.com · Team", n), Profile: profile, On: n == 3, Auth: json.RawMessage(`{"claudeAiOauth":{"accessToken":"fixture-only","refreshToken":"fixture-refresh","expiresAt":9999999999999}}`)})
	}
	if err := writeLogins(ls); err != nil {
		t.Fatal(err)
	}
	c := ClaudeTeamConfig{Enabled: true, OrganizationID: teamOrg, SessionKey: "fixture-private-key", Threshold: 100, Members: []string{teamID(2), teamID(3), teamID(4)}}
	if err := tm.save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return tm, f, c
}

func TestClaudeTeamNativeSwitchesAndSecret(t *testing.T) {
	tm, f, c := teamFixture(t)
	status := ClaudeTeamStatus()
	b, _ := json.Marshal(status)
	if strings.Contains(string(b), c.SessionKey) || !status.SessionSet || status.Seats != 2 || status.InUse != 2 {
		t.Fatalf("unsafe or incorrect status: %s", b)
	}
	st, err := os.Stat(claudeTeamPath())
	if err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
		t.Fatalf("private config: %v %v", st, err)
	}
	for _, l := range readLogins() {
		id, _ := savedSessionIdentity(l)
		want := id.AccountID != teamID(3)
		if l.On != want || l.TeamOff == want {
			t.Fatalf("switch for %s: on %v parked %v", id.AccountID, l.On, l.TeamOff)
		}
	}
	if len(f.writes) != 0 {
		t.Fatal("configuration allocated a seat")
	}
	// A stale rejected standby request never starts inference.
	if release, ok := tm.track("u3@example.com · Team"); ok {
		release()
		t.Fatal("unassigned account admitted")
	}
	c.Enabled = false
	if err := tm.save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, l := range readLogins() {
		id, _ := savedSessionIdentity(l)
		if l.On != (id.AccountID == teamID(3)) || l.TeamOff {
			t.Fatalf("original switches not restored: %+v", l.User)
		}
	}
}

func TestClaudeTeamDrainMultipleSeatsAndLostResponses(t *testing.T) {
	tm, f, c := teamFixture(t)
	for _, kind := range []string{"five_hour", "seven_day"} {
		tm.note("u2@example.com · Team", []ClaudeLimit{{Kind: kind, Used: 1, ResetsAt: time.Now().Add(time.Hour).Unix()}})
	}
	release, ok := tm.track("u2@example.com · Team")
	if !ok {
		t.Fatal("active seat rejected")
	}
	f.loseResponse = true
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { tm.op.Lock(); defer tm.op.Unlock(); done <- tm.reconcile(ctx, "") }()
	// Wait on the actual closed/draining state, not elapsed time.
	for {
		tm.mu.Lock()
		closed, ch := tm.closed[teamID(2)], tm.changed
		tm.mu.Unlock()
		if closed {
			break
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("never started draining")
		}
	}
	f.mu.Lock()
	n := len(f.writes)
	f.mu.Unlock()
	if n != 0 {
		t.Fatal("seat revoked while a request was active")
	}
	if r, ok := tm.track("u2@example.com · Team"); ok {
		r()
		t.Fatal("stale source admitted while draining")
	}
	other, ok := tm.track("u4@example.com · Team")
	if !ok {
		t.Fatal("another Premium seat was stopped")
	}
	other()
	release()
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	writes := slices.Clone(f.writes)
	seats := slices.Clone(f.seats)
	f.mu.Unlock()
	if strings.Join(writes, ",") != teamID(2)+":unassigned,"+teamID(3)+":team_tier_1" {
		t.Fatalf("blind retry or wrong target: %v", writes)
	}
	if got := premiumSet(seats); !slices.Equal(got, []string{teamID(3), teamID(4), teamID(9)}) {
		t.Fatalf("other seats changed: %v", got)
	}
	if tm.disk.Pending != nil || len(tm.disk.History) != 1 || tm.disk.History[0].Result != "assigned" {
		t.Fatal("transaction was not reconciled")
	}
	if err := tm.synchronize(c, seats); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeTeamRollbackAndCrashRecovery(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			tm, f, c := teamFixture(t)
			if restart {
				p := claudeTeamPending{OrganizationID: teamOrg, From: teamID(2), To: teamID(3), Baseline: premiumSet(f.seats)}
				tm.disk.Pending = &p
				f.seats[0].SeatTier = "unassigned"
				if err := tm.reconcile(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
			} else {
				f.failTarget = true
				if err := tm.transfer(context.Background(), c, f, slices.Clone(f.seats), teamID(2), teamID(3)); err != nil {
					t.Fatal(err)
				}
			}
			if f.seats[0].SeatTier != "team_tier_1" || f.seats[1].SeatTier != "unassigned" || tm.disk.Pending != nil || tm.disk.History[0].Result != "rolled_back" {
				t.Fatalf("rollback missing: %+v", tm.disk.History)
			}
		})
	}
}

func TestClaudeTeamFailurePreservesRoutesAndConcurrentChangeStopsWrites(t *testing.T) {
	tm, f, _ := teamFixture(t)
	f.readErr = errors.New("fixture offline")
	before, _ := os.ReadFile(loginsPath())
	if err := tm.reconcile(context.Background(), ""); err == nil {
		t.Fatal("API failure swallowed")
	}
	after, _ := os.ReadFile(loginsPath())
	if string(before) != string(after) {
		t.Fatal("transient API failure changed switches")
	}
	f.readErr = nil
	p := claudeTeamPending{OrganizationID: teamOrg, From: teamID(2), To: teamID(3), Baseline: premiumSet(f.seats)}
	f.seats[0].SeatTier = "unassigned"
	f.seats[3].SeatTier = "unassigned"
	tm.disk.Pending = &p
	if err := tm.reconcile(context.Background(), ""); err == nil || len(f.writes) != 0 {
		t.Fatal("concurrent outside-pool change was overwritten")
	}
}

func TestClaudeTeamRestartOfflineKeepsOnlyOwnedRoutes(t *testing.T) {
	for _, mode := range []string{"snapshot", "legacy", "pending", "not-owned", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			tm, f, c := teamFixture(t)
			if mode == "legacy" || mode == "not-owned" {
				tm.disk.Seats = nil
			}
			if mode == "pending" {
				tm.disk.Pending = &claudeTeamPending{OrganizationID: teamOrg, From: teamID(2), To: teamID(3), Baseline: premiumSet(f.seats)}
			}
			if mode == "not-owned" {
				tm.disk.Original = nil
			}
			if mode == "disabled" {
				c.Enabled = false
			}
			if err := tm.persist(); err != nil {
				t.Fatal(err)
			}
			next := newClaudeTeamController()
			claudeTeam = next
			next.cfg = c
			if err := next.loadState(c); err != nil {
				t.Fatal(err)
			}
			if _, err := next.identities(c); err != nil {
				t.Fatal(err)
			}
			f.readErr = errors.New("fixture Cloudflare challenge")
			before, _ := os.ReadFile(loginsPath())
			if err := next.reconcile(context.Background(), ""); mode != "disabled" && err == nil {
				t.Fatal("admin outage hidden")
			}
			if mode == "disabled" {
				if len(next.seats) != 0 || len(next.legacyRoutes) != 0 || len(f.writes) != 0 {
					t.Fatal("disabled controller restored cached routes or changed cloud assignments")
				}
				return // disabled control restores original switches, not cached routes
			}
			after, _ := os.ReadFile(loginsPath())
			if string(before) != string(after) || len(f.writes) != 0 {
				t.Fatal("outage wrote login switches or cloud assignments")
			}
			for _, n := range []int{2, 3, 4} {
				p := Provider{ID: claudeTeamPrefix + teamOrg, Account: &Account{Agent: "claude", User: fmt.Sprintf("u%d@example.com · Team", n)}}
				want := (mode == "snapshot" || mode == "legacy") && n != 3
				if p.TeamRouteOn() != want {
					t.Fatalf("%s member %d: route=%v want=%v", mode, n, p.TeamRouteOn(), want)
				}
			}
			if mode == "legacy" && len(next.seats) != 0 {
				t.Fatal("legacy switches fabricated verified cloud assignments")
			}
		})
	}
}

func TestClaudeTeamHTTPChallengeIsNotExpiredCredential(t *testing.T) {
	for _, challenge := range []bool{false, true} {
		t.Run(fmt.Sprint(challenge), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if challenge {
					w.Header().Set("cf-mitigated", "challenge")
				}
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, "private-response-body fixture-private-key")
			}))
			defer server.Close()
			client := &claudeTeamHTTP{org: teamOrg, key: "fixture-private-key", base: server.URL, client: server.Client()}
			_, err := client.Members(context.Background())
			if err == nil || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "expired") || strings.Contains(err.Error(), "Cloudflare") != challenge {
				t.Fatalf("challenge misclassified or secret leaked: %v", err)
			}
		})
	}
}

func TestClaudeTeamCachedWindowsAndNoHoursLongWait(t *testing.T) {
	tm, _, c := teamFixture(t)
	now := time.Now()
	for _, id := range c.Members {
		tm.disk.Limits[id] = []ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: now.Add(time.Hour).Unix()}, {Kind: "seven_day", Used: 1, ResetsAt: now.Add(48 * time.Hour).Unix()}}
	}
	if _, to, err := tm.choose(c, []claudeTeamSeat{{teamID(2), "team_tier_1"}, {teamID(3), "unassigned"}, {teamID(4), "unassigned"}}, ""); to != "" || err == nil {
		t.Fatal("chose a still-exhausted account")
	}
	back, ok := teamBack(tm.disk.Limits[teamID(3)], 100, now)
	if !ok || back.Unix() != now.Add(48*time.Hour).Unix() {
		t.Fatal("ignored longer weekly reset")
	}
	tm.disk.Limits[teamID(3)] = []ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: now.Add(-time.Hour).Unix()}, {Kind: "seven_day", Used: .1, ResetsAt: now.Add(time.Hour).Unix()}}
	if teamSpent(tm.disk.Limits[teamID(3)], 100, now) {
		t.Fatal("elapsed reset blocked a probe")
	}
	if tm.disk.Limits[teamID(3)][0].Used != 1 {
		t.Fatal("invented a zero reading")
	}
	if AwaitClaudeTeamRotation(context.Background()) {
		t.Fatal("waited for a reset without a handoff")
	}
	// Refreshing discovery must not resurrect a parked account.
	ls := readLogins()
	parked := ls[0]
	parked.TeamOff = true
	ls[0] = parked
	parked.TeamOff = false
	if !upsertLogin(ls, parked)[0].TeamOff {
		t.Fatal("discovery lost controller-owned switch")
	}
}

func TestClaudeTeamHTTPRealShapeNoRedirectOrSecretLeak(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		cookie, err := r.Cookie("sessionKey")
		if err != nil || cookie.Value != "fixture-private-key" {
			t.Error("session not sent")
		}
		if r.UserAgent() != claudeTeamWebAgent {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method == "PUT" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["seat_tier"] != "unassigned" {
				t.Error("unexpected mutation")
			}
			w.WriteHeader(204)
			return
		}
		fmt.Fprintf(w, `{"members":[{"member":{"account":{"uuid":%q},"seat_tier":"team_tier_1"}}],"has_more":false}`, teamID(2))
	}))
	defer server.Close()
	client := &claudeTeamHTTP{org: teamOrg, key: "fixture-private-key", base: server.URL, client: server.Client()}
	seats, err := client.Members(context.Background())
	if err != nil || len(seats) != 1 || seats[0].AccountID != teamID(2) {
		t.Fatalf("members: %v %v", seats, err)
	}
	if err := client.SetTier(context.Background(), teamID(2), "unassigned"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(paths[0], "/members_v2?offset=0&limit=15&types%5B%5D=member") {
		t.Fatalf("not verified endpoint: %v", paths)
	}
	if err := client.SetTier(context.Background(), "not-a-uuid", "team_tier_1"); err == nil {
		t.Fatal("bad ID accepted")
	}
}

func TestClaudeTeamRestartRestoresFlagsAndCorruptJournalIsPreserved(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			_, _, c := teamFixture(t)
			if broken {
				if err := os.WriteFile(claudeTeamStatePath(), []byte("{broken recovery"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			next := newClaudeTeamController()
			claudeTeam = next
			c.Enabled = false
			err := next.save(context.Background(), c)
			if broken {
				b, _ := os.ReadFile(claudeTeamStatePath())
				if err == nil || string(b) != "{broken recovery" {
					t.Fatal("corrupt journal overwritten")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range readLogins() {
				id, _ := savedSessionIdentity(l)
				if l.On != (id.AccountID == teamID(3)) || l.TeamOff {
					t.Fatalf("restart lost original switch: %s", l.User)
				}
			}
		})
	}
}

func TestClaudeTeamParkedQuotaUsesLastObservationWithoutProbe(t *testing.T) {
	tm, _, _ := teamFixture(t)
	reset := time.Now().Add(-time.Hour).Unix()
	tm.note("u3@example.com · Team", []ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: reset}})
	claudeCLIUsage.RLock()
	before := claudeCLIUsage.f
	claudeCLIUsage.RUnlock()
	calls := 0
	UsageClaudeVia(func(context.Context, string) (string, error) { calls++; return "", errors.New("must not be called") })
	t.Cleanup(func() { UsageClaudeVia(before) })
	ws, err := claudeWindows(context.Background(), "u3@example.com · Team", true)
	if err != nil || len(ws) != 1 || ws[0].Used != 0 || ws[0].ResetsAt != nil || !ws[0].Estimated {
		t.Fatalf("parked snapshot was not projected explicitly: %+v %v", ws, err)
	}
	if tm.disk.Limits[teamID(3)][0].Used != 1 || tm.disk.Limits[teamID(3)][0].ResetsAt != reset {
		t.Fatal("projection changed the actual observation")
	}
	if calls != 0 {
		t.Fatal("unassigned account was probed")
	}
}

func TestClaudeTeamPendingAllowsNewAdministratorSessionNotNewPool(t *testing.T) {
	tm, f, c := teamFixture(t)
	p := claudeTeamPending{OrganizationID: teamOrg, From: teamID(2), To: teamID(3), Baseline: premiumSet(f.seats)}
	tm.disk.Pending = &p
	if err := tm.persist(); err != nil {
		t.Fatal(err)
	}
	c.SessionKey = "fixture-renewed-session"
	if err := tm.save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if tm.cfg.SessionKey != c.SessionKey || tm.disk.Pending == nil || len(f.writes) != 0 {
		t.Fatal("session update lost or changed the unfinished transaction")
	}
	c.Members = []string{teamID(2), teamID(3)}
	if err := tm.save(context.Background(), c); err == nil {
		t.Fatal("unfinished plan was rewritten")
	}
}

func TestClaudeTeamHTTPRejectsRedirectAndDoesNotReflectServerBody(t *testing.T) {
	c := ClaudeTeamConfig{OrganizationID: teamOrg, SessionKey: "fixture-private-key"}
	client := newClaudeTeamAdmin(c).(*claudeTeamHTTP)
	var called atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Store(true); w.WriteHeader(204) }))
	defer target.Close()
	for _, status := range []int{302, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				fmt.Fprint(w, "fixture-private-key")
			}))
			defer server.Close()
			client.base = server.URL
			_, err := client.Members(context.Background())
			if err == nil || strings.Contains(err.Error(), c.SessionKey) || called.Load() {
				t.Fatal("redirect followed or server body leaked")
			}
		})
	}
}
