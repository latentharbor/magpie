package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestClaudeHTTPAuthHandoffRetainsRequest(t *testing.T) {
	for _, kind := range []string{"cleared-store", "upstream-revoked"} {
		t.Run(kind, func(t *testing.T) {
			f := nativeTeamFixture(t)
			cfg := settings.Load()
			cfg.ClaudeTransport = "http"
			if err := settings.Save(cfg); err != nil {
				t.Fatal(err)
			}
			oldAuth := claudeHTTPAuth
			claudeHTTPAuth = func(_ context.Context, a *provider.Account) (provider.ClaudeHTTPCredentials, error) {
				version := provider.ClaudeLoginVersion(a.User)
				if kind == "cleared-store" && a.User == "a@example.com" {
					provider.NoteClaudeSignInFailure(a.User, version, "OAuth token revoked")
					return provider.ClaudeHTTPCredentials{}, errors.New("Claude Code could not authenticate this account; sign in again in magpie")
				}
				return provider.ClaudeHTTPCredentials{Token: a.User, AccountUUID: a.User, DeviceID: "fixture-device", LoginVersion: version}, nil
			}
			t.Cleanup(func() { claudeHTTPAuth = oldAuth })
			var calls atomic.Int32
			s := New()
			t.Cleanup(s.subscription.abortAll)
			s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				if !strings.Contains(string(body), "retain-auth-failed-request") {
					t.Error("handoff lost original request")
				}
				status := 200
				reply := `{"id":"msg_auth","type":"message","role":"assistant","content":[{"type":"text","text":"replacement-answered"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				if r.Header.Get("Authorization") == "Bearer a@example.com" {
					status = 401
					reply = `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked."}}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(reply)), Request: r}, nil
			})}
			h := httptest.NewServer(s.Handler())
			defer h.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			r, _ := http.NewRequestWithContext(ctx, "POST", h.URL+"/v1/messages", strings.NewReader(`{"model":"claude/claude-sonnet-5","max_tokens":128,"messages":[{"role":"user","content":"retain-auth-failed-request"}]}`))
			r.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			wantCalls := int32(2)
			if kind == "cleared-store" {
				wantCalls = 1 // failed before any upstream inference
			}
			if err != nil || res.StatusCode != 200 || !strings.Contains(string(body), "replacement-answered") || calls.Load() != wantCalls {
				t.Fatalf("auth handoff failed: %d calls=%d %s %v", res.StatusCode, calls.Load(), body, err)
			}
			f.mu.Lock()
			writes := f.writes
			f.mu.Unlock()
			if writes != 2 {
				t.Fatalf("authentication failure did not transfer exactly one seat: %d writes", writes)
			}
			for _, l := range provider.Logins("claude") {
				if l.User == "a@example.com" && l.Lapsed == "" {
					t.Fatal("seat transfer cleared genuine credential refusal")
				}
			}
		})
	}
}

func TestClaudeHTTPVisibleOutputIsNeverReplayed(t *testing.T) {
	for _, tc := range []struct{ name, event, visible string }{
		{"text", `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"already-visible"}}
`, "already-visible"},
		{"tool", `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"write-once","name":"Write","input":{}}}
`, "write-once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nativeTeamFixture(t)
			cfg := settings.Load()
			cfg.ClaudeTransport = "http"
			if err := settings.Save(cfg); err != nil {
				t.Fatal(err)
			}
			oldAuth := claudeHTTPAuth
			claudeHTTPAuth = func(_ context.Context, a *provider.Account) (provider.ClaudeHTTPCredentials, error) {
				return provider.ClaudeHTTPCredentials{Token: a.User, DeviceID: "fixture-device", AccountUUID: a.User}, nil
			}
			t.Cleanup(func() { claudeHTTPAuth = oldAuth })
			var calls atomic.Int32
			s := New()
			t.Cleanup(s.subscription.abortAll)
			s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				reply := sse(
					"event: message_start\n"+`data: {"type":"message_start","message":{"id":"msg_visible","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1}}}`,
					tc.event,
					"event: error\n"+`data: {"type":"error","error":{"type":"rate_limit_error","message":"You have hit your session usage limit"}}`)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(reply)), Request: r}, nil
			})}
			h := httptest.NewServer(s.Handler())
			defer h.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			r, err := http.NewRequestWithContext(ctx, "POST", h.URL+"/v1/messages", strings.NewReader(`{"model":"claude/claude-sonnet-5","max_tokens":128,"stream":true,"tools":[{"name":"Write","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"write once"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), tc.visible) || !strings.Contains(string(body), "rate_limit_error") || calls.Load() != 1 {
				t.Fatalf("visible HTTP response lost or replayed: calls=%d body=%s", calls.Load(), body)
			}
		})
	}
}

func TestClaudeHTTPNativeHandoffPreservesCacheAndEffort(t *testing.T) {
	nativeTeamFixture(t)
	cfg := settings.Load()
	cfg.ClaudeTransport = "http"
	if err := settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	oldAuth := claudeHTTPAuth
	claudeHTTPAuth = func(_ context.Context, a *provider.Account) (provider.ClaudeHTTPCredentials, error) {
		return provider.ClaudeHTTPCredentials{Token: a.User, AccountUUID: a.User, DeviceID: "fixture-device"}, nil
	}
	t.Cleanup(func() { claudeHTTPAuth = oldAuth })
	body := `{"model":"claude/claude-sonnet-5","max_tokens":128000,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.310; cc_entrypoint=claude-desktop; cch=00000;"},{"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK.","cache_control":{"type":"ephemeral","ttl":"1h"}}],"thinking":{"type":"adaptive","display":"updates"},"output_config":{"effort":"max"},"tools":[{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"original"},{"role":"assistant","content":[{"type":"thinking","thinking":"opaque","signature":"signed-fixture"},{"type":"tool_use","id":"toolu_handoff","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_handoff","content":"result","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`
	var calls atomic.Int32
	s := New()
	t.Cleanup(s.subscription.abortAll)
	s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		got, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		for _, field := range []string{"messages", "tools", "thinking", "output_config", "max_tokens", "system.1"} {
			if gjson.GetBytes(got, field).Raw != gjson.Get(body, field).Raw {
				t.Errorf("handoff changed native %s", field)
			}
		}
		h := http.Header{"Content-Type": {"application/json"}}
		reply := `{"id":"msg_handoff","type":"message","role":"assistant","content":[{"type":"text","text":"handoff-complete"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		status := 200
		if r.Header.Get("Authorization") == "Bearer a@example.com" {
			status = 429
			h.Set("anthropic-ratelimit-unified-5h-status", "rejected")
			reply = `{"type":"error","error":{"type":"rate_limit_error","message":"You have hit your session usage limit"}}`
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(reply)), Request: r}, nil
	})}
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "POST", h.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "claude-cli/2.1.310 (external, claude-desktop)")
	r.Header.Set("x-app", "cli")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 || !strings.Contains(string(reply), "handoff-complete") || calls.Load() != 2 {
		t.Fatalf("native handoff failed: %d calls=%d %s %v", res.StatusCode, calls.Load(), reply, err)
	}
}
