package claudelogin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGatewayUsageScopeAndRollback(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		enabled, managed   bool
		bad                bool
		status             int
		local              bool
	}{
		{"managed", "GET", "/api/oauth/usage", true, true, false, 200, true},
		{"default official", "GET", "/api/oauth/usage", true, false, false, 200, false},
		{"disabled", "GET", "/api/oauth/usage", false, true, false, 200, false},
		{"profile", "GET", "/api/oauth/profile", true, true, false, 200, false},
		{"authorization", "POST", "/v1/oauth/org/authorize", true, true, false, 200, false},
		{"other method", "POST", "/api/oauth/usage", true, true, false, 200, false},
		{"unknown config", "GET", "/api/oauth/usage", true, true, true, 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var official atomic.Int32
			client, p := fixture(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				official.Add(1)
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("official authentication changed")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":100,"resets_at":"2026-10-25T00:00:00Z"}}`))}, nil
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("usage sent to inference gateway") }))
			p.Config = func() (Config, error) {
				if tc.bad {
					return Config{}, errors.New("fixture unreadable")
				}
				return Config{Enabled: tc.enabled, GatewayUsage: tc.managed, Listen: "127.0.0.1:3426", Models: map[string]string{"claude-opus-5-5": "group/opus"}}, nil
			}
			req, _ := http.NewRequest(tc.method, "https://api.anthropic.com"+tc.path, nil)
			req.Header.Set("Authorization", "Bearer fixture-token")
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			if res.StatusCode != tc.status {
				t.Fatalf("status %d: %s", res.StatusCode, b)
			}
			if tc.local {
				var usage struct {
					Five struct {
						Used  float64 `json:"utilization"`
						Reset any     `json:"resets_at"`
					} `json:"five_hour"`
					Week struct {
						Used  float64 `json:"utilization"`
						Reset any     `json:"resets_at"`
					} `json:"seven_day"`
					Managed bool `json:"magpie_gateway_managed"`
				}
				if err := json.Unmarshal(b, &usage); err != nil {
					t.Fatal(err)
				}
				if !usage.Managed || usage.Five.Used != 0 || usage.Week.Used != 0 || usage.Five.Reset != nil || usage.Week.Reset != nil || official.Load() != 0 || res.Header.Get("Cache-Control") != "no-store" {
					t.Fatalf("wrong local availability: %s (official calls %d)", b, official.Load())
				}
			} else if tc.bad {
				if official.Load() != 0 {
					t.Fatal("unknown config forwarded quota")
				}
			} else if official.Load() != 1 || !strings.Contains(string(b), `"utilization":100`) {
				t.Fatalf("official quota changed: %s", b)
			}
		})
	}
}

func TestGatewayUsageRefusesUnmappedInference(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			client, p := fixture(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("unmapped inference spent official quota")
				return nil, io.EOF
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unmapped inference reached gateway") }))
			p.Config = func() (Config, error) {
				return Config{Enabled: true, GatewayUsage: true, Listen: "127.0.0.1:3426", Models: map[string]string{"claude-opus-5-5": "group/opus"}}, nil
			}
			res, err := client.Post("https://api.anthropic.com"+path, "application/json", strings.NewReader(`{"model":"unmapped","messages":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != 400 {
				t.Fatalf("unmapped inference status=%d", res.StatusCode)
			}
		})
	}
}

func TestGatewayManagedUsageKeepsRealGatewayLimit(t *testing.T) {
	client, p := fixture(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("gateway refusal fell back to official quota")
		return nil, io.EOF
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"all gateway accounts exhausted"}}`)
	}))
	p.Config = func() (Config, error) {
		return Config{Enabled: true, GatewayUsage: true, Listen: "127.0.0.1:3426", Models: map[string]string{"claude-opus-5-5": "group/opus"}}, nil
	}
	res, err := client.Post("https://api.anthropic.com/v1/messages", "application/json", strings.NewReader(`{"model":"claude-opus-5-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 429 || !strings.Contains(string(b), "all gateway accounts exhausted") {
		t.Fatalf("gateway limit lost: %d %s", res.StatusCode, b)
	}
}
