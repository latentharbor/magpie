package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/usage"
)

var claudeHTTPDiagnosticKey = []byte(rand.Text())
var claudeHTTPDiagnosticRun = rand.Text()

// Class is an allow-list, not a raw header: an arbitrary caller's value may
// contain private text. Missing and unrecognized classes remain explicit.
func claudeHTTPDiagnosticClass(c claudeHTTPCall) string {
	if c.probe {
		return "probe"
	}
	switch class := strings.ToLower(c.headers.Get("x-claude-code-request-class")); class {
	case "main", "subagent", "probe", "quota", "title", "summary":
		return class
	case "":
		return "unspecified"
	default:
		return "other"
	}
}

// Capture final protocol cache positions without retaining message content,
// signatures, credentials, raw headers or metadata. Each prefix hash includes
// all prior blocks, but not the changing billing attribution or TTL markers.
func claudeHTTPDiagnostic(body []byte, c claudeHTTPCall, callerCache bool, dropped int) usage.ClaudeHTTPDiagnostic {
	root := gjson.ParseBytes(body)
	messages := root.Get("messages").Array()
	toolBlocks := root.Get("tools").Array()
	d := usage.ClaudeHTTPDiagnostic{RunID: claudeHTTPDiagnosticRun, Native: c.native, RequestClass: claudeHTTPDiagnosticClass(c), CacheSource: "none", UnsignedDropped: dropped,
		MaxTokens: root.Get("max_tokens").Int(), Messages: len(messages), Tools: len(toolBlocks)}
	if callerCache {
		d.CacheSource = "caller"
	} else if len(claudeHTTPCachePaths(body)) != 0 {
		d.CacheSource = "generated"
	}
	prefix := hmac.New(sha256.New, claudeHTTPDiagnosticKey)
	sum := func(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
	write := func(h hash.Hash, label, raw string) {
		fmt.Fprintf(h, "%d:%s%d:", len(label), label, len(raw))
		io.WriteString(h, raw)
	}
	mark := func(position string, control gjson.Result) {
		if !control.Exists() {
			return
		}
		if len(d.Prefixes) == 8 {
			d.PrefixesOmitted++
			return
		}
		ttl := "unknown"
		if control.Get("type").String() == "ephemeral" {
			switch value := control.Get("ttl"); {
			case !value.Exists():
				ttl = "5m"
			case value.String() == "5m" || value.String() == "1h":
				ttl = value.String()
			}
		}
		d.Prefixes = append(d.Prefixes, usage.ClaudeCachePrefix{Position: position, TTL: ttl, Hash: sum(prefix)})
	}
	block := func(label string, b gjson.Result, section hash.Hash) {
		write(prefix, "block", label)
		if section != nil {
			write(section, "block", label)
		}
		// Hash fields directly rather than rebuilding an arbitrarily large
		// text/image/tool-result block just to omit its cache marker.
		b.ForEach(func(key, value gjson.Result) bool {
			if key.String() != "cache_control" {
				write(prefix, label+"."+key.String(), value.Raw)
				if section != nil {
					write(section, label+"."+key.String(), value.Raw)
				}
			}
			return true
		})
		mark(label, b.Get("cache_control"))
	}
	tools := hmac.New(sha256.New, claudeHTTPDiagnosticKey)
	for i, b := range toolBlocks {
		block(fmt.Sprintf("tools.%d", i), b, tools)
	}
	d.ToolsHash = sum(tools)
	system := hmac.New(sha256.New, claudeHTTPDiagnosticKey)
	for i, b := range root.Get("system").Array() {
		if strings.HasPrefix(b.Get("text").String(), "x-anthropic-billing-header:") {
			mark(fmt.Sprintf("system.%d", i), b.Get("cache_control"))
			continue
		}
		block(fmt.Sprintf("system.%d", i), b, system)
	}
	d.SystemHash = sum(system)
	config := hmac.New(sha256.New, claudeHTTPDiagnosticKey)
	for _, field := range []string{"thinking", "output_config", "tool_choice", "context_management"} {
		write(config, field, root.Get(field).Raw)
	}
	d.ConfigHash = sum(config)
	for i, message := range messages {
		message.ForEach(func(key, value gjson.Result) bool {
			if key.String() != "content" {
				write(prefix, "message."+key.String(), value.Raw)
			}
			return true
		})
		content := message.Get("content")
		if content.IsArray() {
			for j, b := range content.Array() {
				block(fmt.Sprintf("messages.%d.content.%d", i, j), b, nil)
			}
		} else {
			write(prefix, "content", content.Raw)
		}
	}
	mark("cache_control", root.Get("cache_control"))
	return d
}
