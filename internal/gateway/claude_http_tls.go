package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/yetone/magpie/internal/netproxy"
	"golang.org/x/net/proxy"
)

// Captured twice from the local native Claude Code 2.1.309/macOS arm64 through
// a loopback CONNECT sink. Unlike CLIProxyAPI's older 2.1.220 profile, this
// offers X25519MLKEM768 and Ed25519. The inference plane offers HTTP/1.1 ALPN,
// status_request and SCT, unlike the same CLI's settings/policy control plane.
// Randoms, session IDs and ephemeral keys must remain freshly generated.
func claudeHTTPHello() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites:       []uint16{0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8, 0xc009, 0xc013, 0xc00a, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519MLKEM768, utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0807, 0x0603, 0x0201}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519MLKEM768}, {Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
	}
}

// A dedicated pool, never the ordinary vendor transport: HTTP/1.1 ALPN
// and the captured ClientHello. Pools are partitioned by proxy
// and selected account. Proxy edits can never reuse the old route's tunnel.
type claudeHTTPWire struct {
	next  http.RoundTripper
	mu    sync.Mutex
	pools map[string]*http.Transport
	roots *x509.CertPool
}

func (t *claudeHTTPWire) RoundTrip(r *http.Request) (*http.Response, error) {
	c, ok := r.Context().Value(claudeHTTPKey{}).(claudeHTTPCall)
	if !ok {
		return t.next.RoundTrip(r)
	}
	if r.URL.Scheme != "https" || r.URL.Host != "api.anthropic.com" {
		return nil, fmt.Errorf("Claude OAuth HTTP transport requires the fixed Anthropic HTTPS origin")
	}
	p, err := netproxy.Func(r)
	if err != nil {
		return nil, err
	}
	route := ""
	if p != nil {
		route = p.String()
	}
	key := route + "\x00" + c.auth.AccountUUID
	t.mu.Lock()
	pool := t.pools[key]
	if pool == nil {
		if len(t.pools) >= 32 {
			for key, old := range t.pools {
				old.CloseIdleConnections()
				delete(t.pools, key)
			}
		}
		pool = newClaudeHTTPPool(p, t.roots)
		t.pools[key] = pool
	}
	t.mu.Unlock()
	return pool.RoundTrip(r)
}

func (t *claudeHTTPWire) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, pool := range t.pools {
		pool.CloseIdleConnections()
	}
}

func newClaudeHTTPPool(p *url.URL, roots *x509.CertPool) *http.Transport {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	return &http.Transport{
		Protocols:             &protocols,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: upstreamHeaderWait,
		DialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			conn, err := dialClaudeHTTP(ctx, network, address, p)
			if err != nil {
				return nil, err
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				conn.Close()
				return nil, err
			}
			return handshakeClaudeHTTP(ctx, conn, host, roots)
		},
	}
}

func handshakeClaudeHTTP(ctx context.Context, conn net.Conn, host string, roots *x509.CertPool) (net.Conn, error) {
	// Native 2.1.309 reconnected without PSK/session resumption against the
	// same local TLS endpoint; do not introduce a different resumption profile.
	c := utls.UClient(conn, &utls.Config{ServerName: host, RootCAs: roots}, utls.HelloCustom)
	if err := c.ApplyPreset(claudeHTTPHello()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("Claude HTTP TLS profile: %w", err)
	}
	if err := c.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("Claude HTTP TLS handshake: %w", err)
	}
	return c, nil
}

// CONNECT is explicit because net/http's built-in proxy tunnel would perform
// its own crypto/tls handshake instead of calling DialTLSContext. Verification
// remains enabled for both an HTTPS proxy and the upstream server.
func dialClaudeHTTP(ctx context.Context, network, address string, p *url.URL) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if p == nil {
		return dialer.DialContext(ctx, network, address)
	}
	if netproxy.IsSOCKS(p) {
		d, err := proxy.FromURL(p, dialer)
		if err != nil {
			return nil, fmt.Errorf("Claude HTTP SOCKS proxy: %w", err)
		}
		cd, ok := d.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("Claude HTTP proxy does not support cancellation")
		}
		return cd.DialContext(ctx, network, address)
	}
	if p.Scheme != "http" && p.Scheme != "https" {
		return nil, fmt.Errorf("Claude HTTP unsupported proxy scheme %q", p.Scheme)
	}
	port := p.Port()
	if port == "" {
		port = "80"
		if p.Scheme == "https" {
			port = "443"
		}
	}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(p.Hostname(), port))
	if err != nil {
		return nil, err
	}
	if p.Scheme == "https" {
		tunnel := tls.Client(conn, &tls.Config{ServerName: p.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tunnel.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tunnel
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if p.User != nil {
		password, _ := p.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.User.Username()+":"+password)))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	res, err := http.ReadResponse(reader, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("Claude HTTP proxy CONNECT returned HTTP %d", res.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})
	return &claudeHTTPBufferedConn{Conn: conn, reader: reader}, nil
}

type claudeHTTPBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *claudeHTTPBufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }
