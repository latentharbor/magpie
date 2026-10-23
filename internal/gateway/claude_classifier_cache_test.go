package gateway

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// Keep the real classifier's shape (autoModeAsk): policy, user settings,
// and a transcript in several text parts of a single user message. Its
// growing history must not be flattened into one uncacheable text block.
func classifierCacheAsk(t *testing.T, transcript string) *Request {
	t.Helper()
	r, err := parseAnthropic([]byte(autoModeAsk("claude-sonnet-5")))
	if err != nil {
		t.Fatal(err)
	}
	r.Messages[1].Parts[1].Text = transcript
	return r
}

func classifierMarked(t *testing.T, blocks []map[string]any) int {
	t.Helper()
	mark := -1
	for i, b := range blocks {
		if cc, ok := b["cache_control"].(map[string]string); ok {
			if mark >= 0 || cc["ttl"] != "1h" {
				t.Fatal("classifier must use one 1h bridge cache slot")
			}
			mark = i
		}
	}
	if mark <= 0 || mark == len(blocks)-1 {
		t.Fatal("long classifier transcript has no marked prefix before its changing tail")
	}
	return mark
}

func classifierWords(blocks []map[string]any) string {
	var s strings.Builder
	for _, b := range blocks {
		text, _ := b["text"].(string)
		s.WriteString(text)
	}
	return s.String()
}

func TestClaudeClassifierCacheAcrossNewRuns(t *testing.T) {
	for _, policy := range []string{"<block>", "<severity>"} {
		for _, lean := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/lean_%t", policy, lean), func(t *testing.T) {
				// Multi-byte characters straddle the fixed byte boundaries.
				history := strings.Repeat("User: 检查项目🙂\nAction: read build.log\n", 16000)
				r := classifierCacheAsk(t, history+"Action: inspect files\n")
				r.System = "Security monitor. Reply " + policy
				r.leanPrompt = lean
				first := mustRender(t, r)
				a := classifierMarked(t, first)
				// The next check adds content inside <transcript>, before its
				// closing tag, not after the previous complete user message.
				r.Messages[1].Parts[1].Text += strings.Repeat("Result: more file contents\n", 4000) + "Action: inspect another file\n"
				second := mustRender(t, r)
				b := classifierMarked(t, second)
				if b < a || b-a >= 20 {
					t.Fatal("earlier cache boundary is outside the next mark's lookback")
				}
				for i := 0; i <= a; i++ {
					if first[i]["type"] != second[i]["type"] || first[i]["text"] != second[i]["text"] {
						t.Fatalf("cached prefix changed at block %d", i)
					}
				}
				for _, blocks := range [][]map[string]any{first, second} {
					for _, block := range blocks {
						if s, ok := block["text"].(string); ok && !utf8.ValidString(s) {
							t.Fatal("cache boundary split a UTF-8 character")
						}
					}
				}
				// Suppress classifier recognition without changing the text
				// renderer. Concatenated bytes, including policy and verdict
				// instructions, must be exactly those of the old renderer.
				r.Tools = []Tool{{Name: "unused"}}
				if classifierWords(second) != classifierWords(mustRender(t, r)) {
					t.Fatal("cache optimization changed classifier input")
				}
			})
		}
	}
}

func TestClaudeClassifierCachePreservesNeighbors(t *testing.T) {
	long := strings.Repeat("history\n", 24000)
	for _, name := range []string{"short", "ordinary", "tools", "client_marks"} {
		t.Run(name, func(t *testing.T) {
			r := classifierCacheAsk(t, long)
			switch name {
			case "short":
				r.Messages[1].Parts[1].Text = "a short transcript"
			case "ordinary":
				r.System = "Write a title."
			case "tools":
				r.Tools = []Tool{{Name: "read"}}
			case "client_marks":
				r.clientCache = &clientCache{count: 1, system: []cachePoint{{End: len(r.System), TTL: "5m"}}}
			}
			got := mustRender(t, r)
			// Tools stop classifier detection but don't change this text.
			r.Tools = []Tool{{Name: "unused"}}
			want := mustRender(t, r)
			if !reflect.DeepEqual(got, want) {
				t.Fatal("changed rendering outside default long classifier requests")
			}
		})
	}
}

func TestClaudeClassifierCachePreservesImages(t *testing.T) {
	r := classifierCacheAsk(t, strings.Repeat("transcript\n", 18000))
	r.Messages[1].Parts = append(r.Messages[1].Parts,
		Part{Kind: Image, MediaType: "image/png", Data: "aGVsbG8="},
		Part{Kind: Text, Text: "Check the action with the image above."})
	got := mustRender(t, r)
	classifierMarked(t, got)
	r.Tools = []Tool{{Name: "unused"}}
	want := mustRender(t, r)
	if classifierWords(got) != classifierWords(want) {
		t.Fatal("text beside image changed")
	}
	images := func(blocks []map[string]any) string {
		var out []map[string]any
		for _, b := range blocks {
			if b["type"] == "image" {
				out = append(out, b)
			}
		}
		b, _ := json.Marshal(out)
		return string(b)
	}
	if images(got) != images(want) || images(got) == "null" {
		t.Fatal("classifier image changed or was lost")
	}
}

func TestClaudeClassifierCacheBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, classifierCacheChunk - 1, classifierCacheChunk, classifierCacheChunk + 1, 2 * classifierCacheChunk} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			text := strings.Repeat("x", n)
			in := []map[string]any{{"type": "text", "text": "policy", "cache_control": cacheMark1h()}, {"type": "text", "text": text}}
			before, _ := json.Marshal(in)
			out := cacheClaudeClassifier(in, 0)
			after, _ := json.Marshal(in)
			if string(before) != string(after) || classifierWords(out) != "policy"+text {
				t.Fatal("input maps or text changed")
			}
			if n <= classifierCacheChunk {
				if !reflect.DeepEqual(in, out) {
					t.Fatal("short or exact-boundary transcript changed")
				}
			} else {
				classifierMarked(t, out)
			}
		})
	}
}

func TestClaudeClassifierCacheReachesOneOffRuns(t *testing.T) {
	// The shell's echo expands backslash escapes in the JSON prompt on
	// some platforms. Keep the captured CLI input byte-for-byte instead.
	h := newScriptHarness(t, "<severity>0</severity>", "echo() { printf '%s\\n' \"$*\"; }\n")
	var prompts [][]map[string]any
	for i := 0; i < 2; i++ {
		var body map[string]any
		if err := json.Unmarshal([]byte(autoModeAsk("claude-sonnet-5")), &body); err != nil {
			t.Fatal(err)
		}
		messages := body["messages"].([]any)
		parts := messages[1].(map[string]any)["content"].([]any)
		parts[1].(map[string]any)["text"] = strings.Repeat("User: inspect source\nAction: read file\n", 5000+i*2000)
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		var usage Usage
		code, msg := h.s.serveClaudeSubscription(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(encoded))),
			"anthropic", h.p, "claude-sonnet-5", encoded, &usage)
		if code != 200 || !strings.Contains(rec.Body.String(), "severity") {
			t.Fatalf("classifier verdict lost: status %d, %s", code, msg)
		}
	}
	runs := h.runs()
	if len(runs) != 2 {
		t.Fatal("classifiers must remain separate one-off runs")
	}
	for _, log := range runs {
		if !strings.Contains(log, "--effort low") || !strings.Contains(log, "--thinking disabled") {
			t.Fatal("classifier effort or thinking changed")
		}
		for _, line := range strings.Split(log, "\n") {
			if !strings.HasPrefix(line, "told ") {
				continue
			}
			var told struct {
				Message struct {
					Content []map[string]any `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "told ")), &told); err != nil {
				t.Fatal(err)
			}
			// JSON round-tripping changes the map type of cache_control.
			for _, b := range told.Message.Content {
				if cc, ok := b["cache_control"].(map[string]any); ok {
					b["cache_control"] = map[string]string{"type": cc["type"].(string), "ttl": cc["ttl"].(string)}
				}
			}
			classifierMarked(t, told.Message.Content)
			prompts = append(prompts, told.Message.Content)
		}
	}
	if len(prompts) != 2 {
		t.Fatal("each one-off must receive one classifier prompt")
	}
	if len(prompts[0]) > len(prompts[1]) {
		prompts[0], prompts[1] = prompts[1], prompts[0]
	}
	mark := classifierMarked(t, prompts[0])
	for i := 0; i <= mark; i++ {
		if prompts[0][i]["text"] != prompts[1][i]["text"] {
			t.Fatal("the actual CLI input lost the stable cached prefix")
		}
	}
	h.s.subscription.mu.Lock()
	defer h.s.subscription.mu.Unlock()
	if len(h.s.subscription.idle) != 0 {
		t.Fatal("one-off classifier displaced an idle conversation")
	}
}
