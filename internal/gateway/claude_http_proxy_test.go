package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/yetone/magpie/internal/claudelogin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// Exercise the two real listeners, including CONNECT and TLS. The committed
// fake-token CLI/SDK/Desktop-entrypoint captures are inputs, not live Desktop
// evidence. The appended signed tool round is a synthetic continuation.
func TestClaudeHTTPNativeThroughLoginProxy(t *testing.T) {
	claudeMadeFirst(t, false)
	cfg := settings.Load()
	cfg.ClaudeTransport = "http"
	if err := settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	oldAuth := claudeHTTPAuth
	claudeHTTPAuth = func(context.Context, *provider.Account) (provider.ClaudeHTTPCredentials, error) {
		return provider.ClaudeHTTPCredentials{Token: "selected-oauth", DeviceID: "selected-device", AccountUUID: "selected-account"}, nil
	}
	t.Cleanup(func() { claudeHTTPAuth = oldAuth })
	type observation struct {
		body []byte
		head http.Header
	}
	observed := make(chan observation, 1)
	s := New()
	t.Cleanup(s.subscription.abortAll)
	s.client = &http.Client{Transport: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		observed <- observation{b, r.Header.Clone()}
		reply := sse(
			"event: message_start\n"+`data: {"type":"message_start","message":{"id":"msg_proxy","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
			"event: message_delta\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
			"event: message_stop\n"+`data: {"type":"message_stop"}`)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(reply)), Request: r}, nil
	})}
	gateway := httptest.NewServer(s.Handler())
	defer gateway.Close()
	a, err := claudelogin.EnsureAuthority(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target, err := claudelogin.GatewayURL(gateway.URL)
	if err != nil {
		t.Fatal(err)
	}
	local := &http.Transport{}
	defer local.CloseIdleConnections()
	proxy := &claudelogin.Proxy{Authority: a, Gateway: target, Local: local,
		Config: func() (claudelogin.Config, error) {
			return claudelogin.Config{Enabled: true, Listen: "127.0.0.1:3426",
				Models: map[string]string{"claude-opus-5-5": "claude/claude-opus-5-5"}}, nil
		},
		Official: claudeHTTPRoundTrip(func(*http.Request) (*http.Response, error) {
			t.Error("selected inference escaped to the official account")
			return nil, io.EOF
		})}
	listener := httptest.NewServer(proxy)
	defer listener.Close()
	defer proxy.Close()
	proxyURL, _ := url.Parse(listener.URL)
	roots := x509.NewCertPool()
	roots.AddCert(a.CA)
	wire := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer wire.CloseIdleConnections()
	client := &http.Client{Transport: wire, Timeout: 15 * time.Second}
	checked := 0
	for _, path := range claudeHTTPCapturePaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var captures []struct {
			Headers map[string]string `json:"headers"`
			Body    json.RawMessage   `json:"body"`
		}
		if err := json.Unmarshal(data, &captures); err != nil {
			t.Fatal(err)
		}
		for _, capture := range captures {
			if gjson.GetBytes(capture.Body, "model").String() != "claude-opus-5-5" {
				continue
			}
			for _, toolRound := range []bool{false, true} {
				body := []byte(capture.Body)
				if toolRound {
					// Preserve the caller's opaque signed blocks and explicit
					// cache marks on a later turn; do not create another harness.
					body, err = sjson.SetRawBytes(body, "messages.-1", []byte(`{"role":"assistant","content":[{"type":"thinking","thinking":"fixture reasoning","signature":"opaque-signature"},{"type":"tool_use","id":"toolu_proxy","name":"Read","input":{"path":"fixture.txt"}}]}`))
					if err == nil {
						body, err = sjson.SetRawBytes(body, "messages.-1", []byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_proxy","content":"fixture result","cache_control":{"type":"ephemeral","ttl":"5m"}}]}`))
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				r, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				for key, value := range capture.Headers {
					r.Header.Set(key, value)
				}
				// A non-default caller choice exposes loss at either hop;
				// filling in the same default later must not mask the bug.
				if toolRound {
					r.Header.Set("Accept-Encoding", "identity")
				}
				r.Header.Set("Authorization", "Bearer official-must-not-escape")
				r.Header.Set("Cookie", "sessionKey=must-not-escape")
				res, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				reply, err := io.ReadAll(res.Body)
				res.Body.Close()
				if err != nil || res.StatusCode != 200 {
					t.Fatalf("%s tool=%v: %d %s %v", path, toolRound, res.StatusCode, reply, err)
				}
				var got observation
				select {
				case got = <-observed:
				default:
					t.Fatal("no HTTP subscription inference reached the mock")
				}
				for _, field := range []string{"messages", "tools", "max_tokens", "thinking", "output_config", "context_management", "diagnostics"} {
					if gjson.GetBytes(body, field).Raw != gjson.GetBytes(got.body, field).Raw {
						t.Errorf("%s tool=%v: proxy chain rewrote %s", path, toolRound, field)
					}
				}
				before, after := gjson.GetBytes(body, "system").Array(), gjson.GetBytes(got.body, "system").Array()
				if len(before) != len(after) {
					t.Fatal("proxy chain duplicated or removed system blocks")
				}
				for i := 1; i < len(before); i++ {
					if before[i].Raw != after[i].Raw {
						t.Errorf("proxy chain rewrote system block %d", i)
					}
				}
				for _, key := range []string{"User-Agent", "Accept", "Accept-Encoding", "Anthropic-Dangerous-Direct-Browser-Access", "X-Client-Request-Id", "X-Claude-Code-Session-Id", "X-Claude-Code-Prompt-Id", "X-Stainless-Package-Version"} {
					if got.head.Get(key) != r.Header.Get(key) {
						t.Errorf("%s tool=%v: proxy chain rewrote %s", path, toolRound, key)
					}
				}
				if got.head.Get("Authorization") != "Bearer selected-oauth" || got.head.Get("Cookie") != "" {
					t.Error("proxy chain forwarded official credentials")
				}
				checked++
			}
		}
	}
	if checked < 6 {
		t.Fatalf("checked %d requests, want all three native profiles and their tool rounds", checked)
	}
}
