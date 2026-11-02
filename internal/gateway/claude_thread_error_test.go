package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// Desktop's bundled Code 2.1.304 classifies a 404 as recoverable only when
// error.details.error_code (or the message) says thread_not_found. It then
// creates a new Thread using its own complete history, not the delta on wire.
func TestClaudeThreadRecoverySignalThroughHTTP(t *testing.T) {
	for _, tc := range []struct {
		status        int
		code, message string
	}{
		{404, "thread_not_found", "No thread state was found for the requested `previous_message_id`. Replay the full conversation with `thread: {\"type\": \"create\"}` to start a new Thread."},
		{400, "thread_fingerprint_mismatch", "thread: differing field(s): cache_control, system, tools"},
		{400, "thread_already_continued", "This thread has already been continued"},
		{400, "thread_unsupported_request", "Unsupported thread request"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			s, p, _, _ := httpClaudeFixture(t)
			body := []byte(`{"model":"claude-opus-5-5","max_tokens":64,"thread":{"type":"continue","previous_message_id":"msg_anchor"},"messages":[{"role":"user","content":"delta, not complete history"}]}`)
			s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
				b, _ := io.ReadAll(r.Body)
				if gjson.GetBytes(b, "thread.previous_message_id").String() != "msg_anchor" || len(gjson.GetBytes(b, "messages").Array()) != 1 {
					t.Error("gateway fabricated a replay from delta history")
				}
				rec := httptest.NewRecorder()
				writeJSON(rec, tc.status, map[string]any{"type": "error", "error": map[string]any{"type": "not_found_error", "message": tc.message, "details": map[string]any{"error_code": tc.code, "private_debug": "do not copy arbitrary details"}}})
				return rec.Result(), nil
			})}
			out := httptest.NewRecorder()
			status, _ := s.serveClaudeHTTP(out, httpClaudeRequest(), provider.Anthropic, p, "claude-opus-5-5", body, &Usage{})
			if status != tc.status || gjson.Get(out.Body.String(), "error.details.error_code").String() != tc.code {
				t.Fatalf("lost native recovery signal: %d %s", status, out.Body)
			}
			if strings.Contains(out.Body.String(), "private_debug") {
				t.Fatal("copied unrelated upstream details")
			}
			// A keepalive may have committed HTTP 200: the final stream error must
			// still carry the same machine-readable recovery detail.
			stream := httptest.NewRecorder()
			h := newHoldWriter(stream, true)
			h.alive = &keptAlive{proto: provider.Anthropic, sent: true, streams: true}
			h.WriteHeader(tc.status)
			h.Write(out.Body.Bytes())
			h.release()
			if !bytes.Contains(stream.Body.Bytes(), []byte(tc.code)) {
				t.Fatalf("lost recovery signal after keepalive: %s", stream.Body)
			}
		})
	}
}

func TestClaudeThreadRecoveryDetailDoesNotInventCodes(t *testing.T) {
	for _, body := range []string{`{"error":{"message":"model missing","details":{"error_code":"model_not_found"}}}`, `{"error":{"message":"No thread state was found"}}`, `{"error":{"details":{"error_code":{"unexpected":true}}}}`} {
		w := httptest.NewRecorder()
		writeVendorError(w, provider.Anthropic, 404, "unchanged", []byte(body))
		if gjson.Get(w.Body.String(), "error.details").Exists() {
			t.Fatalf("invented thread code: %s", w.Body)
		}
	}
	// Other protocols retain their original shapes.
	w := httptest.NewRecorder()
	writeVendorError(w, provider.Chat, 404, "unchanged", []byte(`{"error":{"details":{"error_code":"thread_not_found"}}}`))
	if gjson.Get(w.Body.String(), "error.details").Exists() {
		t.Fatal("Anthropic detail leaked into another protocol")
	}
}

// The gateway cannot keep a Message Thread across account handoffs. Both
// native thread forms are refused before routing, then a complete stateless
// caller replay reaches the selected account without changing seats.
func TestClaudeThreadRecoveryThroughHandler(t *testing.T) {
	f := nativeTeamFixture(t)
	conf := settings.Load()
	conf.ClaudeTransport = "http"
	if err := settings.Save(conf); err != nil {
		t.Fatal(err)
	}
	oldAuth := claudeHTTPAuth
	claudeHTTPAuth = func(_ context.Context, a *provider.Account) (provider.ClaudeHTTPCredentials, error) {
		return provider.ClaudeHTTPCredentials{Token: a.User, DeviceID: "fixture-device", AccountUUID: a.User}, nil
	}
	t.Cleanup(func() { claudeHTTPAuth = oldAuth })
	s := New()
	t.Cleanup(s.subscription.abortAll)
	calls := 0
	s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		b, _ := io.ReadAll(r.Body)
		rec := httptest.NewRecorder()
		if r.Header.Get("Authorization") != "Bearer a@example.com" || gjson.GetBytes(b, "thread").Exists() || len(gjson.GetBytes(b, "messages").Array()) != 3 || gjson.GetBytes(b, "tools.0.name").String() != "Read" {
			t.Error("the stateless replay lost its history, tools or account")
		}
		writeJSON(rec, 200, map[string]any{"id": "msg_recovered", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "text", "text": "recovered"}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
		return rec.Result(), nil
	})}
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	request := func(body string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest("POST", h.URL+"/v1/messages", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	first := request(`{"model":"claude/claude-sonnet-5","stream":true,"max_tokens":64,"thread":{"type":"continue","previous_message_id":"msg_anchor"},"messages":[{"role":"user","content":"delta"}]}`)
	b, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != 400 || gjson.GetBytes(b, "error.details.error_code").String() != threadUnsupportedCode || calls != 0 {
		t.Fatalf("continuation reached a provider: status=%d calls=%d body=%s", first.StatusCode, calls, b)
	}
	second := request(`{"model":"claude/claude-sonnet-5","max_tokens":64,"thread":{"type":"create"},"system":"original instructions","tools":[{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"original"},{"role":"assistant","content":"prior reply"},{"role":"user","content":"delta"}]}`)
	b, _ = io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != 400 || gjson.GetBytes(b, "error.details.error_code").String() != threadUnsupportedCode || calls != 0 {
		t.Fatalf("thread creation reached a provider: status=%d calls=%d body=%s", second.StatusCode, calls, b)
	}
	// This is the caller's complete replay, with no thread to strand.
	third := request(`{"model":"claude/claude-sonnet-5","max_tokens":64,"system":"original instructions","tools":[{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"original"},{"role":"assistant","content":"prior reply"},{"role":"user","content":"delta"}]}`)
	b, _ = io.ReadAll(third.Body)
	third.Body.Close()
	if third.StatusCode != 200 || !bytes.Contains(b, []byte("recovered")) || calls != 1 {
		t.Fatalf("stateless replay failed: %d calls=%d %s", third.StatusCode, calls, b)
	}
	f.mu.Lock()
	writes := f.writes
	f.mu.Unlock()
	if writes != 0 {
		t.Fatalf("thread recovery moved a seat: %d", writes)
	}
}

func TestClaudeThreadExplicitReplayInstruction(t *testing.T) {
	body := mustJSON(map[string]any{"error": map[string]string{"type": "not_found_error", "message": "No thread state was found for the requested `previous_message_id`. Replay the full conversation with `thread: {\"type\": \"create\"}` to start a new Thread."}})
	w := httptest.NewRecorder()
	writeVendorError(w, provider.Anthropic, 404, "upstream replay instruction", body)
	if gjson.Get(w.Body.String(), "error.details.error_code").String() != "thread_not_found" || retryable(404, body) {
		t.Fatalf("lost explicit replay instruction: %s", w.Body)
	}
	if !retryable(404, []byte(`{"error":{"type":"not_found_error","message":"model not found"}}`)) {
		t.Fatal("ordinary model fallback was disabled")
	}
}

func TestClaudeThreadNativeContinuationKeepsInheritedFieldsOmitted(t *testing.T) {
	s, p, sent, head := httpClaudeFixture(t)
	body := []byte(`{"model":"claude-opus-5-5","max_tokens":64,"thread":{"type":"continue","previous_message_id":"msg_anchor"},"messages":[{"role":"user","content":"only the new turn"}]}`)
	r := httpClaudeRequest()
	r.Header.Set("User-Agent", "claude-cli/2.1.304 (external, cli)")
	r.Header.Set("x-app", "cli")
	r.Header.Set("x-claude-code-session-id", "00000000-0000-4000-8000-000000000001")
	r.Header.Set("anthropic-beta", "oauth-2025-04-20,interleaved-thinking-2025-05-14")
	out := httptest.NewRecorder()
	status, _ := s.serveClaudeHTTP(out, r, provider.Anthropic, p, "claude-opus-5-5", body, &Usage{})
	if status != 200 {
		t.Fatalf("continuation failed: %d %s", status, out.Body)
	}
	for _, field := range []string{"system", "tools", "cache_control", "diagnostics"} {
		if gjson.GetBytes(*sent, field).Exists() {
			t.Errorf("inherited %s was injected into native continuation", field)
		}
	}
	for _, field := range []string{"thread", "messages"} {
		if gjson.GetBytes(*sent, field).Raw != gjson.GetBytes(body, field).Raw {
			t.Errorf("changed native %s", field)
		}
	}
	if head.Get("User-Agent") != r.Header.Get("User-Agent") || head.Get("anthropic-beta") != r.Header.Get("anthropic-beta") {
		t.Errorf("native Thread fingerprint changed: user-agent=%s beta=%s", head.Get("User-Agent"), head.Get("anthropic-beta"))
	}
}
