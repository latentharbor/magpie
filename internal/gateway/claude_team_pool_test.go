package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

type poolAdmin struct {
	mu     sync.Mutex
	base   http.RoundTripper
	tiers  map[string]string
	writes int
}

func (p *poolAdmin) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "claude.ai" {
		if r.URL.Hostname() == "127.0.0.1" || r.URL.Hostname() == "localhost" || r.URL.Hostname() == "::1" {
			return p.base.RoundTrip(r)
		}
		return nil, errors.New("fixture blocked an unexpected outbound request")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Method != "GET" {
		p.writes++
		return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error"}}`)), Request: r}, nil
	}
	rows := []map[string]string{}
	for id, tier := range p.tiers {
		rows = append(rows, map[string]string{"uuid": id, "seat_tier": tier})
	}
	body, _ := json.Marshal(map[string]any{"members": rows, "has_more": false})
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
}

func nativePremiumPool(t *testing.T, n int) (*poolAdmin, provider.ClaudeTeamConfig, []string) {
	t.Helper()
	old := nativeTeamFixture(t)
	old.stop()
	teamFableLocalAllowance(t)
	file := filepath.Join(filepath.Dir(provider.Path()), "logins.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var ls []map[string]any
	if err = json.Unmarshal(raw, &ls); err != nil {
		t.Fatal(err)
	}
	users := []string{"a@example.com", "b@example.com"}
	for i := 2; i < max(n+1, 2); i++ {
		user := fmt.Sprintf("pool-%02d@example.test", i)
		users = append(users, user)
		ls = append(ls, map[string]any{"agent": "claude", "user": user, "plan": "max", "on": false, "profile": map[string]string{"emailAddress": user, "accountUuid": nativeTeamID(i + 2), "organizationUuid": nativeTeamOrg}, "auth": map[string]any{"claudeAiOauth": map[string]any{"accessToken": "fixture-token-" + user, "refreshToken": "fixture-refresh-" + user, "expiresAt": time.Now().Add(8 * time.Hour).UnixMilli()}}})
	}
	if err = os.WriteFile(file, mustJSON(ls), 0600); err != nil {
		t.Fatal(err)
	}
	provider.ForgetAccounts()
	p := &poolAdmin{base: http.DefaultTransport, tiers: map[string]string{}}
	c := provider.ClaudeTeamConfig{Enabled: true, OrganizationID: nativeTeamOrg, Threshold: 100}
	for i := range users {
		id := nativeTeamID(i + 2)
		c.Members = append(c.Members, id)
		p.tiers[id] = "unassigned"
		if i < n {
			p.tiers[id] = "team_tier_1"
		}
	}
	http.DefaultTransport = p
	t.Cleanup(func() { http.DefaultTransport = p.base })
	usage := provider.UsageClaudeVia(func(context.Context, string) (string, error) {
		return "Current session: 0% used\nCurrent week (all models): 0% used\nCurrent week (Fable): 0% used", nil
	})
	t.Cleanup(func() { provider.UsageClaudeVia(usage) })
	if err = provider.SaveClaudeTeam(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	cfg := settings.Load()
	cfg.ClaudeTransport = "http"
	if err = settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); provider.KeepClaudeTeam(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	auth := claudeHTTPAuth
	claudeHTTPAuth = func(_ context.Context, a *provider.Account) (provider.ClaudeHTTPCredentials, error) {
		return provider.ClaudeHTTPCredentials{Token: a.User, AccountUUID: a.User, DeviceID: "fixture-device"}, nil
	}
	t.Cleanup(func() { claudeHTTPAuth = auth })
	return p, c, users
}

func TestClaudeTeamDynamicPremiumPool(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 7} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			prefix := fmt.Sprintf("size-%d-", n)
			sessionFor := func(i int) string { return fmt.Sprintf("%spool-session-%d", prefix, i) }
			admin, c, users := nativePremiumPool(t, n)
			s := New()
			t.Cleanup(s.subscription.abortAll)
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			started := make(chan string, max(n, 1))
			var mu sync.Mutex
			assigned := map[string]string{}
			s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				content := gjson.GetBytes(body, "messages.0.content")
				session := content.String()
				if content.IsArray() {
					session = content.Get("0.text").String()
				}
				who := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				mu.Lock()
				assigned[session] = who
				mu.Unlock()
				found := false
				for _, block := range gjson.GetBytes(body, "system").Array() {
					if block.Get("text").String() == "cache-contract" {
						found = true
						if block.Get("cache_control.ttl").String() != "1h" {
							t.Error("changed caller's cache TTL")
						}
					}
				}
				if !found {
					t.Error("lost caller's cache-controlled system block")
				}
				select {
				case started <- who:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				out := `{"id":"pool-message","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"pool-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(out)), Request: r}, nil
			})}
			var serial atomic.Int64
			var completed sync.Map
			handler := s.Handler()
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ch, ok := completed.Load(r.Header.Get("X-Fixture-Request")); ok {
					defer close(ch.(chan struct{}))
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(h.Close)
			turns := map[string]int{}
			ask := func(session, model string) int {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				requestID := fmt.Sprint(serial.Add(1))
				done := make(chan struct{})
				completed.Store(requestID, done)
				defer completed.Delete(requestID)
				mu.Lock()
				turns[session]++
				turn := turns[session]
				mu.Unlock()
				messages := []map[string]string{{"role": "user", "content": session}}
				if turn > 1 {
					messages = append(messages, map[string]string{"role": "assistant", "content": "pool-ok"}, map[string]string{"role": "user", "content": fmt.Sprintf("continue-%d", turn)})
				}
				body := map[string]any{"model": "claude/" + model, "max_tokens": 100, "system": []map[string]any{{"type": "text", "text": "cache-contract", "cache_control": map[string]string{"type": "ephemeral", "ttl": "1h"}}}, "messages": messages}
				req, _ := http.NewRequestWithContext(ctx, "POST", h.URL+"/v1/messages", strings.NewReader(string(mustJSON(body))))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set(SessionHeader, session)
				req.Header.Set("X-Fixture-Request", requestID)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
					return 0
				}
				defer res.Body.Close()
				_, _ = io.Copy(io.Discard, res.Body)
				select {
				case <-done:
				case <-ctx.Done():
					t.Error("handler did not publish completion")
					return 0
				}
				return res.StatusCode
			}
			if n == 0 {
				releaseOnce.Do(func() { close(release) })
				if got := ask("empty-pool", "claude-opus-5-5"); got != 503 {
					t.Fatal("empty pool:", got)
				}
				return
			}
			results := make(chan int, n)
			for i := 0; i < n; i++ {
				go func(i int) { results <- ask(sessionFor(i), "claude-opus-5-5") }(i)
			}
			distinct := map[string]bool{}
			deadline := time.After(25 * time.Second)
			for range n {
				select {
				case u := <-started:
					distinct[u] = true
				case <-deadline:
					releaseOnce.Do(func() { close(release) })
					t.Fatal("requests did not enter concurrently")
				}
			}
			releaseOnce.Do(func() { close(release) })
			if len(distinct) != n {
				t.Fatalf("concurrent pool size %d used only %d Premium accounts: %v", n, len(distinct), distinct)
			}
			for range n {
				if got := <-results; got != 200 {
					t.Fatal("concurrent request:", got)
				}
			}
			for i := 0; i < n; i++ {
				session := sessionFor(i)
				mu.Lock()
				want := assigned[session]
				mu.Unlock()
				if want == "" {
					t.Fatal("transport did not observe the original conversation body", session)
				}
				if ask(session, "claude-opus-5-5") != 200 {
					t.Fatal("continuation failed")
				}
				mu.Lock()
				got := assigned[session]
				mu.Unlock()
				if got != want {
					t.Fatalf("cold/zero-cache session moved unnecessarily: %s -> %s", want, got)
				}
			}
			// Membership changes are observed, never a fixed three-seat setting.
			admin.mu.Lock()
			admin.tiers[nativeTeamID(n+2)] = "team_tier_1"
			admin.mu.Unlock()
			if err := provider.SaveClaudeTeam(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			grownSession := prefix + "grown-pool-session"
			if ask(grownSession, "claude-opus-5-5") != 200 {
				t.Fatal("new Premium not routable")
			}
			mu.Lock()
			grown := assigned[grownSession]
			mu.Unlock()
			if grown != users[n] {
				p, m, _ := provider.Resolve("claude/claude-opus-5-5")
				cs, plan := s.plan(p, m, provider.Anthropic)
				var choices []string
				for _, c := range cs {
					choices = append(choices, c.p.Account.User)
				}
				sticks.Lock()
				counts := map[string]int{}
				for k, st := range sticks.m {
					if strings.HasPrefix(k, p.ID+"/") {
						counts[st.who+"/"+st.model+"/"+st.effort]++
					}
				}
				sticks.Unlock()
				t.Fatalf("new Premium not added: %s want %s; affinity=%s routing=%s candidates=%v order=%+v bindings=%v", grown, users[n], p.Affinity, p.Routing, choices, plan.order, counts)
			}
			admin.mu.Lock()
			admin.tiers[nativeTeamID(2)] = "unassigned"
			admin.mu.Unlock()
			if err := provider.SaveClaudeTeam(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if ask(sessionFor(0), "claude-opus-5-5") != 200 {
				t.Fatal("removed seat stranded a session")
			}
			mu.Lock()
			removed := assigned[sessionFor(0)]
			mu.Unlock()
			if removed == users[0] {
				t.Fatal("removed Premium was still used")
			}
			if n > 1 {
				provider.NoteClaudeLimits(users[1], []provider.ClaudeLimit{{Kind: "seven_day_overage_included", Used: 1, ResetsAt: time.Now().Add(24 * time.Hour).Unix()}})
				for _, model := range []string{"claude-fable-5", "claude-fable-5-1", "claude-fable-5-2"} {
					session := prefix + "fable-" + model
					if ask(session, model) != 200 {
						t.Fatal("Fable request failed", model)
					}
					mu.Lock()
					u := assigned[session]
					mu.Unlock()
					if u == users[1] {
						t.Fatal("Fable exhausted account was selected", model)
					}
				}
			}
			admin.mu.Lock()
			writes := admin.writes
			admin.mu.Unlock()
			if writes != 0 {
				t.Fatalf("inference/session distribution changed cloud seats: %d", writes)
			}
		})
	}
}

func TestClaudeTeamSessionAllocationCountsRefinedDispatch(t *testing.T) {
	for _, n := range []int{1, 4, 10} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			fresh(t)
			id := "claude-team-" + nativeTeamOrg
			model := "claude-fable-5-2"
			scope := id + "/" + model
			var cs []candidate
			var plan planned
			for i := 0; i <= n; i++ {
				c := candidate{p: provider.Provider{ID: id, Account: &provider.Account{Agent: "claude", User: fmt.Sprintf("holder-%d@example.test", i)}}, model: model, rest: fmt.Sprintf("holder-%d", i)}
				cs = append(cs, c)
				plan.order = append(plan.order, Weighed{Known: true})
				if i < n {
					sticks.Lock()
					sticks.m[scope+fmt.Sprintf("|session-%d", i)] = stick{who: c.who(), model: model + "[1m]", effort: "high", at: time.Now()}
					sticks.Unlock()
				}
			}
			got, _ := leastHeld(scope, scope+"|new", cs, plan, true)
			if got[0].who() != cs[n].who() {
				t.Fatal("post-routing model/effort hid held conversations; new Premium stayed idle")
			}
		})
	}
}
