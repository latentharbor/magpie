package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
)

func TestClaudeHTTPNativeCallerKeepsItsProtocol(t *testing.T) {
	s, p, sent, head := httpClaudeFixture(t)
	r := httpClaudeRequest()
	for k, value := range map[string]string{
		"User-Agent": "claude-cli/2.1.309 (external, cli)", "x-app": "cli",
		"Accept-Encoding":             "gzip",
		"x-claude-code-session-id":    "c5779c78-6a35-42c8-a6ec-13fb67a140f2",
		"x-claude-code-prompt-id":     "78992615-9a72-44e0-bccb-be6a55f90646",
		"x-claude-code-request-class": "subagent", "x-claude-code-agent-id": "native-child",
		"x-stainless-runtime-version": "v26.3.0", "x-stainless-retry-count": "2",
		"anthropic-beta":            "claude-code-20250219,extended-cache-ttl-2025-04-11",
		"anthropic-organization-id": "caller-organization",
	} {
		r.Header.Set(k, value)
	}
	body := []byte(`{"model":"claude-opus-5-5","max_tokens":512,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.309.c22; cc_entrypoint=cli; cch=00000; cc_prompt_id=78992615-9a72-44e0-bccb-be6a55f90646; cc_turn_origin=tool; cc_prompt_index=9; cc_turn_index=3;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}},{"type":"text","text":"Keep the native harness.","cache_control":{"type":"ephemeral","ttl":"5m"}}],"metadata":{"user_id":"{\"device_id\":\"caller-device\",\"account_uuid\":\"caller-account\",\"session_id\":\"c5779c78-6a35-42c8-a6ec-13fb67a140f2\",\"future_field\":true}","other":"keep"},"diagnostics":{"previous_message_id":"msg_previous"},"messages":[{"role":"user","content":"Reply OK."}]}`)
	w := httptest.NewRecorder()
	var call Call
	status, msg := s.attempt(w, r, provider.Anthropic, p, "claude-opus-5-5", body, &call)
	if status != 200 {
		t.Fatalf("native call failed: %d %s", status, msg)
	}
	for k := range r.Header {
		if claudeCodeHeader(k) || strings.EqualFold(k, "x-client-request-id") || strings.EqualFold(k, "Accept-Encoding") {
			if strings.EqualFold(k, "anthropic-beta") || strings.EqualFold(k, "anthropic-organization-id") {
				continue
			}
			if head.Get(k) != r.Header.Get(k) {
				t.Errorf("rewrote native %s: %q", k, head.Get(k))
			}
		}
	}
	for _, field := range []string{"system.1", "system.2", "messages", "diagnostics", "metadata.other"} {
		if gjson.GetBytes(*sent, field).Raw != gjson.GetBytes(body, field).Raw {
			t.Errorf("rewrote native %s", field)
		}
	}
	if gjson.GetBytes(*sent, "tools").Exists() {
		t.Error("inserted tools into native request")
	}
	billing := gjson.GetBytes(*sent, "system.0.text").String()
	for _, tag := range []string{"cc_entrypoint=cli;", "cc_turn_origin=tool;", "cc_prompt_index=9;", "cc_turn_index=3;"} {
		if !strings.Contains(billing, tag) {
			t.Errorf("lost native %s", tag)
		}
	}
	var id map[string]any
	if err := json.Unmarshal([]byte(gjson.GetBytes(*sent, "metadata.user_id").String()), &id); err != nil {
		t.Fatal(err)
	}
	if id["account_uuid"] != "account-a" || id["device_id"] != "device-local" || id["future_field"] != true || id["session_id"] != head.Get("x-claude-code-session-id") {
		t.Errorf("wrong selected identity: %+v", id)
	}
	if head.Get("Authorization") != "Bearer fixture-oauth" || head.Get("x-api-key") != "" {
		t.Error("caller credential escaped")
	}
	if head.Get("Anthropic-Organization-Id") != "" {
		t.Error("caller organization escaped to the selected OAuth account")
	}
}

func TestClaudeHTTPIdentityIsNotDuplicatedOrStripped(t *testing.T) {
	for _, identity := range []string{claudeHTTPIdentity, "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.", "You are a Claude agent, built on Anthropic's Claude Agent SDK."} {
		for _, array := range []bool{false, true} {
			system, _ := json.Marshal(identity)
			if array {
				system, _ = json.Marshal([]map[string]any{{"type": "text", "text": identity, "cache_control": map[string]string{"type": "ephemeral", "ttl": "5m"}}})
			}
			body := []byte(`{"system":` + string(system) + `,"messages":[{"role":"user","content":"hi"}]}`)
			got, err := claudeHTTPBody(body, claudeHTTPCall{})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, b := range gjson.GetBytes(got, "system").Array() {
				if claudeHTTPIdentityText(b.Get("text").String()) {
					count++
				}
			}
			if count != 1 {
				t.Errorf("identity inserted %d times", count)
			}
			if array && gjson.GetBytes(got, "system.1.cache_control.ttl").String() != "5m" {
				t.Error("lost identity cache mark")
			}
		}
	}
}

func TestClaudeHTTPSubscriptionCacheDefaults(t *testing.T) {
	for _, class := range []string{"main", "subagent", "probe"} {
		t.Run(class, func(t *testing.T) {
			h := httptest.NewRequest("POST", "/", nil).Header
			h.Set("x-claude-code-request-class", class)
			body, err := claudeHTTPBody([]byte(`{"system":"Instructions","tools":[{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`), claudeHTTPCall{headers: h})
			if err != nil {
				t.Fatal(err)
			}
			want := "1h"
			if class == "subagent" {
				want = "5m"
			}
			paths := claudeHTTPCachePaths(body)
			if class == "probe" {
				if len(paths) != 0 {
					t.Fatal("probe gained cache marks")
				}
				return
			}
			if len(paths) != 3 {
				t.Fatalf("cache breakpoints=%d, want 3", len(paths))
			}
			for _, p := range paths {
				if gjson.GetBytes(body, p+".ttl").String() != want {
					t.Errorf("wrong %s TTL", p)
				}
			}
		})
	}
	for _, ttl := range []string{"5m", "1h", ""} {
		mark := `{"type":"ephemeral"}`
		if ttl != "" {
			mark = `{"type":"ephemeral","ttl":"` + ttl + `"}`
		}
		body := []byte(`{"system":[{"type":"text","text":"Keep","cache_control":` + mark + `}],"messages":[{"role":"user","content":"hi"}]}`)
		got, err := claudeHTTPBody(body, claudeHTTPCall{})
		if err != nil {
			t.Fatal(err)
		}
		paths := claudeHTTPCachePaths(got)
		if len(paths) != 1 || gjson.GetBytes(got, paths[0]).Raw != mark {
			t.Errorf("explicit cache policy changed: %s", got)
		}
	}
}

func TestClaudeHTTPCacheDoesNotReadToolData(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x","content":{"cache_control":{"type":"ephemeral","ttl":"1h"}}}]}],"tools":[{"name":"x","input_schema":{"cache_control":{"ttl":"1h"}}}]}`)
	if len(claudeHTTPCachePaths(body)) != 0 {
		t.Fatal("tool data was treated as protocol cache policy")
	}
	r := httptest.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	claudeHTTPHeaders(r, body, claudeHTTPCall{})
	if strings.Contains(r.Header.Get("anthropic-beta"), "extended-cache-ttl") {
		t.Fatal("tool data enabled cache beta")
	}
}

func TestClaudeHTTPRefusedBetaStaysRemoved(t *testing.T) {
	s, p, _, head := httpClaudeFixture(t)
	s.refuseBetas(p, []byte("{\"error\":{\"message\":\"Unexpected value(s) `thinking-token-count-2026-05-13` for the `anthropic-beta` header.\"}}"))
	w := httptest.NewRecorder()
	var call Call
	status, msg := s.attempt(w, httpClaudeRequest(), provider.Anthropic, p, "claude-opus-5-5", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), &call)
	if status != 200 {
		t.Fatalf("failed: %d %s", status, msg)
	}
	if strings.Contains(head.Get("anthropic-beta"), "thinking-token-count-2026-05-13") {
		t.Fatal("compatibility profile re-added a refused beta")
	}
	if !strings.Contains(head.Get("anthropic-beta"), "oauth-2025-04-20") {
		t.Fatal("lost required OAuth beta")
	}
}

func TestClaudeHTTPProbeDoesNotGainCache(t *testing.T) {
	got, err := claudeHTTPBody([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), claudeHTTPCall{probe: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(claudeHTTPCachePaths(got)) != 0 {
		t.Fatal("test request acquired main-conversation cache defaults")
	}
}
