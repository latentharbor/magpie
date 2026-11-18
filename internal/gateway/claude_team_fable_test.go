package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// Both a pre-known full window (no initial candidate) and new rejection
// headers must keep the original HTTP request through the same seat handoff.
func TestClaudeHTTPTeamFableHandoffRetainsRequest(t *testing.T) {
	for _, mode := range []string{"headers", "known", "usage"} {
		t.Run(mode, func(t *testing.T) {
			known := mode == "known"
			f := nativeTeamFixture(t)
			f.stop()
			conf := settings.Load()
			conf.ClaudeTransport = "http"
			if err := settings.Save(conf); err != nil {
				t.Fatal(err)
			}
			var refused atomic.Bool
			var probes atomic.Int32
			oldUsage := provider.UsageClaudeVia(func(_ context.Context, dir string) (string, error) {
				if mode != "usage" || !refused.Load() {
					return "", errors.New("fixture quota unavailable")
				}
				probes.Add(1)
				auth, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
				if err != nil {
					return "", err
				}
				fable := "20"
				if strings.Contains(string(auth), "tok-a") {
					fable = "100"
				}
				now := time.Now().UTC()
				week := now.Add(3 * 24 * time.Hour).Format("Jan 2 at 3:04pm") + " (UTC)"
				return "Current session: 20% used · resets " + now.Add(time.Hour).Format("Jan 2 at 3:04pm") + " (UTC)\nCurrent week (all models): 83% used · resets " + week + "\nCurrent week (Fable): " + fable + "% used · resets " + week, nil
			})
			t.Cleanup(func() { provider.UsageClaudeVia(oldUsage) })
			for _, user := range []string{"a@example.com", "b@example.com"} {
				used := .2
				if user == "a@example.com" && known {
					used = 1
				}
				provider.NoteClaudeLimits(user, []provider.ClaudeLimit{{Kind: "five_hour", Used: .2, ResetsAt: time.Now().Add(time.Hour).Unix()}, {Kind: "seven_day", Used: .83, ResetsAt: time.Now().Add(3 * 24 * time.Hour).Unix()}, {Kind: "seven_day_overage_included", Used: used, ResetsAt: time.Now().Add(3 * 24 * time.Hour).Unix()}})
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); provider.KeepClaudeTeam(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			oldAuth := claudeHTTPAuth
			claudeHTTPAuth = func(_ context.Context, a *provider.Account) (provider.ClaudeHTTPCredentials, error) {
				return provider.ClaudeHTTPCredentials{Token: a.User, DeviceID: "fixture-device", AccountUUID: a.User}, nil
			}
			t.Cleanup(func() { claudeHTTPAuth = oldAuth })
			s := New()
			t.Cleanup(s.subscription.abortAll)
			var first atomic.Int32
			s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
				raw, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(raw), "retain-fable-original") {
					t.Error("lost original request")
				}
				h := http.Header{"Content-Type": {"application/json"}}
				status := 200
				body := `{"id":"msg_ok","type":"message","role":"assistant","model":"claude-fable-5-2","content":[{"type":"text","text":"fable-original-answered"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				if r.Header.Get("Authorization") == "Bearer a@example.com" {
					first.Add(1)
					refused.Store(true)
					status = 429
					h.Set("anthropic-ratelimit-unified-status", "rejected")
					h.Set("anthropic-ratelimit-unified-representative-claim", "seven_day_overage_included")
					h.Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.97")
					h.Set("anthropic-ratelimit-unified-7d_oi-reset", strconv.FormatInt(time.Now().Add(3*24*time.Hour).Unix(), 10))
					if mode == "usage" {
						h = http.Header{"Content-Type": {"application/json"}}
					}
					body = `{"type":"error","error":{"type":"rate_limit_error","message":"You've reached your Fable limit. Switch models with /model."}}`
				} else if r.Header.Get("Authorization") != "Bearer b@example.com" {
					t.Error("unknown inference identity")
				}
				return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			h := httptest.NewServer(s.Handler())
			defer h.Close()
			requestCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			req, _ := http.NewRequestWithContext(requestCtx, "POST", h.URL+"/v1/messages", strings.NewReader(`{"model":"claude/claude-fable-5-2","max_tokens":100,"messages":[{"role":"user","content":"retain-fable-original"}]}`))
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != 200 || !strings.Contains(string(body), "fable-original-answered") {
				t.Fatalf("Fable request stranded: %d %s, team %+v", res.StatusCode, body, provider.ClaudeTeamStatus())
			}
			f.mu.Lock()
			writes := f.writes
			f.mu.Unlock()
			if writes != 2 {
				t.Fatalf("wanted one background handoff, got %d writes", writes)
			}
			if known && first.Load() != 0 {
				t.Fatal("known full Fable window still sent inference")
			}
			if mode == "usage" && probes.Load() == 0 {
				t.Fatal("body-only quota refusal never rechecked model allowance")
			}
			if provider.ClaudeTeamExhausted("a@example.com") {
				t.Fatal("Fable rejection fabricated global exhaustion")
			}
		})
	}
}
