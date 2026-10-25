package claudelogin

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixture(t *testing.T, official http.RoundTripper, local http.Handler, connectContext ...context.Context) (*http.Client, *Proxy) {
	t.Helper()
	a, err := EnsureAuthority(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := httptest.NewServer(local)
	t.Cleanup(g.Close)
	u, _ := GatewayURL(g.URL)
	p := &Proxy{Authority: a, Config: func() (Config, error) {
		return Config{Enabled: true, Listen: "127.0.0.1:3426", Models: map[string]string{"claude-opus-5-5": "group/opus"}}, nil
	}, Gateway: u, Official: official, Local: &http.Transport{}}
	var handler http.Handler = p
	if len(connectContext) != 0 {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.ServeHTTP(w, r.WithContext(connectContext[0])) })
	}
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	t.Cleanup(p.Close)
	proxyURL, _ := url.Parse(s.URL)
	roots := x509.NewCertPool()
	roots.AddCert(a.CA)
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}, p
}

func TestSelectedInferenceUsesGatewayWithoutOfficialCredentials(t *testing.T) {
	var official atomic.Int32
	client, _ := fixture(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		official.Add(1)
		t.Error("selected request reached official API")
		return nil, io.EOF
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.URL.RawQuery != "beta=true" {
			t.Errorf("path lost: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer magpie-claude-desktop" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Anthropic-Organization-Id") != "" {
			t.Error("official credentials or identity leaked")
		}
		if r.Header.Get("Anthropic-Beta") != "context-1m" {
			t.Error("capability header lost")
		}
		if r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Anthropic-Dangerous-Direct-Browser-Access") != "true" {
			t.Error("native response encoding or SDK protocol header lost")
		}
		for _, h := range []string{"x-claude-code-session-id", "x-claude-code-prompt-id", "x-claude-code-request-class", "x-claude-code-agent-id", "x-claude-code-parent-agent-id"} {
			if r.Header.Get(h) != "native-"+h {
				t.Errorf("native affinity or request role lost: %s", h)
			}
		}
		var b map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&b)
		if string(b["model"]) != `"group/opus"` || string(b["output_config"]) != `{"effort":"max"}` || !bytes.Contains(b["messages"], []byte("tool_use")) {
			t.Errorf("inference changed: %s %s", b["model"], b["output_config"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "official-session=bad")
		w.Header().Set("Location", "https://evil.invalid/")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(`{"model":"claude-opus-5-5","stream":true,"max_tokens":128,"output_config":{"effort":"max"},"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"hello"}}]}]}`))
	req.Header.Set("Authorization", "Bearer official-oauth-secret")
	req.Header.Set("Cookie", "sessionKey=official-cookie")
	req.Header.Set("X-Api-Key", "official-api-secret")
	req.Header.Set("Anthropic-Organization-Id", "private-org")
	req.Header.Set("Anthropic-Beta", "context-1m")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	for _, h := range []string{"x-claude-code-session-id", "x-claude-code-prompt-id", "x-claude-code-request-class", "x-claude-code-agent-id", "x-claude-code-parent-agent-id"} {
		req.Header.Set(h, "native-"+h)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(b, []byte("message_stop")) || res.Header.Get("Set-Cookie") != "" || res.Header.Get("Location") != "" || official.Load() != 0 {
		t.Fatalf("bad gateway reply: %d %s", res.StatusCode, b)
	}
}

func TestUnselectedAndControlPlaneStayOfficial(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/api/oauth/profile", "/v1/messages/batches"} {
		t.Run(path, func(t *testing.T) {
			body := []byte(`{"model":"claude-sonnet-5-5","opaque":"unchanged"}`)
			var compressed bytes.Buffer
			gz := gzip.NewWriter(&compressed)
			gz.Write(body)
			gz.Close()
			client, _ := fixture(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				b, _ := io.ReadAll(r.Body)
				if !bytes.Equal(b, compressed.Bytes()) || r.Header.Get("Authorization") != "Bearer original" || r.Header.Get("Cookie") != "session=original" || r.URL.Host != Host || r.URL.Path != path {
					t.Error("official passthrough changed")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"official":true}`))}, nil
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("unselected/control request reached gateway")
				w.WriteHeader(500)
			}))
			req, _ := http.NewRequest("POST", "https://api.anthropic.com"+path, bytes.NewReader(compressed.Bytes()))
			req.Header.Set("Content-Encoding", "gzip")
			req.Header.Set("Authorization", "Bearer original")
			req.Header.Set("Cookie", "session=original")
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatal(res.StatusCode)
			}
		})
	}
}

func TestGatewayRefusalNeverFallsBackToSubscription(t *testing.T) {
	client, _ := fixture(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("gateway failure spent official allowance")
		return nil, io.EOF
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	}))
	res, err := client.Post("https://api.anthropic.com/v1/messages", "application/json", strings.NewReader(`{"model":"claude-opus-5-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 429 {
		t.Fatal(res.StatusCode)
	}
}

func TestBrokenRulesDoNotLeakInferenceToOfficial(t *testing.T) {
	client, p := fixture(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("broken config reached official")
		return nil, io.EOF
	}), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("broken config reached gateway") }))
	p.Config = func() (Config, error) { return Config{}, io.ErrUnexpectedEOF }
	res, err := client.Post("https://api.anthropic.com/v1/messages", "application/json", strings.NewReader(`{"model":"claude-opus-5-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatal(res.StatusCode)
	}
}

func TestAuthorityIsPrivatePersistentAndHostRestricted(t *testing.T) {
	dir := t.TempDir()
	a, err := EnsureAuthority(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EnsureAuthority(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.CA.Raw, b.CA.Raw) {
		t.Fatal("authority rotated on restart")
	}
	public, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(public)
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatal("public file contains private material")
	}
	roots := x509.NewCertPool()
	roots.AddCert(a.CA)
	leaf, _ := x509.ParseCertificate(a.Certificate.Certificate[0])
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: Host}); err != nil {
		t.Fatal(err)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "claude.ai"}); err == nil {
		t.Fatal("leaf impersonates another host")
	}
	for _, f := range []string{"authority.pem", "ca.pem"} {
		st, _ := os.Stat(filepath.Join(dir, f))
		if st.Mode().Perm() != 0o600 {
			t.Fatal("private file permissions", f, st.Mode())
		}
	}
	broken := []byte("corrupt-authority")
	os.WriteFile(filepath.Join(dir, "authority.pem"), broken, 0o600)
	if _, err = EnsureAuthority(context.Background(), dir); err == nil {
		t.Fatal("corrupt authority accepted")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "authority.pem"))
	if !bytes.Equal(after, broken) {
		t.Fatal("corrupt authority overwritten")
	}
}

func TestProxyRefusesBrowserAndPrivateTunnels(t *testing.T) {
	p := &Proxy{}
	for _, origin := range []string{"null", "https://evil.invalid"} {
		r := httptest.NewRequest("CONNECT", "http://api.anthropic.com:443", nil)
		r.Host = "api.anthropic.com:443"
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	for _, raw := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "10.0.0.1", "169.254.169.254", "fc00::1", "224.0.0.1", "0.0.0.0"} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Fatal("private destination accepted", raw)
		}
	}
}

// Headers, prompt cache markers, signed thinking and effort must survive the
// TLS hop as data. Only the model and the selected request's auth may change.
func TestCompressedCountTokensPreservesNativePayload(t *testing.T) {
	client, _ := fixture(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("selected token count reached official")
		return nil, io.EOF
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" || r.Header.Get("Content-Encoding") != "" {
			t.Errorf("bad count route %s", r.URL)
		}
		var b map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		if !bytes.Contains(b["system"], []byte(`"ttl":"1h"`)) || !bytes.Contains(b["messages"], []byte(`"signature":"native-signature"`)) || string(b["output_config"]) != `{"effort":"max"}` {
			t.Errorf("native data changed: %v", b)
		}
		io.WriteString(w, `{"input_tokens":42}`)
	}))
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	gz.Write([]byte(`{"model":"claude-opus-5-5","system":[{"type":"text","text":"cached","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reasoning","signature":"native-signature"}]}],"output_config":{"effort":"max"}}`))
	gz.Close()
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages/count_tokens", &b)
	req.Header.Set("Content-Encoding", "gzip")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(body, []byte("42")) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestControlPlaneSurvivesBrokenRoutingAndDisabledIsPassthrough(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		client, p := fixture(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer native" {
				t.Error("login credential changed")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("official"))}, nil
		}), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("passthrough reached gateway") }))
		p.Config = func() (Config, error) {
			if disabled {
				return Config{Listen: "127.0.0.1:3426", Models: map[string]string{"claude-opus-5-5": "group/opus"}}, nil
			}
			return Config{}, io.ErrUnexpectedEOF
		}
		path := "/api/oauth/profile"
		if disabled {
			path = "/v1/messages"
		}
		req, _ := http.NewRequest("POST", "https://api.anthropic.com"+path, strings.NewReader(`{"model":"claude-opus-5-5"}`))
		req.Header.Set("Authorization", "Bearer native")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(body) != "official" {
			t.Fatal(string(body))
		}
	}
}

func TestClientCancellationReachesGateway(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	client, _ := fixture(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5"}`))
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	cancel()
	res.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled Desktop stream kept gateway inference running")
	}
}

func TestProxyCloseCancelsOpenStream(t *testing.T) {
	cancelled := make(chan struct{})
	client, p := fixture(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	res, err := client.Post("https://api.anthropic.com/v1/messages", "application/json", strings.NewReader(`{"model":"claude-opus-5-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	res.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("closed proxy left upstream running")
	}
}

// CONNECT request lifetime does not own the hijacked tunnel. Native clients
// can end that outer request while continuing encrypted HTTP inside it.
func TestHijackedTunnelOutlivesConnectRequestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client, _ := fixture(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("official-ok"))}, nil
	}), http.NotFoundHandler(), ctx)
	res, err := client.Get("https://api.anthropic.com/api/oauth/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(b) != "official-ok" {
		t.Fatalf("tunnel inherited ended CONNECT: %d %s", res.StatusCode, b)
	}
}
