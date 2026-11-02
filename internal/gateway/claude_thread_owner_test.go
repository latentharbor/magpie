package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/gwbudget"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

type claudeTouchBudget struct{ calls int }

func (b *claudeTouchBudget) Reserve(access.Identity, int64, string, time.Time) (func(), gwbudget.Refusal) {
	b.calls++
	return func() {}, nil
}

func TestClaudeCacheTouchDoesNotReserveInferenceBudget(t *testing.T) {
	b := &claudeTouchBudget{}
	t.Cleanup(gwbudget.Limit(b))
	h := keyLimited(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for _, path := range []string{"/v1/messages/cache_touch", "/v1/messages"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"request_id":"req_fixture"}`))
		r = r.WithContext(access.WithIdentity(r.Context(), access.Identity{KeyID: "limited", Limit: &access.Limit{Period: "day", Tokens: 1}}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 204 || path == "/v1/messages/cache_touch" && b.calls != 0 {
			t.Fatal("cache-only request reserved inference budget")
		}
	}
	if b.calls != 1 {
		t.Fatal("inference budget was bypassed")
	}
}

func TestClaudeThreadAccountChangeRequiresFullReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		s, p, sent, _ := httpClaudeFixture(t)
		auth := provider.ClaudeHTTPCredentials{Token: "fixture-oauth", AccountUUID: "account-a", OrganizationUUID: "org-a", DeviceID: "device-local"}
		// Seed from a real accepted reply, including streaming message_start.
		body := `{"model":"claude-opus-5-5","max_tokens":64,"thread":{"type":"create"},"messages":[{"role":"user","content":"original"}]}`
		if stream {
			body = strings.Replace(body, `"max_tokens":64`, `"max_tokens":64,"stream":true`, 1)
		}
		w := httptest.NewRecorder()
		status, _ := s.serveClaudeHTTP(w, httpClaudeRequest(), provider.Anthropic, p, "claude-opus-5-5", []byte(body), &Usage{})
		if status != 200 {
			t.Fatalf("create failed: %d %s", status, w.Body)
		}
		delta := []byte(`{"model":"claude-opus-5-5","max_tokens":64,"thread":{"type":"continue","previous_message_id":"msg_fixture"},"messages":[{"role":"user","content":"delta"}]}`)
		// An eligible original owner continues unchanged.
		w = httptest.NewRecorder()
		status, _ = s.serveClaudeHTTP(w, httpClaudeRequest(), provider.Anthropic, p, "claude-opus-5-5", delta, &Usage{})
		if status != 200 || gjson.GetBytes(*sent, "thread.type").String() != "continue" {
			t.Fatal("same-owner continuation changed")
		}
		// The selected credential moved, even if the display account stayed.
		oldAuth := claudeHTTPAuth
		claudeHTTPAuth = func(context.Context, *provider.Account) (provider.ClaudeHTTPCredentials, error) {
			other := auth
			other.AccountUUID = "account-b"
			return other, nil
		}
		*sent = nil
		w = httptest.NewRecorder()
		status, _ = s.serveClaudeHTTP(w, httpClaudeRequest(), provider.Anthropic, p, "claude-opus-5-5", delta, &Usage{})
		if status != 404 || gjson.Get(w.Body.String(), "error.details.error_code").String() != "thread_not_found" || len(*sent) != 0 {
			t.Fatalf("delta sent to a different account: %d %s", status, w.Body)
		}
		// The caller's complete replay is admitted on the newly selected owner.
		w = httptest.NewRecorder()
		status, _ = s.serveClaudeHTTP(w, httpClaudeRequest(), provider.Anthropic, p, "claude-opus-5-5", []byte(body), &Usage{})
		claudeHTTPAuth = oldAuth
		if status != 200 || gjson.GetBytes(*sent, "thread.type").String() != "create" {
			t.Fatal("caller replay did not recover")
		}
	}
}

func TestClaudeCacheTouchThroughHandler(t *testing.T) {
	f := nativeTeamFixture(t)
	conf := settings.Load()
	conf.ClaudeTransport = "http"
	if err := settings.Save(conf); err != nil {
		t.Fatal(err)
	}
	oldAuth := claudeHTTPAuth
	claudeHTTPAuth = func(_ context.Context, a *provider.Account) (provider.ClaudeHTTPCredentials, error) {
		return provider.ClaudeHTTPCredentials{Token: a.User, AccountUUID: a.User, OrganizationUUID: nativeTeamOrg, DeviceID: "fixture"}, nil
	}
	t.Cleanup(func() { claudeHTTPAuth = oldAuth })
	s := New()
	t.Cleanup(s.subscription.abortAll)
	inferences, touches := 0, 0
	s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer a@example.com" || r.Header.Get("Cookie") != "" {
			t.Error("cache touch selected wrong credentials")
		}
		rec := httptest.NewRecorder()
		if r.URL.Path == "/v1/messages/cache_touch" {
			touches++
			if string(b) != `{"request_id":"req_fixture"}` || r.Header.Get("Anthropic-Beta") != "cache-keepalive-2026-09-03,oauth-2025-04-20" {
				t.Errorf("native touch changed: %s %v", b, r.Header)
			}
			rec.WriteHeader(204)
		} else {
			inferences++
			rec.Header().Set("Request-Id", "req_fixture")
			rec.Header().Set("Anthropic-Cache-Keepalive", "active")
			writeJSON(rec, 200, map[string]any{"id": "msg_owned", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "text", "text": "ok"}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
		}
		return rec.Result(), nil
	})}
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	post := func(path, body string) int {
		t.Helper()
		r, _ := http.NewRequest("POST", h.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Anthropic-Beta", "cache-keepalive-2026-09-03")
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		if path == "/v1/messages" && res.Header.Get("Anthropic-Cache-Keepalive") != "active" {
			t.Error("native cache activation header lost")
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	if status := post("/v1/messages", `{"model":"claude/claude-sonnet-5","max_tokens":64,"thread":{"type":"create"},"messages":[{"role":"user","content":"original"}]}`); status != 200 {
		t.Fatalf("create: %d", status)
	}
	if status := post("/v1/messages/cache_touch", `{"request_id":"req_fixture"}`); status != 204 {
		t.Fatalf("owned cache touch failed: %d", status)
	}
	for _, body := range []string{`{"request_id":"req_unknown"}`, `{"request_id":"msg_owned"}`} {
		if status := post("/v1/messages/cache_touch", body); status != 404 {
			t.Fatalf("unknown request inferred or impersonated: %d", status)
		}
	}
	// The same opaque ID cannot cross caller or current model/account scopes.
	for _, who := range []access.Identity{{KeyID: "other"}, {Models: []string{"unrelated/*"}}, {Accounts: []string{"claude/other"}}} {
		r := httptest.NewRequest("POST", "/v1/messages/cache_touch", strings.NewReader(`{"request_id":"req_fixture"}`))
		r = r.WithContext(access.WithIdentity(r.Context(), who))
		w := httptest.NewRecorder()
		s.claudeCacheTouch(w, r)
		if w.Code != 404 {
			t.Fatalf("touch escaped caller scope: %d", w.Code)
		}
	}
	if err := provider.SaveGroup(provider.Group{Name: "Touch grant", Members: []string{"claude/claude-sonnet-5"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/messages/cache_touch", strings.NewReader(`{"request_id":"req_fixture"}`))
	r.Header.Set("Anthropic-Beta", "cache-keepalive-2026-09-03")
	r = r.WithContext(access.WithIdentity(r.Context(), access.Identity{Models: []string{"group/touch-grant"}}))
	key := claudeAnchorKey(r, "request", "req_fixture")
	owner, known := s.claudeAnchors.Get(key, time.Now())
	if !known {
		t.Fatal("accepted request ownership missing")
	}
	owner.grant = "group/touch-grant"
	s.claudeAnchors.Put(key, owner, time.Now())
	w := httptest.NewRecorder()
	s.claudeCacheTouch(w, r)
	if w.Code != 204 {
		t.Fatalf("current group grant refused: %d %s", w.Code, w.Body)
	}
	if err := provider.SaveGroup(provider.Group{Name: "Touch grant", Members: []string{"claude/claude-haiku-5"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	r.Body = io.NopCloser(strings.NewReader(`{"request_id":"req_fixture"}`))
	w = httptest.NewRecorder()
	s.claudeCacheTouch(w, r)
	if w.Code != 404 {
		t.Fatal("removed group member still authorized")
	}
	if inferences != 1 || touches != 2 {
		t.Fatalf("cache touch became inference/retry: inference=%d touch=%d", inferences, touches)
	}
	f.mu.Lock()
	writes := f.writes
	f.mu.Unlock()
	if writes != 0 {
		t.Fatal("cache touch transferred a seat")
	}
}

func TestClaudeThreadOwnersSeparateBranchesAndCallers(t *testing.T) {
	s := &Server{}
	r := httpClaudeRequest()
	a := claudeThreadOwner{account: "a"}
	b := claudeThreadOwner{account: "b"}
	s.rememberClaudeAnchors(r, a, &Usage{ResponseID: "msg_main"})
	s.rememberClaudeAnchors(r, b, &Usage{ResponseID: "msg_subagent"})
	for id, owner := range map[string]claudeThreadOwner{"msg_main": a, "msg_subagent": b} {
		body := []byte(`{"thread":{"type":"continue","previous_message_id":"` + id + `"}}`)
		if s.claudeThreadMoved(r, body, owner) {
			t.Fatal("subagent replaced parent's owner")
		}
		other := r.WithContext(access.WithIdentity(r.Context(), access.Identity{KeyID: "other"}))
		if s.claudeThreadMoved(other, body, claudeThreadOwner{}) {
			t.Fatal("another caller learned the anchor")
		}
		s.claudeAnchors.Put(claudeAnchorKey(r, "message", id), owner, time.Now().Add(-25*time.Hour))
		if s.claudeThreadMoved(r, body, claudeThreadOwner{}) {
			t.Fatal("expired local metadata still enforced")
		}
	}
}

func TestClaudeCacheTouchPreservesFailureAndCancellation(t *testing.T) {
	for _, status := range []int{204, 401, 404, 429, 500} {
		s, p, _, _ := httpClaudeFixture(t)
		auth, _ := claudeHTTPAuth(context.Background(), p.Account)
		owner := claudeOwner(p, "claude-opus-5-5", auth)
		calls := 0
		s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			if c, ok := r.Context().Value(claudeHTTPKey{}).(claudeHTTPCall); !ok || c.auth.AccountUUID != owner.account {
				t.Error("cache touch did not use account-partitioned native TLS path")
			}
			rec := httptest.NewRecorder()
			rec.Header().Set("Retry-After", "30")
			rec.Header().Set("Set-Cookie", "must-not-copy")
			rec.WriteHeader(status)
			return rec.Result(), nil
		})}
		w := httptest.NewRecorder()
		s.touchClaudeOwner(w, httpClaudeRequest(), []byte(`{"request_id":"req_fixture"}`), "request", owner, p)
		if w.Code != status || calls != 1 || w.Header().Get("Retry-After") != "30" || w.Header().Get("Set-Cookie") != "" {
			t.Fatalf("touch failure retried, hidden or changed: %d calls=%d", w.Code, calls)
		}
		_, known := s.claudeAnchors.Get("request", time.Now())
		if known != (status == 204) {
			t.Fatal("failed touch renewed the local record")
		}
		owner.account = "previous-credential"
		w = httptest.NewRecorder()
		s.touchClaudeOwner(w, httpClaudeRequest(), []byte(`{"request_id":"req_fixture"}`), "request", owner, p)
		if w.Code != 404 || calls != 1 {
			t.Fatal("cache touch reused a changed account identity")
		}
	}
	s, p, _, _ := httpClaudeFixture(t)
	auth, _ := claudeHTTPAuth(context.Background(), p.Account)
	ctx, cancel := context.WithCancel(context.Background())
	s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	w := httptest.NewRecorder()
	s.touchClaudeOwner(w, httpClaudeRequest().WithContext(ctx), []byte(`{"request_id":"req_fixture"}`), "request", claudeOwner(p, "claude-opus-5-5", auth), p)
	if w.Code != 502 || ctx.Err() != context.Canceled {
		t.Fatal("cancelled touch did not end")
	}
}
