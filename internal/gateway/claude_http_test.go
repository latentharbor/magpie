package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

type claudeHTTPRoundTrip func(*http.Request) (*http.Response, error)

func (f claudeHTTPRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func httpClaudeFixture(t *testing.T) (*Server, provider.Provider, *[]byte, *http.Header) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := claudeHTTPAuth
	claudeHTTPAuth = func(context.Context, *provider.Account) (provider.ClaudeHTTPCredentials, error) {
		return provider.ClaudeHTTPCredentials{Token: "fixture-oauth", AccountUUID: "account-a", OrganizationUUID: "org-a", DeviceID: "device-local"}, nil
	}
	t.Cleanup(func() { claudeHTTPAuth = old })
	var sent []byte
	var head http.Header
	s := New()
	t.Cleanup(s.subscription.abortAll)
	s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.anthropic.com/v1/messages?beta=true" {
			t.Fatalf("wrong OAuth destination: %s", r.URL)
		}
		sent, _ = io.ReadAll(r.Body)
		head = r.Header.Clone()
		reply := sse(`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":8,"output_tokens":0}}}`,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"HTTP answered"}}`,
			`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
			`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(reply)), Request: r}, nil
	})}
	p := provider.Provider{ID: "claude-http-fixture", Name: "Claude", Anthropic: "http://must-not-receive-oauth.invalid", Account: &provider.Account{Agent: "claude", User: "http-fixture@example.com"}}
	return s, p, &sent, &head
}

func httpClaudeRequest() *http.Request {
	r := httptest.NewRequest("POST", "http://gateway/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer must-not-forward")
	r.Header.Set("X-Session-Id", "fixture-session")
	return r.WithContext(context.WithValue(r.Context(), claudeTransportKey{}, "http"))
}

func TestClaudeHTTPNativeAndTranslated(t *testing.T) {
	for _, from := range []provider.Protocol{provider.Anthropic, provider.Chat, provider.Responses} {
		for _, stream := range []bool{false, true} {
			t.Run(string(from)+"/"+map[bool]string{false: "whole", true: "stream"}[stream], func(t *testing.T) {
				s, p, sent, head := httpClaudeFixture(t)
				body := `{"model":"claude-opus-5-5","max_tokens":1234,"stream":false,"system":"Keep my actual instructions.","thinking":{"type":"adaptive","display":"updates"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"Reply OK."}]}`
				switch from {
				case provider.Chat:
					body = `{"model":"claude-opus-5-5","max_tokens":1234,"stream":false,"reasoning_effort":"max","messages":[{"role":"system","content":"Keep my actual instructions."},{"role":"user","content":"Reply OK."}]}`
				case provider.Responses:
					body = `{"model":"claude-opus-5-5","max_output_tokens":1234,"stream":false,"reasoning":{"effort":"max"},"instructions":"Keep my actual instructions.","input":"Reply OK."}`
				}
				if stream {
					body = strings.Replace(body, `"stream":false`, `"stream":true`, 1)
				}
				w := httptest.NewRecorder()
				var call Call
				status, msg := s.attempt(w, httpClaudeRequest(), from, p, "claude-opus-5-5", []byte(body), &call)
				if status != 200 || !strings.Contains(w.Body.String(), "HTTP answered") {
					t.Fatalf("HTTP failed %d %s: %s", status, msg, w.Body.String())
				}
				if len(*sent) == 0 || head.Get("Authorization") != "Bearer fixture-oauth" || head.Get("X-Api-Key") != "" {
					t.Fatal("not the selected OAuth HTTP path")
				}
				if !strings.Contains(string(*sent), "Keep my actual instructions.") || gjson.GetBytes(*sent, "output_config.effort").String() != "max" {
					t.Fatalf("lost prompt/effort: %s", *sent)
				}
				if from == provider.Anthropic && gjson.GetBytes(*sent, "max_tokens").Int() != 1234 {
					t.Fatal("changed explicit max_tokens")
				}
				var identity map[string]string
				if json.Unmarshal([]byte(gjson.GetBytes(*sent, "metadata.user_id").String()), &identity) != nil || identity["account_uuid"] != "account-a" || identity["session_id"] != head.Get("x-claude-code-session-id") {
					t.Fatal("HTTP identity/header drift")
				}
				if head.Get("User-Agent") != "claude-cli/2.1.310 (external, cli)" || head.Get("x-claude-code-prompt-id") == "" {
					t.Fatal("missing native application profile")
				}
			})
		}
	}
}

func TestClaudeHTTPPreservesNativeProtocol(t *testing.T) {
	c := claudeHTTPCall{auth: provider.ClaudeHTTPCredentials{Token: "test", DeviceID: "device", AccountUUID: "acct"}, session: "session", prompt: "prompt", turn: 1}
	body := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":[{"type":"text","text":"Reply OK.","cache_control":{"type":"ephemeral","ttl":"5m"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"signed","signature":"keep-signature"},{"type":"tool_use","id":"toolu_123","name":"mcp__my_tool","input":{"file":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"result"}]}],"system":[{"type":"text","text":"Client instructions","cache_control":{"type":"ephemeral","ttl":"1h"}}],"tools":[{"name":"mcp__my_tool","description":"Keep","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"},"thinking":{"type":"adaptive","display":"updates"},"output_config":{"effort":"xhigh"}}`)
	got, err := claudeHTTPBody(body, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"messages", "tools", "tool_choice", "thinking", "output_config"} {
		if gjson.GetBytes(got, field).Raw != gjson.GetBytes(body, field).Raw {
			t.Fatalf("changed native %s", field)
		}
	}
	if gjson.GetBytes(got, "system.2.text").String() != "Client instructions" || gjson.GetBytes(got, "system.2.cache_control.ttl").String() != "1h" {
		t.Fatal("lost native system/cache marks")
	}
	signed, err := signAnthropicMessagesBody(got)
	if err != nil || string(signed) != string(got) {
		t.Fatal("CCH is not signed from the exact final body")
	}
	if strings.Contains(gjson.GetBytes(got, "system.0.text").String(), "cch=00000") {
		t.Fatal("CCH placeholder escaped")
	}
	req := httptest.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Header.Set("x-api-key", "caller")
	claudeHTTPHeaders(req, got, c)
	for _, beta := range []string{"oauth-2025-04-20", "effort-2025-11-24", "thinking-display-updates-2026-08-18", "extended-cache-ttl-2025-04-11", "cache-diagnosis-2026-04-07"} {
		if !strings.Contains(req.Header.Get("anthropic-beta"), beta) {
			t.Fatalf("missing %s", beta)
		}
	}
	if !strings.Contains(gjson.GetBytes(got, "system.0.text").String(), "2.1.310.efb;") {
		t.Fatal("fingerprint differs from the observed local native CLI")
	}
}

func TestClaudeHTTPTransportSnapshot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	original := withClaudeTransport(httptest.NewRequest("POST", "/", nil))
	if usesClaudeHTTP(original.Context()) {
		t.Fatal("default is not real CLI")
	}
	conf := settings.Load()
	conf.ClaudeTransport = "http"
	if err := settings.Save(conf); err != nil {
		t.Fatal(err)
	}
	next := withClaudeTransport(httptest.NewRequest("POST", "/", nil))
	conf.ClaudeTransport = "cli"
	if err := settings.Save(conf); err != nil {
		t.Fatal(err)
	}
	if usesClaudeHTTP(original.Context()) || !usesClaudeHTTP(next.Context()) || usesClaudeHTTP(withClaudeTransport(httptest.NewRequest("POST", "/", nil)).Context()) {
		t.Fatal("mode changed during an admitted request")
	}
}

func TestClaudeHTTPFailureDoesNotStartCLI(t *testing.T) {
	s, p, _, _ := httpClaudeFixture(t)
	claudeHTTPAuth = func(context.Context, *provider.Account) (provider.ClaudeHTTPCredentials, error) {
		return provider.ClaudeHTTPCredentials{}, errors.New("refresh this account")
	}
	w := httptest.NewRecorder()
	var call Call
	status, _ := s.attempt(w, httpClaudeRequest(), provider.Anthropic, p, "claude-opus-5-5", []byte(`{"messages":[]}`), &call)
	if status != 503 || !strings.Contains(w.Body.String(), "refresh this account") {
		t.Fatalf("unexpected failure %d %s", status, w.Body.String())
	}
}

func TestClaudeHTTPHeaderLimits(t *testing.T) {
	user := "http-limit@example.com"
	reset := time.Now().Add(time.Hour).Unix()
	r := &http.Response{StatusCode: 429, Header: http.Header{}}
	r.Header.Set("anthropic-ratelimit-unified-5h-utilization", "0.97")
	// Decimal epoch as emitted by the API, not a local wall-clock guess.
	r.Header.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(reset, 10))
	r.Header.Set("anthropic-ratelimit-unified-status", "rejected")
	r.Header.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	r.Header.Set("anthropic-ratelimit-unified-7d-utilization", "NaN")
	heardClaudeHTTPLimits(provider.Provider{Account: &provider.Account{Agent: "claude", User: user}}, r)
	ls := provider.KeptClaudeLimits(user)
	if len(ls) != 1 || ls[0].Kind != "five_hour" || ls[0].Used != 1 || ls[0].ResetsAt != reset {
		t.Fatalf("wrong quota feedback: %+v", ls)
	}
}

func TestClaudeHTTPTeamLimitRetainsRequest(t *testing.T) {
	f := nativeTeamFixture(t)
	conf := settings.Load()
	conf.ClaudeTransport = "http"
	if err := settings.Save(conf); err != nil {
		t.Fatal(err)
	}
	old := claudeHTTPAuth
	claudeHTTPAuth = func(_ context.Context, a *provider.Account) (provider.ClaudeHTTPCredentials, error) {
		return provider.ClaudeHTTPCredentials{Token: a.User, DeviceID: "fixture-device", AccountUUID: a.User}, nil
	}
	t.Cleanup(func() { claudeHTTPAuth = old })
	s := New()
	t.Cleanup(s.subscription.abortAll)
	calls := 0
	s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "retain-http-request") {
			t.Error("lost original request during handoff")
		}
		h := http.Header{"Content-Type": {"application/json"}}
		if r.Header.Get("Authorization") == "Bearer a@example.com" {
			h.Set("anthropic-ratelimit-unified-5h-utilization", "0.97")
			h.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			h.Set("anthropic-ratelimit-unified-status", "rejected")
			h.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
			return &http.Response{StatusCode: 429, Status: "429 Too Many Requests", Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"You have hit your session usage limit"}}`)), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"id":"msg_b","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"retained-http-by-B"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)), Request: r}, nil
	})}
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", h.URL+"/v1/messages", strings.NewReader(`{"model":"claude/claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"retain-http-request"}]}`))
	r.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(b), "retained-http-by-B") || calls != 2 {
		t.Fatalf("HTTP handoff failed: %d %s calls=%d", res.StatusCode, b, calls)
	}
	f.mu.Lock()
	writes := f.writes
	f.mu.Unlock()
	if writes != 2 {
		t.Fatalf("not one handoff: writes=%d", writes)
	}
	if d := lastUsage(t).ClaudeHTTP; d == nil || d.CacheSource != "generated" || len(d.Prefixes) == 0 {
		t.Fatalf("HTTP handoff lost its ledger diagnostic: %+v", d)
	}
}

func TestClaudeHTTPCancelDoesNotReplay(t *testing.T) {
	s, p, _, _ := httpClaudeFixture(t)
	calls := 0
	s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) { calls++; return nil, r.Context().Err() })}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httpClaudeRequest().WithContext(context.WithValue(ctx, claudeTransportKey{}, "http"))
	w := httptest.NewRecorder()
	var c Call
	status, _ := s.attempt(w, r, provider.Anthropic, p, "claude-opus-5-5", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), &c)
	if status < 400 || calls > 1 {
		t.Fatalf("cancelled HTTP replayed: %d calls=%d", status, calls)
	}
}

// Independently recorded compatibility vectors from CLIProxyAPI (MIT notice
// in third_party/cliproxyapi-LICENSE.txt), not this implementation's own hash.
func TestClaudeHTTPCCHKnownVector(t *testing.T) {
	base := `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.test; cc_entrypoint=sdk-cli; cch=00000;"},{"type":"text","text":"system-x"}],"tools":[],"metadata":{"user_id":"meta-x"},"max_tokens":1,"thinking":{"type":"adaptive","display":"omitted"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"output_config":{"effort":"high"},"stream":true}`
	for _, tt := range []struct{ body, want string }{{base, "7ee87"}, {strings.Replace(base, `"max_tokens":1`, `"max_tokens":2`, 1), "7ee87"}, {strings.Replace(base, `"text":"x"`, `"text":"y"`, 1), "b9cc8"}} {
		b, err := signAnthropicMessagesBody([]byte(tt.body))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(gjson.GetBytes(b, "system.0.text").String(), "cch="+tt.want+";") {
			t.Fatalf("CCH differs from independent vector: %s", b)
		}
	}
}

// Captures use fake tokens and a loopback-only mock. Never load live CLI
// credentials; an optional extra capture is admitted in the same format.
func claudeHTTPCapturePaths() []string {
	paths := []string{"testdata/claude-310-cli-request.json", "testdata/claude-310-sdk-request.json", "testdata/claude-308-desktop-entrypoint-request.json"}
	if path := os.Getenv("MAGPIE_CLAUDE_HTTP_CAPTURE"); path != "" {
		paths = append(paths, path)
	}
	return paths
}

func TestClaudeHTTPNativeCCHCapture(t *testing.T) {
	for _, path := range claudeHTTPCapturePaths() {
		t.Run(path, func(t *testing.T) { checkClaudeHTTPNativeCCHCapture(t, path) })
	}
}

func checkClaudeHTTPNativeCCHCapture(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var captures []struct {
		URL  string          `json:"url"`
		Body json.RawMessage `json:"body"`
	}
	if err = json.Unmarshal(b, &captures); err != nil {
		t.Fatal(err)
	}
	checked := false
	for _, r := range captures {
		if !strings.Contains(r.URL, "api.anthropic.com/v1/messages") {
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, r.Body); err != nil {
			t.Fatal(err)
		}
		signed, err := signAnthropicMessagesBody(compact.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(signed, "system.0.text").String() != gjson.GetBytes(r.Body, "system.0.text").String() {
			t.Fatalf("native CCH mismatch: %s", gjson.GetBytes(signed, "system.0.text").String())
		}
		checked = true
	}
	if !checked {
		t.Fatal("no native first-party-host request in capture")
	}
}

// Run the real CLI's fake-token capture through request admission and the
// ordinary upstream builder, not only the body helper.
func TestClaudeHTTPNativeCaptureThroughGateway(t *testing.T) {
	for _, path := range claudeHTTPCapturePaths() {
		t.Run(path, func(t *testing.T) { checkClaudeHTTPNativeCaptureThroughGateway(t, path) })
	}
}

func checkClaudeHTTPNativeCaptureThroughGateway(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var captures []struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	}
	if err = json.Unmarshal(b, &captures); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, capture := range captures {
		if !strings.Contains(capture.URL, "/v1/messages") {
			continue
		}
		s, p, sent, head := httpClaudeFixture(t)
		r := httpClaudeRequest()
		for k, v := range capture.Headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		var call Call
		status, msg := s.attempt(w, r, provider.Anthropic, p, gjson.GetBytes(capture.Body, "model").String(), capture.Body, &call)
		if status != 200 {
			t.Fatalf("native capture failed: %d %s", status, msg)
		}
		for _, field := range []string{"messages", "tools", "thinking", "output_config", "context_management", "diagnostics"} {
			if gjson.GetBytes(*sent, field).Raw != gjson.GetBytes(capture.Body, field).Raw {
				t.Errorf("changed captured native %s", field)
			}
		}
		before, after := gjson.GetBytes(capture.Body, "system").Array(), gjson.GetBytes(*sent, "system").Array()
		if len(before) != len(after) {
			t.Fatal("changed captured native system length")
		}
		for i := 1; i < len(before); i++ {
			if before[i].Raw != after[i].Raw {
				t.Errorf("changed captured native system block %d", i)
			}
		}
		if head.Get("x-claude-code-session-id") != r.Header.Get("x-claude-code-session-id") {
			t.Error("changed captured native session")
		}
		for _, field := range []string{"User-Agent", "Accept", "Accept-Encoding", "x-client-request-id", "x-stainless-package-version", "x-stainless-runtime-version"} {
			if head.Get(field) != r.Header.Get(field) {
				t.Errorf("changed captured native %s: %q", field, head.Get(field))
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("capture contained no native inference request")
	}
}
