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

func TestClaudeHTTPTeamGeneric429(t *testing.T) {
	for _, outcome := range []string{"exhausted", "healthy", "unknown", "persistent"} {
		t.Run(outcome, func(t *testing.T) {
			f := nativeTeamFixture(t)
			conf := settings.Load()
			conf.ClaudeTransport = "http"
			if err := settings.Save(conf); err != nil {
				t.Fatal(err)
			}
			// Startup capture must finish before installing the failure probe.
			f.stop()
			var probes atomic.Int32
			oldUsage := provider.UsageClaudeVia(func(_ context.Context, dir string) (string, error) {
				probes.Add(1)
				if outcome == "unknown" {
					return "", errors.New("fixture quota unavailable")
				}
				auth, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
				if err != nil {
					return "", err
				}
				if outcome == "exhausted" && strings.Contains(string(auth), "tok-a") {
					return "Current session: 100% used · resets " + time.Now().UTC().Add(2*time.Hour).Format("3:04pm") +
						" (UTC)\nCurrent week (all models): 0% used", nil
				}
				return "Current session: 0% used\nCurrent week (all models): 0% used", nil
			})
			t.Cleanup(func() { provider.UsageClaudeVia(oldUsage) })
			ctx, cancelController := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); provider.KeepClaudeTeam(ctx) }()
			t.Cleanup(func() { cancelController(); <-done })
			oldAuth := claudeHTTPAuth
			claudeHTTPAuth = func(_ context.Context, a *provider.Account) (provider.ClaudeHTTPCredentials, error) {
				return provider.ClaudeHTTPCredentials{Token: a.User, DeviceID: "fixture-device", AccountUUID: a.User}, nil
			}
			t.Cleanup(func() { claudeHTTPAuth = oldAuth })
			oldPause := retryPause
			retryPause = time.Millisecond
			t.Cleanup(func() { retryPause = oldPause })
			s := New()
			t.Cleanup(s.subscription.abortAll)
			var calls atomic.Int32
			s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
				n := calls.Add(1)
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), "retain-generic-429") {
					t.Error("lost the original request")
				}
				h := http.Header{"Content-Type": {"application/json"}}
				a := r.Header.Get("Authorization") == "Bearer a@example.com"
				if outcome == "exhausted" && a || outcome == "persistent" || n == 1 {
					h.Set("anthropic-ratelimit-unified-5h-utilization", "0.99")
					h.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10))
					return &http.Response{StatusCode: 429, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`)), Request: r}, nil
				}
				if outcome == "exhausted" && a || outcome != "exhausted" && !a {
					t.Error("retried on the wrong account")
				}
				return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"id":"msg_ok","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"retained-generic-429"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)), Request: r}, nil
			})}
			h := httptest.NewServer(s.Handler())
			defer h.Close()
			reqCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			r, _ := http.NewRequestWithContext(reqCtx, "POST", h.URL+"/v1/messages", strings.NewReader(`{"model":"claude/claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"retain-generic-429"}]}`))
			r.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			wantStatus, wantCalls, wantWrites := 200, int32(2), 0
			if outcome == "persistent" {
				wantStatus, wantCalls = 429, 1+rateRetries
			}
			if outcome == "exhausted" {
				wantWrites = 2
			}
			f.mu.Lock()
			writes := f.writes
			f.mu.Unlock()
			if res.StatusCode != wantStatus || calls.Load() != wantCalls || writes != wantWrites {
				t.Fatalf("status=%d calls=%d writes=%d, want %d/%d/%d: %s", res.StatusCode, calls.Load(), writes, wantStatus, wantCalls, wantWrites, body)
			}
			if probes.Load() == 0 {
				t.Fatal("ambiguous 429 never verified quota")
			}
			if outcome != "exhausted" && probes.Load() != 1 {
				t.Fatalf("a retry burst collected quota %d times", probes.Load())
			}
			if outcome != "exhausted" && provider.ClaudeTeamExhausted("a@example.com") {
				t.Fatal("generic 429 fabricated global exhaustion")
			}
		})
	}
}

func TestClaudeTeamRateHeadersKeepExplicitBackoff(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	h.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10))
	h.Set("Retry-After", "2")
	clean := claudeTeamRateHeaders(h)
	if wait, ok := passing(429, clean, []byte(`{"error":{"message":"rate limit"}}`), 0); !ok || wait != 2*time.Second {
		t.Fatalf("explicit backoff lost: %v %v", wait, ok)
	}
	if h.Get("anthropic-ratelimit-unified-5h-reset") == "" || clean.Get("anthropic-ratelimit-unified-5h-reset") != "" {
		t.Fatal("mutated original headers or retained an allowance deadline")
	}
}
