package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
)

const nativeTeamOrg = "00000000-0000-4000-8000-000000000001"

func TestClaudeTeamLegacyScopesStayWithOriginalOrganization(t *testing.T) {
	p := provider.Provider{ID: "claude-team-" + nativeTeamOrg, Was: []string{"claude"}, Key: "fixture-only-a"}
	who := access.Identity{Models: []string{"claude/sonnet"}, Accounts: []string{"claude/" + p.AccountID()}}
	if !modelAllowed(who, p, "sonnet") || !accountAllowed(who, candidate{p: p}) {
		t.Fatal("original scoped route lost during migration")
	}
	wrong := p
	wrong.Key = "fixture-only-b"
	if accountAllowed(who, candidate{p: wrong}) {
		t.Fatal("migration broadened the original account restriction")
	}
	other := p
	other.ID = "claude-team-" + nativeTeamID(70)
	other.Was = nil
	if modelAllowed(who, other, "sonnet") {
		t.Fatal("new organization inherited legacy model grants")
	}
	who.Accounts = append(who.Accounts, p.ID+"/"+wrong.AccountID())
	if accountAllowed(who, candidate{p: p}) || accountAllowed(who, candidate{p: wrong}) {
		t.Fatal("old and explicit new scopes must both be respected")
	}
}

func nativeTeamID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

type nativeTeamTransport struct {
	base          http.RoundTripper
	mu            sync.Mutex
	premium       string
	reads, writes int
	offline       bool
}

func (f *nativeTeamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "claude.ai" {
		return f.base.RoundTrip(r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.offline {
		return nil, fmt.Errorf("fixture unavailable")
	}
	body := "{}"
	if r.Method == "PUT" {
		f.writes++
		var in struct {
			SeatTier string `json:"seat_tier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		if in.SeatTier == "unassigned" {
			f.premium = ""
		} else {
			f.premium = filepath.Base(r.URL.Path)
		}
	} else {
		f.reads++
		var members []map[string]string
		for _, n := range []int{2, 3} {
			tier := "unassigned"
			if nativeTeamID(n) == f.premium {
				tier = "team_tier_1"
			}
			members = append(members, map[string]string{"uuid": nativeTeamID(n), "seat_tier": tier})
		}
		b, _ := json.Marshal(map[string]any{"members": members, "has_more": false})
		body = string(b)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func nativeTeamFixture(t *testing.T) *nativeTeamTransport {
	t.Helper()
	claudeMadeFirst(t, false)
	oldUsage := provider.UsageClaudeVia(func(context.Context, string) (string, error) {
		return "Current session: 10% used\nCurrent week (all models): 10% used", nil
	})
	t.Cleanup(func() { provider.UsageClaudeVia(oldUsage) })
	home := os.Getenv("HOME")
	profile := func(user string, n int) map[string]any {
		return map[string]any{"emailAddress": user, "accountUuid": nativeTeamID(n), "organizationUuid": nativeTeamOrg}
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), mustJSON(map[string]any{"oauthAccount": profile("a@example.com", 2)}), 0600); err != nil {
		t.Fatal(err)
	}
	var ls []map[string]any
	file := filepath.Join(filepath.Dir(provider.Path()), "logins.json")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &ls); err != nil {
		t.Fatal(err)
	}
	ls[0]["profile"] = profile("b@example.com", 3)
	if err = os.WriteFile(file, mustJSON(ls), 0600); err != nil {
		t.Fatal(err)
	}
	provider.ForgetAccounts()
	provider.Logins("claude") // saves the agent's own A alongside B
	for _, user := range []string{"a@example.com", "b@example.com"} {
		provider.NoteClaudeLimits(user, []provider.ClaudeLimit{{Kind: "five_hour", Used: 0.1, ResetsAt: time.Now().Add(time.Hour).Unix()}, {Kind: "seven_day", Used: 0.1, ResetsAt: time.Now().Add(24 * time.Hour).Unix()}})
	}
	f := &nativeTeamTransport{base: http.DefaultTransport, premium: nativeTeamID(2)}
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = f.base })
	c := provider.ClaudeTeamConfig{Enabled: true, OrganizationID: nativeTeamOrg, SessionKey: "fixture-only", Threshold: 100, Members: []string{nativeTeamID(2), nativeTeamID(3)}}
	if err := provider.SaveClaudeTeam(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); provider.KeepClaudeTeam(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		c.Enabled = false
		if err := provider.SaveClaudeTeam(context.Background(), c); err != nil {
			t.Error(err)
		}
	})
	deadline := time.After(10 * time.Second)
	for !provider.ClaudeTeamStatus().Running || provider.ClaudeTeamStatus().Rotating {
		select {
		case <-deadline:
			t.Fatal("controller never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := provider.SetRouting("claude", provider.Ordered); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetAccountOrder("claude", []string{"a@example.com", "b@example.com"}); err != nil {
		t.Fatal(err)
	}
	restingUntil.Lock()
	restingUntil.m = map[string]time.Time{}
	restingUntil.Unlock()
	return f
}

func TestClaudeTeamNativeLimitRetainsRequest(t *testing.T) {
	for _, window := range []string{"five_hour", "seven_day"} {
		t.Run(window, func(t *testing.T) {
			f := nativeTeamFixture(t)
			bin := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
			testenv.Program(t, filepath.Join(bin, "claude"), `#!/bin/sh
case "$1" in auth) exit 1;; esac
while read -r line; do
  creds="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/.credentials.json"
  if grep -q tok-a "$creds"; then
    echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"`+window+`","utilization":1,"resetsAt":`+strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)+`}}'
    echo '{"type":"result","is_error":true,"result":"You have hit your `+window+` usage limit"}'
  else
    case "$line" in *retain-this-original-request*) ;; *) echo '{"type":"result","is_error":true,"result":"original request was lost"}'; continue;; esac
    echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"m","model":"claude-sonnet-5","usage":{"input_tokens":1}}}}'
    echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"retained-request-answered-by-B"}}}'
    echo '{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}}'
    echo '{"type":"stream_event","event":{"type":"message_stop"}}'
    echo '{"type":"result","is_error":false,"result":""}'
  fi
done
`)
			s := New()
			t.Cleanup(s.subscription.abortAll)
			h := httptest.NewServer(s.Handler())
			defer h.Close()
			body := `{"model":"claude/claude-sonnet-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"retain-this-original-request"}]}`
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", h.URL+"/v1/messages", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			if res.StatusCode != 200 || !strings.Contains(string(b), "retained-request-answered-by-B") || strings.Contains(string(b), "have hit") {
				t.Fatalf("request lost or refusal leaked: %d %s; status %+v", res.StatusCode, b, provider.ClaudeTeamStatus())
			}
			f.mu.Lock()
			writes := f.writes
			f.mu.Unlock()
			if writes != 2 {
				t.Fatalf("not one background handoff: %d writes", writes)
			}
		})
	}
}

func TestClaudeTeamNativeAdminOutageDoesNotGateRequests(t *testing.T) {
	f := nativeTeamFixture(t)
	f.mu.Lock()
	f.offline = true
	before := f.reads
	f.mu.Unlock()
	s := New()
	t.Cleanup(s.subscription.abortAll)
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	for range 2 {
		res, err := http.Post(h.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude/claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"still work"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !strings.Contains(string(b), "tok-a turn") {
			t.Fatalf("control plane gated a healthy reply: %d %s", res.StatusCode, b)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reads != before || f.writes != 0 {
		t.Fatalf("healthy inference touched the admin API: %d reads, %d writes", f.reads-before, f.writes)
	}
}

// Hold an actual admitted request open so rotation must drain it. The
// second HTTP request arrives with no enabled account, hears a comment
// before the seat is freed, then receives its answer in that same stream.
func TestClaudeTeamNativeNewRequestWaitsWithKeepalive(t *testing.T) {
	f := nativeTeamFixture(t)
	k, tick := keepHeldAfter, watchEvery
	keepHeldAfter, watchEvery = 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { keepHeldAfter, watchEvery = k, tick })
	release, ok := provider.TrackClaudeTeamRequest("a@example.com")
	if !ok {
		t.Fatal("active seat was not admitted")
	}
	var once sync.Once
	finish := func() { once.Do(release) }
	defer finish()
	if err := provider.RotateClaudeTeam(nativeTeamID(2)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		view := provider.ClaudeTeamStatus()
		if view.Rotating && view.InUse == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("account never began draining")
		case <-time.After(5 * time.Millisecond):
		}
	}
	s := New()
	t.Cleanup(s.subscription.abortAll)
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	defer finish() // release before the HTTP server/controller cleanup
	req, _ := http.NewRequestWithContext(ctx, "POST", h.URL+"/v1/messages", strings.NewReader(`{"model":"claude/claude-sonnet-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"waiting through handoff"}]}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, ":") {
		t.Fatalf("no pre-handoff keepalive: %q %v", line, err)
	}
	f.mu.Lock()
	writes := f.writes
	f.mu.Unlock()
	if writes != 0 {
		t.Fatalf("seat transferred before admitted work drained: %d", writes)
	}
	finish()
	b, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(b), "tok-b turn") || strings.Contains(string(b), `"type":"error"`) {
		t.Fatalf("waiting request not answered on its stream: %s %v", b, err)
	}
}

func TestClaudeTeamNativeAllExhaustedReturnsKnownReset(t *testing.T) {
	f := nativeTeamFixture(t)
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	for _, user := range []string{"b@example.com", "a@example.com"} {
		provider.NoteClaudeLimits(user, []provider.ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: reset.Unix()}})
	}
	s := New()
	t.Cleanup(s.subscription.abortAll)
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	bin := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	testenv.Program(t, filepath.Join(bin, "claude"), `#!/bin/sh
case "$1" in auth) exit 1;; esac
while read -r line; do
echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","utilization":1,"resetsAt":`+strconv.FormatInt(reset.Unix(), 10)+`}}'
echo '{"type":"result","is_error":true,"result":"You have hit your usage limit"}'
done
`)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", h.URL+"/v1/messages", strings.NewReader(`{"model":"claude/claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"all exhausted"}]}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 429 || !strings.Contains(string(b), reset.UTC().Format(time.RFC3339)) || res.Header.Get("Retry-After") == "" {
		t.Fatalf("no useful reset: %d %s", res.StatusCode, b)
	}
	provider.AwaitClaudeTeamRotation(context.Background())
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writes != 0 {
		t.Fatal("all-exhausted pool was rotated")
	}
}

func TestClaudeTeamNativeVisibleOutputIsNeverReplayed(t *testing.T) {
	for _, block := range []struct{ name, events, visible string }{
		{"text", `echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"already-visible-A"}}}'`, "already-visible-A"},
		{"tool", `echo '{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"write-once","name":"mcp__magpie__write","input":{}}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}}'`, "write-once"},
	} {
		t.Run(block.name, func(t *testing.T) {
			nativeTeamFixture(t)
			bin := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
			calls := filepath.Join(t.TempDir(), "inference-calls")
			testenv.Program(t, filepath.Join(bin, "claude"), `#!/bin/sh
case "$1" in auth) exit 1;; esac
while read -r line; do
echo called >> '`+calls+`'
echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"m","model":"claude-sonnet-5","usage":{"input_tokens":1}}}}'
`+block.events+`
echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","utilization":1,"resetsAt":`+strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)+`}}'
echo '{"type":"result","is_error":true,"result":"You have hit your usage limit"}'
done
`)
			s := New()
			t.Cleanup(s.subscription.abortAll)
			h := httptest.NewServer(s.Handler())
			defer h.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", h.URL+"/v1/messages", strings.NewReader(`{"model":"claude/claude-sonnet-5","max_tokens":100,"stream":true,"tools":[{"name":"write","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"write once"}]}`))
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if !strings.Contains(string(b), block.visible) {
				t.Fatalf("visible output lost: %d %s", res.StatusCode, b)
			}
			data, _ := os.ReadFile(calls)
			if strings.Count(string(data), "called") != 1 {
				t.Fatalf("visible answer was replayed: %s", data)
			}
		})
	}
}
