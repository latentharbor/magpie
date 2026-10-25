package claudelogin

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/claudeanchor"
)

const maxBody = 64 << 20

// Proxy terminates TLS for api.anthropic.com and an explicitly trusted optional
// claude.ai quota scope. All other CONNECTs are
// opaque tunnels to public addresses. It never follows upstream redirects.
type Proxy struct {
	Authority    *Authority
	WebAuthority *Authority
	Config       func() (Config, error)
	Gateway      *url.URL
	Official     http.RoundTripper
	Local        http.RoundTripper
	mu           sync.Mutex
	conns        map[net.Conn]struct{}
	closed       bool
	requests     claudeanchor.Store[string]
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || r.Header.Get("Origin") != "" {
		http.Error(w, "local native clients only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodConnect {
		http.Error(w, "HTTPS CONNECT required", http.StatusMethodNotAllowed)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || port != "443" {
		http.Error(w, "HTTPS port 443 required", http.StatusForbidden)
		return
	}
	if strings.EqualFold(host, Host) {
		p.intercept(w, r, Host, p.Authority, p.inference)
		return
	}
	if strings.EqualFold(host, UsageHost) {
		a, err := p.usageAuthority(r.Context())
		if err != nil {
			http.Error(w, "organization usage certificate unavailable", http.StatusServiceUnavailable)
			return
		}
		if a != nil {
			p.intercept(w, r, UsageHost, a, p.webUsage)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	up, err := dialPublic(ctx, host, port)
	if err != nil {
		http.Error(w, "cannot open public HTTPS tunnel", http.StatusBadGateway)
		return
	}
	down, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		up.Close()
		return
	}
	if !p.track(down) {
		up.Close()
		down.Close()
		return
	}
	defer p.untrack(down)
	defer down.Close()
	defer up.Close()
	if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() { io.Copy(up, rw); up.Close(); close(done) }()
	io.Copy(down, up)
	down.Close()
	up.Close()
	<-done
}

func dialPublic(ctx context.Context, host, port string) (net.Conn, error) {
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, fmt.Errorf("private tunnel destination refused")
		}
	}
	var last error
	for _, ip := range ips {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("no public address")
	}
	return nil, last
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}

func (p *Proxy) intercept(w http.ResponseWriter, r *http.Request, host string, authority *Authority, handler http.HandlerFunc) {
	c, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	if !p.track(c) {
		c.Close()
		return
	}
	defer p.untrack(c)
	defer c.Close()
	if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}
	// Honor bytes already read by net/http beyond the CONNECT headers.
	tc := tls.Server(&bufferedConn{Conn: c, reader: rw}, &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello.ServerName != host {
			return nil, fmt.Errorf("unexpected TLS server name")
		}
		return &authority.Certificate, nil
	}})
	// A hijacked CONNECT is no longer owned by the outer HTTP request.
	// The inner server tracks real stream cancellation; Close owns shutdown.
	tunnelContext := context.WithoutCancel(r.Context())
	ctx, cancel := context.WithTimeout(tunnelContext, 15*time.Second)
	err = tc.HandshakeContext(ctx)
	cancel()
	if err != nil {
		return
	}
	ln := &oneConnListener{conn: tc, done: make(chan struct{})}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: time.Minute, BaseContext: func(net.Listener) context.Context { return tunnelContext }}
	// Serve waits for Close rather than returning after the first Accept: its
	// handler and streaming reply must finish before the tunnel is released.
	srv.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			ln.Close()
		}
	}
	srv.Serve(ln)
}

type bufferedConn struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

type oneConnListener struct {
	conn  net.Conn
	once  sync.Once
	done  chan struct{}
	mu    sync.Mutex
	taken bool
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.taken {
		l.taken = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	<-l.done
	return nil, net.ErrClosed
}
func (l *oneConnListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func (p *Proxy) inference(w http.ResponseWriter, r *http.Request) {
	if r.Host != Host && r.Host != Host+":443" {
		http.Error(w, "unexpected authority", http.StatusForbidden)
		return
	}
	selected := ""
	if r.Method == http.MethodGet && r.URL.Path == "/api/oauth/usage" {
		cfg, err := p.Config()
		if err != nil || cfg.Validate() != nil {
			http.Error(w, "Claude login configuration cannot be read", http.StatusServiceUnavailable)
			return
		}
		if cfg.Enabled && cfg.GatewayUsage {
			// This is local Code availability, not the official account's quota.
			// Requests in this mode must have a gateway mapping (see below).
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			io.WriteString(w, gatewayUsageJSON)
			return
		}
	}
	if r.Method == http.MethodPost && (r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/messages/count_tokens" || r.URL.Path == "/v1/messages/cache_touch") {
		cfg, err := p.Config()
		if err != nil {
			http.Error(w, "Claude login configuration cannot be read", http.StatusServiceUnavailable)
			return
		}
		if err = cfg.Validate(); err != nil {
			http.Error(w, "Claude login configuration is invalid", http.StatusServiceUnavailable)
			return
		}
		if cfg.Enabled {
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
			r.Body.Close()
			if err != nil || len(body) > maxBody {
				http.Error(w, "inference request too large or unreadable", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			decoded := body
			switch r.Header.Get("Content-Encoding") {
			case "":
			case "gzip":
				gz, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					http.Error(w, "invalid compressed inference", http.StatusBadRequest)
					return
				}
				decoded, err = io.ReadAll(io.LimitReader(gz, maxBody+1))
				gz.Close()
				if err != nil || len(decoded) > maxBody {
					http.Error(w, "compressed inference too large or unreadable", http.StatusRequestEntityTooLarge)
					return
				}
			default:
				http.Error(w, "unsupported inference encoding", http.StatusUnsupportedMediaType)
				return
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(decoded, &fields); err != nil || fields == nil {
				http.Error(w, "invalid inference JSON", http.StatusBadRequest)
				return
			}
			if r.URL.Path == "/v1/messages/cache_touch" {
				var id string
				json.Unmarshal(fields["request_id"], &id)
				if target, known := p.requests.Get(id, time.Now()); known {
					for _, value := range cfg.Models {
						if value == target {
							selected = target
							break
						}
					}
					if selected == "" {
						http.Error(w, "Claude cache model is no longer mapped", http.StatusNotFound)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(decoded))
					r.ContentLength = int64(len(decoded))
					r.Header.Del("Content-Encoding")
					r.Header.Del("Content-Length")
				} else if cfg.GatewayUsage {
					http.Error(w, "Claude cache request is unknown", http.StatusNotFound)
					return
				}
				// A cache touch has no model. Preserve its native request_id
				// body; the gateway resolves the account from its own record.
			} else {
				var model string
				if err := json.Unmarshal(fields["model"], &model); err != nil {
					http.Error(w, "inference model is missing", http.StatusBadRequest)
					return
				}
				selected = cfg.Models[model]
				if selected == "" && cfg.GatewayUsage {
					http.Error(w, "map this Code model in Magpie before using gateway-managed usage", http.StatusBadRequest)
					return
				}
				if selected != "" {
					fields["model"], _ = json.Marshal(selected)
					decoded, err = json.Marshal(fields)
					if err != nil {
						http.Error(w, "cannot encode inference", 500)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(decoded))
					r.ContentLength = int64(len(decoded))
					r.Header.Del("Content-Encoding")
					r.Header.Del("Content-Length")
				}
			}
		}
	}
	base := &url.URL{Scheme: "https", Host: Host}
	transport := p.Official
	if selected != "" {
		base = p.Gateway
		transport = p.Local
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(base)
		pr.Out.Host = base.Host
		pr.Out.Header.Del("Proxy-Authorization")
		pr.Out.Header.Del("Proxy-Connection")
		if selected != "" {
			clean := http.Header{}
			for key, values := range pr.Out.Header {
				lower := strings.ToLower(key)
				if nativeHeader(lower) {
					clean[key] = values
				}
			}
			clean.Set("Authorization", "Bearer magpie-claude-desktop")
			pr.Out.Header = clean
		}
	}, Transport: transport, FlushInterval: -1, ModifyResponse: func(res *http.Response) error {
		if selected != "" {
			res.Header.Del("Set-Cookie")
			res.Header.Del("Location")
			if r.URL.Path == "/v1/messages" && res.StatusCode >= 200 && res.StatusCode < 300 {
				id := res.Header.Get("Request-Id")
				if id == "" {
					id = res.Header.Get("X-Request-Id")
				}
				p.requests.Put(id, selected, time.Now())
			}
		}
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {

		// Do not send a failed selected request to the official subscription:
		// it can have generated output already and incur a second charge.
		http.Error(w, "Claude login upstream unavailable", http.StatusBadGateway)
	}}
	proxy.ServeHTTP(w, r)
}

// Preserve native session affinity, prompt IDs and main/subagent roles as
// well as protocol capabilities. These are not login or account credentials.
func nativeHeader(lower string) bool {
	switch lower {
	case "content-type", "accept", "user-agent", "anthropic-version", "anthropic-beta", "x-app",
		"x-claude-code-session-id", "x-claude-code-prompt-id", "x-claude-code-request-class",
		"x-claude-code-agent-id", "x-claude-code-parent-agent-id", "x-session-id", "session-id", "session_id",
		"x-client-request-id":
		return true
	}
	return strings.HasPrefix(lower, "x-stainless-")
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	if p.conns == nil {
		p.conns = map[net.Conn]struct{}{}
	}
	p.conns[c] = struct{}{}
	return true
}
func (p *Proxy) untrack(c net.Conn) { p.mu.Lock(); delete(p.conns, c); p.mu.Unlock() }
func (p *Proxy) Close() {
	p.mu.Lock()
	p.closed = true
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// Serve owns only the explicit local proxy. No hosts file, OS proxy or
// certificate trust is changed. Cancellation closes CONNECT tunnels too.
func Serve(ctx context.Context, c Config, p *Proxy) error {
	return serveReady(ctx, c, p, func() {})
}

func serveReady(ctx context.Context, c Config, p *Proxy, ready func()) error {
	defer p.Close()
	if err := c.Validate(); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	ready()
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: time.Minute, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			srv.Close()
			p.Close()
		case <-done:
		}
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return nil
	}
	return err
}
