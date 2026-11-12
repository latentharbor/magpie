package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/sjson"
)

func TestClaudeHTTPDiagnosticPrivacyAndPrefixes(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cch=11111;"},{"type":"text","text":"PRIVATE-SYSTEM","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"Read","description":"PRIVATE-TOOL","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"PRIVATE-MESSAGE","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],"metadata":{"user_id":"PRIVATE-IDENTITY"},"output_config":{"effort":"max"}}`)
	c := claudeHTTPCall{native: true, headers: http.Header{"X-Claude-Code-Request-Class": {"PRIVATE-HEADER"}}}
	d := claudeHTTPDiagnostic(body, c, true, 2)
	if d.RequestClass != "other" || d.CacheSource != "caller" || d.UnsignedDropped != 2 || len(d.Prefixes) != 2 || d.Prefixes[0].TTL != "5m" || d.Prefixes[1].TTL != "1h" {
		t.Fatalf("wrong diagnostic: %+v", d)
	}
	encoded, err := json.Marshal(d)
	if err != nil || bytes.Contains(encoded, []byte("PRIVATE-")) || bytes.Contains(encoded, claudeHTTPDiagnosticKey) {
		t.Fatal("diagnostic retained private data or its hash key")
	}
	change := func(path, value string) []byte {
		t.Helper()
		got, err := sjson.SetBytes(body, path, value)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for path, value := range map[string]string{"system.0.text": "x-anthropic-billing-header: cch=99999;", "metadata.user_id": "another account", "system.1.cache_control.ttl": "1h"} {
		next := claudeHTTPDiagnostic(change(path, value), c, true, 0)
		if next.Prefixes[1].Hash != d.Prefixes[1].Hash || next.SystemHash != d.SystemHash {
			t.Errorf("non-content %s invalidated content fingerprint", path)
		}
	}
	next := claudeHTTPDiagnostic(change("messages.0.content.0.text", "different"), c, true, 0)
	if next.Prefixes[0].Hash != d.Prefixes[0].Hash || next.Prefixes[1].Hash == d.Prefixes[1].Hash {
		t.Fatal("changed tail must leave earlier prefix alone, but change its own")
	}
	next = claudeHTTPDiagnostic(change("tools.0.description", "different"), c, true, 0)
	if next.ToolsHash == d.ToolsHash || next.Prefixes[0].Hash == d.Prefixes[0].Hash {
		t.Fatal("tool change didn't change following prefix")
	}
	next = claudeHTTPDiagnostic(change("messages.0.output_config.effort", "high"), c, true, 0)
	if next.Prefixes[1].Hash == d.Prefixes[1].Hash {
		t.Fatal("per-message effort was omitted from prefix")
	}
	next = claudeHTTPDiagnostic(change("output_config.effort", "high"), c, true, 0)
	if next.ConfigHash == d.ConfigHash {
		t.Fatal("request effort was omitted from config fingerprint")
	}
	if again := claudeHTTPDiagnostic(body, c, true, 2); again.Prefixes[1].Hash != d.Prefixes[1].Hash {
		t.Fatal("fingerprints change within the same process")
	}
}

func TestClaudeHTTPDiagnosticBoundedAndClassified(t *testing.T) {
	block := `{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"PRIVATE-TTL"}}`
	body := []byte(`{"system":[` + strings.TrimSuffix(strings.Repeat(block+",", 20), ",") + `]}`)
	d := claudeHTTPDiagnostic(body, claudeHTTPCall{}, true, 0)
	if len(d.Prefixes) != 8 || d.PrefixesOmitted != 12 || d.Prefixes[0].TTL != "unknown" || d.RequestClass != "unspecified" {
		t.Fatalf("unbounded or raw diagnostic: %+v", d)
	}
	for _, class := range []string{"main", "subagent", "probe", "quota", "title", "summary"} {
		c := claudeHTTPCall{headers: http.Header{"X-Claude-Code-Request-Class": {class}}}
		if claudeHTTPDiagnosticClass(c) != class {
			t.Errorf("lost known class %q", class)
		}
	}
}

func BenchmarkClaudeHTTPDiagnosticLargeHistory(b *testing.B) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("a", 8<<20) + `","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		claudeHTTPDiagnostic(body, claudeHTTPCall{}, true, 0)
	}
}
