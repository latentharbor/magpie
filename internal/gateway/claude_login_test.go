package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/claudelogin"
	"github.com/yetone/magpie/internal/provider"
)

// A native HTTPS client traverses CONNECT, TLS, the real gateway's caller
// guard, route resolution and Anthropic upstream translation. Nothing leaves
// loopback; fixture CA trust is confined to this test client.
func TestClaudeLoginThroughRealGatewayStack(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "max"} {
		t.Run(effort, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Api-Key") != "provider-secret" || strings.Contains(r.Header.Get("Authorization"), "native-secret") || r.Header.Get("Cookie") != "" {
					t.Error("wrong upstream credentials")
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if string(body["model"]) != `"claude-opus-5-5"` || !strings.Contains(string(body["output_config"]), effort) {
					t.Errorf("model/effort changed: %s %s", body["model"], body["output_config"])
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"gateway-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`)
			}))
			defer up.Close()
			if err := provider.Save(provider.Provider{ID: "login-fixture", Name: "Login fixture", Key: "provider-secret", Anthropic: up.URL, Models: []string{"claude-opus-5-5"}}); err != nil {
				t.Fatal(err)
			}
			gateway := httptest.NewServer(lanGuard(New().Handler()))
			defer gateway.Close()
			base, _ := url.Parse(gateway.URL)
			authority, err := claudelogin.EnsureAuthority(context.Background(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			p := &claudelogin.Proxy{Authority: authority, Gateway: base, Local: &http.Transport{}, Config: func() (claudelogin.Config, error) {
				return claudelogin.Config{Enabled: true, Listen: "127.0.0.1:3426", Models: map[string]string{"claude-opus-5-5": "login-fixture/claude-opus-5-5"}}, nil
			}}
			proxy := httptest.NewServer(p)
			defer proxy.Close()
			defer p.Close()
			proxyURL, _ := url.Parse(proxy.URL)
			roots := x509.NewCertPool()
			roots.AddCert(authority.CA)
			tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr}
			req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"`+effort+`"},"messages":[{"role":"user","content":"hello"}]}`))
			req.Header.Set("Authorization", "Bearer native-secret")
			req.Header.Set("Cookie", "session=native-secret")
			req.Header.Set("Content-Type", "application/json")
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != 200 || !strings.Contains(string(body), "gateway-ok") {
				t.Fatalf("%d %s", res.StatusCode, body)
			}
		})
	}
}
