package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/routes"
)

func TestClaudeTeamMultipleOrganizationsKeepCredentialsQuotasAndRoutesSeparate(t *testing.T) {
	tm, _, c := teamFixture(t)
	tm.note("u3@example.com · Team", []ClaudeLimit{{Kind: "five_hour", Used: .23, ResetsAt: time.Now().Add(time.Hour).Unix()}, {Kind: "seven_day", Used: .67, ResetsAt: time.Now().Add(24 * time.Hour).Unix()}})
	if err := tm.persist(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(tm.statePath())
	other := c
	other.OrganizationID, other.SessionKey, other.Name = teamID(70), "second-private-admin", "Second team"
	other.Members = []string{teamID(71), teamID(72)}
	other.Enabled = false
	ls := readLogins()
	for _, n := range []int{71, 72} {
		profile, _ := json.Marshal(map[string]string{"emailAddress": teamID(n) + "@example.com", "accountUuid": teamID(n), "organizationUuid": other.OrganizationID})
		ls = append(ls, savedLogin{Agent: "claude", User: teamID(n) + " · Other", Profile: profile})
	}
	if err := writeLogins(ls); err != nil {
		t.Fatal(err)
	}
	if err := SaveClaudeTeam(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	second, err := claudeTeamForOrg(other.OrganizationID, false)
	if err != nil || second == tm || second.statePath() == tm.statePath() {
		t.Fatal("organizations share a controller or journal")
	}
	after, _ := os.ReadFile(tm.statePath())
	if string(before) != string(after) {
		t.Fatal("adding another organization rewrote the first quota journal")
	}
	views := ClaudeTeamSubscriptions()
	if len(views) != 2 {
		t.Fatalf("subscriptions = %d", len(views))
	}
	b, _ := json.Marshal(views)
	if strings.Contains(string(b), c.SessionKey) || strings.Contains(string(b), other.SessionKey) {
		t.Fatal("organization listing leaked credentials")
	}
	for _, v := range views {
		for _, m := range v.Members {
			if m.OrganizationID != v.Config.OrganizationID {
				t.Fatal("cross-organization member exposed")
			}
		}
	}
	base := claudeProvider(&Account{Agent: "claude"})
	ps := claudeTeamProviders(base)
	if len(ps) != 2 || ps[0].ID == ps[1].ID {
		t.Fatal("not independently addressable subscriptions")
	}
	for _, p := range ps {
		for _, q := range p.AlsoOn() {
			if !claudeTeamLoginBelongs(ClaudeTeamOf(p), q.Account.User) {
				t.Fatal("cross-organization inference route")
			}
		}
	}
	ws, err := claudeWindows(context.Background(), "u3@example.com · Team", false)
	if err != nil || len(ws) != 2 || ws[1].Used != 67 {
		t.Fatalf("cached quotas unavailable: %+v %v", ws, err)
	}
	if err := tm.save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	var persisted claudeTeamDisk
	b, _ = os.ReadFile(tm.statePath())
	_ = json.Unmarshal(b, &persisted)
	if len(persisted.Limits[teamID(3)]) != 2 {
		t.Fatal("saving config lost quota snapshots")
	}
}

func TestClaudeTeamStandbyUsesRoutingAndBothResetTimes(t *testing.T) {
	tm, _, c := teamFixture(t)
	now := time.Now().Truncate(time.Hour)
	seats := []claudeTeamSeat{{teamID(2), "team_tier_1"}, {teamID(3), "unassigned"}, {teamID(4), "unassigned"}}
	tm.disk.Limits[teamID(2)] = []ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: now.Add(4 * time.Hour).Unix()}}
	window := func(five, week time.Duration) []ClaudeLimit {
		return []ClaudeLimit{{Kind: "five_hour", Used: .2, ResetsAt: now.Add(five).Unix()}, {Kind: "seven_day", Used: .2, ResetsAt: now.Add(week).Unix()}}
	}
	check := func(want string) {
		t.Helper()
		_, got, err := tm.choose(c, seats, "")
		if err != nil || got != want {
			t.Fatalf("standby = %s, want %s: %v", got, want, err)
		}
	}
	tm.disk.Limits[teamID(3)], tm.disk.Limits[teamID(4)] = window(2*time.Hour, 72*time.Hour), window(4*time.Hour, 24*time.Hour)
	check(teamID(4)) // week, not member order or short window alone
	tm.disk.Limits[teamID(3)], tm.disk.Limits[teamID(4)] = window(4*time.Hour, 24*time.Hour), window(2*time.Hour, 24*time.Hour)
	check(teamID(4)) // equal week: five-hour reset decides
	tm.disk.Limits[teamID(4)][1].Used = 1
	check(teamID(3)) // an exhausted weekly window blocks an otherwise fresh 5h
	tm.disk.Limits[teamID(4)][1].ResetsAt = now.Add(-time.Hour).Unix()
	tm.disk.Limits[teamID(3)][0].Used = 1
	check(teamID(4)) // eligible again, without rewriting its 100% observation
	if tm.disk.Limits[teamID(4)][1].Used != 1 {
		t.Fatal("invented usage after reset")
	}
	remove := routes.Register(Ordered, routes.Ordered)
	defer remove()
	tm.disk.Limits[teamID(3)][0].Used = .2
	c.Routing = Ordered
	check(teamID(3)) // explicit route policy, no hidden preferred account
}

func TestClaudeTeamColdQuotaCacheFeedsRoutingWhenStandaloneClaudeIsOff(t *testing.T) {
	tm, _, c := teamFixture(t)
	tm.note("u2@example.com · Team", []ClaudeLimit{{Kind: "five_hour", Used: .23, ResetsAt: time.Now().Add(time.Hour).Unix()}, {Kind: "seven_day", Used: .67, ResetsAt: time.Now().Add(24 * time.Hour).Unix()}})
	// The old standalone subscription can stay off while this organization
	// has its own normal provider switch. No quota probe is authorized here.
	if err := store(file{Providers: []Provider{{ID: "claude", Off: true}, {ID: claudeTeamPrefix + c.OrganizationID}}}); err != nil {
		t.Fatal(err)
	}
	qs := LoginUsage(context.Background(), "claude")
	q, ok := qs["u2@example.com · Team"]
	if !ok || len(q.Windows) != 2 || q.Windows[1].Used != 67 || q.Error != "" || !q.ReadDeferred || q.AsOf == nil {
		t.Fatalf("routing did not receive persisted Team windows: %+v", q)
	}
	a := allowanceOf(q.Windows, time.Now())
	if used, _ := a.For("", time.Now()); used != 67 {
		t.Fatalf("wrong allowance: %v", used)
	}
	// A parked account's explicit refresh is still a local snapshot.
	tm.note("u3@example.com · Team", []ClaudeLimit{{Kind: "seven_day", Used: 1, ResetsAt: time.Now().Add(time.Hour).Unix()}})
	for _, l := range Logins("claude") {
		if strings.HasPrefix(l.User, "u3@") {
			ctx := context.WithValue(Asked(context.Background()), cardRefreshKey{}, cardRefresh{claudeTeamPrefix + c.OrganizationID, l.User})
			read := loginReading(ctx, l).read
			if len(read.Windows) != 1 || read.Windows[0].Used != 100 || !read.ReadDeferred || read.Error != "" {
				t.Fatalf("parked snapshot missing: %+v", read)
			}
		}
	}
}

func TestClaudeTeamQuotaRefreshUsesSavedCodeCommandAndKeepsSnapshotOnFailure(t *testing.T) {
	tm, _, _ := teamFixture(t)
	user := "u2@example.com · Team"
	runs := 0
	old := UsageClaudeVia(func(_ context.Context, dir string) (string, error) {
		runs++
		if dir != claudeAccountDir(user) {
			t.Error("quota command used the agent's own login or another account")
		}
		return "Current session: 17% used\nCurrent week (all models): 38% used", nil
	})
	t.Cleanup(func() { UsageClaudeVia(old) })
	tm.refreshQuota(context.Background(), teamID(2))
	ws, ok := claudeTeamQuota(user)
	if runs != 1 || !ok || len(ws) != 2 || ws[0].Used != 17 || ws[1].Used != 38 || claudeTeamObservedAt(user) == nil {
		t.Fatalf("missing fresh quota: %+v", ws)
	}
	UsageClaudeVia(func(context.Context, string) (string, error) { return "", errors.New("fixture temporary quota outage") })
	tm.refreshQuota(context.Background(), teamID(2))
	ws, ok = claudeTeamQuota(user)
	if !ok || len(ws) != 2 || ws[1].Used != 38 {
		t.Fatal("a transient quota error erased the last observation")
	}
}

func TestClaudeTeamDisabledProviderDoesNotRotateOrResurrectAccounts(t *testing.T) {
	tm, f, c := teamFixture(t)
	tm.note("u2@example.com · Team", []ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: time.Now().Add(time.Hour).Unix()}})
	if err := store(file{Providers: []Provider{{ID: claudeTeamPrefix + c.OrganizationID, Off: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := tm.reconcile(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) > 0 {
		t.Fatal("disabled subscription moved a real seat")
	}
	c.Enabled = false
	if err := tm.save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	p := claudeProvider(&Account{Agent: "claude", User: "u3@example.com · Team"})
	p.ID = claudeTeamPrefix + c.OrganizationID
	if p.TeamRouteOn() || len(p.AlsoOn()) != 0 {
		t.Fatal("disabled controller resurrected an unassigned login")
	}
}

func TestClaudeTeamCorruptOrganizationIsQuarantinedWithoutWrites(t *testing.T) {
	_, _, _ = teamFixture(t)
	org := teamID(70)
	profile, _ := json.Marshal(map[string]string{"emailAddress": "other@example.com", "accountUuid": teamID(71), "organizationUuid": org})
	ls := append(readLogins(), savedLogin{Agent: "claude", User: "other@example.com · Other", Profile: profile, On: true})
	if err := writeLogins(ls); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(filepath.Dir(Path()), "claude-teams", org)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	discoverClaudeTeams()
	v, err := ClaudeTeamOrganization(org)
	if err != nil || v.Error == "" || v.Config.Enabled || len(v.Config.Members) != 1 {
		t.Fatal("corrupt organization was not quarantined")
	}
	if !claudeLoginInTeam("other@example.com · Other") {
		t.Fatal("corrupt organization leaked into standalone Claude")
	}
	ctrl, _ := claudeTeamForOrg(org, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctrl.keep(ctx)
	b, _ := os.ReadFile(file)
	if string(b) != "{broken" || !claudeLoginInTeam("other@example.com · Other") {
		t.Fatal("startup rewrote or resurrected a corrupt organization")
	}
}
