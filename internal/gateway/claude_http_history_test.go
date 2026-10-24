package gateway

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/yetone/magpie/internal/provider"
)

// Older CLI-backed sessions persisted thinking text without a signature. A
// resumed tool round must still carry its calls/results after HTTP migration.
func TestClaudeHTTPUnsignedHistory(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5-5","max_tokens":512,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.309; cch=00000;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"continue"},{"role":"assistant","content":[{"type":"thinking","thinking":"old CLI history","signature":""},{"type":"thinking","thinking":"missing signature"},{"type":"thinking","thinking":"null signature","signature":null},{"type":"thinking","thinking":"","signature":"valid-opaque-signature"},{"type":"redacted_thinking","data":"opaque"},{"type":"tool_use","id":"toolu_keep","name":"Read","input":{"signature":"","type":"thinking"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_keep","content":"result"}]}]}`)
	body, err := sjson.SetRawBytes(body, "system.1.cache_control", []byte(`{"type":"ephemeral","ttl":"5m"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "generated", true: "native"}[native], func(t *testing.T) {
			s, p, sent, _ := httpClaudeFixture(t)
			r := httpClaudeRequest()
			if native {
				r.Header.Set("User-Agent", "claude-cli/2.1.309 (external, cli)")
				r.Header.Set("x-app", "cli")
			}
			var call Call
			original := bytes.Clone(body)
			status, msg := s.attempt(httptest.NewRecorder(), r, provider.Anthropic, p, "claude-opus-5-5", body, &call)
			if !bytes.Equal(body, original) {
				t.Fatal("repair mutated the caller's history")
			}
			if status != 200 {
				t.Fatalf("attempt: %d %s", status, msg)
			}
			if d := call.Usage.claudeHTTP; d == nil || d.Native != native || d.UnsignedDropped != 3 || d.CacheSource != "caller" {
				t.Fatalf("missing or wrong final-wire diagnostic: %+v", d)
			}
			blocks := gjson.GetBytes(*sent, "messages.1.content").Array()
			if len(blocks) != 3 {
				t.Fatalf("unsigned history still on wire: got %d blocks, want 3", len(blocks))
			}
			for i, original := range gjson.GetBytes(body, "messages.1.content").Array()[3:] {
				if blocks[i].Raw != original.Raw {
					t.Fatalf("changed signed thinking/redaction/tool block %d", i)
				}
			}
			if gjson.GetBytes(*sent, "messages.2").Raw != gjson.GetBytes(body, "messages.2").Raw || gjson.GetBytes(*sent, "output_config.effort").String() != "max" {
				t.Fatal("changed tool result or requested effort")
			}
		})
	}
}

func TestClaudeHTTPHistoryLeavesNeighboringDataAlone(t *testing.T) {
	for _, content := range []string{
		`{"type":"thinking","thinking":"","signature":"valid"}`,
		`{"type":"thinking","thinking":"malformed signature","signature":42}`,
		`{"type":"redacted_thinking","data":"opaque"}`,
		`{"type":"text","text":"old","signature":""}`,
		`{"type":"tool_use","id":"toolu_x","input":{"type":"thinking","signature":""}}`,
	} {
		body := []byte(`{"messages":[{"role":"assistant","content":[` + content + `]}]}`)
		got, err := claudeHTTPBody(body, claudeHTTPCall{native: true})
		if err != nil || gjson.GetBytes(got, "messages").Raw != gjson.GetBytes(body, "messages").Raw {
			t.Fatalf("changed neighboring protocol data: %s, %v", content, err)
		}
	}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"thinking","signature":""}]}]}`)
	got, err := claudeHTTPBody(body, claudeHTTPCall{native: true})
	if err != nil || gjson.GetBytes(got, "messages").Raw != gjson.GetBytes(body, "messages").Raw {
		t.Fatal("repair reached non-assistant data")
	}
}

func TestClaudeHTTPUnsignedOnlyMessage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"before"},{"role":"assistant","content":[{"type":"thinking","thinking":"old","signature":""}]},{"role":"user","content":"after"}]}`)
	got, err := claudeHTTPBody(body, claudeHTTPCall{native: true})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, "messages.#").Int() != 2 || gjson.GetBytes(got, "messages.1.content").String() != "after" {
		t.Fatal("repair left an empty assistant message or lost the following user message")
	}
}

func TestClaudeHTTPUnsignedHistoryDoesNotInventCachePolicy(t *testing.T) {
	body := []byte(`{"system":"instructions","messages":[{"role":"user","content":"before"},{"role":"assistant","content":[{"type":"thinking","thinking":"old","signature":"","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"after"}]}`)
	got, err := claudeHTTPBody(body, claudeHTTPCall{})
	if err != nil || len(claudeHTTPCachePaths(got)) != 0 {
		t.Fatal("removing unsigned history replaced the caller's cache policy with generated defaults")
	}
}
