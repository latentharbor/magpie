package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func claudeHTTPIdentityText(text string) bool {
	switch strings.TrimSpace(text) {
	case claudeHTTPIdentity,
		"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.":
		return true
	}
	return false
}

// This is a compatibility decision, never authorization. Gateway authentication
// and the selected account's identity are checked separately.
func nativeClaudeHTTPRequest(h http.Header, body []byte) bool {
	if !fromClaudeCode(h) || h.Get("x-app") != "cli" {
		return false
	}
	// Continuations inherit system/tools/cache policy from the stored Thread.
	// Requiring identity blocks here misclassifies native deltas and injects
	// a different fingerprint, defeating the caller's inherited-field protocol.
	if gjson.GetBytes(body, "thread.type").String() == "continue" && gjson.GetBytes(body, "thread.previous_message_id").String() != "" {
		return true
	}
	if !gjson.GetBytes(body, "system").IsArray() {
		return false
	}
	billing := gjson.GetBytes(body, "system.0.text").String()
	if !strings.HasPrefix(billing, "x-anthropic-billing-header:") {
		return false
	}
	for _, block := range gjson.GetBytes(body, "system").Array() {
		if block.Get("type").String() == "text" && claudeHTTPIdentityText(block.Get("text").String()) {
			return true
		}
	}
	return false
}

func claudeHTTPUUID(value string) string {
	if _, err := uuid.Parse(value); err == nil {
		return value
	}
	return ""
}

func claudeHTTPMetadata(body []byte, c claudeHTTPCall) ([]byte, error) {
	// Keep future, caller-owned metadata fields while replacing credential-bound
	// identifiers. Never forward a caller's account/device as the selected login.
	id := gjson.GetBytes(body, "metadata.user_id").String()
	if !gjson.Valid(id) || !gjson.Parse(id).IsObject() {
		id = "{}"
	}
	var err error
	for _, field := range []struct{ key, value string }{{"device_id", c.auth.DeviceID}, {"account_uuid", c.auth.AccountUUID}, {"session_id", c.session}} {
		id, err = sjson.Set(id, field.key, field.value)
		if err != nil {
			return nil, err
		}
	}
	return sjson.SetBytes(body, "metadata.user_id", id)
}

// Only protocol cache positions count. A tool's schema, input or result may
// contain arbitrary user data with the same key and must never affect policy.
func claudeHTTPCachePaths(body []byte) []string {
	var paths []string
	add := func(path string) {
		if gjson.GetBytes(body, path).Exists() {
			paths = append(paths, path)
		}
	}
	for i := range gjson.GetBytes(body, "tools").Array() {
		add(fmt.Sprintf("tools.%d.cache_control", i))
	}
	for i := range gjson.GetBytes(body, "system").Array() {
		add(fmt.Sprintf("system.%d.cache_control", i))
	}
	for i, message := range gjson.GetBytes(body, "messages").Array() {
		for j := range message.Get("content").Array() {
			add(fmt.Sprintf("messages.%d.content.%d.cache_control", i, j))
		}
	}
	add("cache_control")
	return paths
}

// Native callers own placement and TTL, including an omitted TTL (5m). For
// generated requests with no cache policy, subscription main turns use 1h,
// subagents 5m. Any caller policy wins; no explicit TTL is silently upgraded.
func claudeHTTPDefaultCache(body []byte, c claudeHTTPCall) ([]byte, error) {
	if c.native || c.probe || c.callerCache || len(claudeHTTPCachePaths(body)) != 0 {
		return body, nil
	}
	class := strings.ToLower(c.headers.Get("x-claude-code-request-class"))
	if strings.Contains(class, "probe") || strings.Contains(class, "quota") || strings.Contains(class, "title") || strings.Contains(class, "summary") {
		return body, nil
	}
	ttl := "1h"
	if class != "" && class != "main" || c.headers.Get("x-claude-code-agent-id") != "" {
		ttl = "5m"
	}
	mark, _ := json.Marshal(map[string]string{"type": "ephemeral", "ttl": ttl})
	var paths []string
	tools := gjson.GetBytes(body, "tools").Array()
	if len(tools) != 0 {
		paths = append(paths, fmt.Sprintf("tools.%d.cache_control", len(tools)-1))
	}
	system := gjson.GetBytes(body, "system").Array()
	if len(system) > 1 {
		paths = append(paths, fmt.Sprintf("system.%d.cache_control", len(system)-1))
	}
	// A string content has no marker position; retain its text exactly while
	// representing it as a single protocol text block.
	for i := len(gjson.GetBytes(body, "messages").Array()) - 1; i >= 0; i-- {
		message := gjson.GetBytes(body, fmt.Sprintf("messages.%d", i))
		if message.Get("role").String() != "user" {
			continue
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			blocks, _ := json.Marshal([]map[string]string{{"type": "text", "text": content.String()}})
			var err error
			body, err = sjson.SetRawBytes(body, fmt.Sprintf("messages.%d.content", i), blocks)
			if err != nil {
				return nil, err
			}
			paths = append(paths, fmt.Sprintf("messages.%d.content.0.cache_control", i))
		} else if blocks := content.Array(); len(blocks) != 0 {
			paths = append(paths, fmt.Sprintf("messages.%d.content.%d.cache_control", i, len(blocks)-1))
		}
		break
	}
	for _, path := range paths {
		var err error
		body, err = sjson.SetRawBytes(body, path, mark)
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}
