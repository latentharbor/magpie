package claudeproxy

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/claudelogin"
	"github.com/yetone/magpie/internal/filelock"
)

type activity struct {
	mu       sync.Mutex
	active   int
	draining bool
	changed  chan struct{}
}

func (a *activity) change() {
	if a.changed != nil {
		close(a.changed)
	}
	a.changed = make(chan struct{})
}

func (a *activity) begin() (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.draining {
		return nil, fmt.Errorf("proxy is stopping")
	}
	a.active++
	a.change()
	var once sync.Once
	return func() { once.Do(func() { a.mu.Lock(); a.active--; a.change(); a.mu.Unlock() }) }, nil
}

func (a *activity) drain(ctx context.Context) error {
	a.mu.Lock()
	a.draining = true
	for a.active != 0 {
		ch := a.changed
		a.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		a.mu.Lock()
	}
	a.mu.Unlock()
	return nil
}

func (a *activity) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.active }

type countedTransport struct {
	base http.RoundTripper
	jobs *activity
}

type countedBody struct {
	io.ReadCloser
	end func()
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.end()
	}
	return n, err
}
func (b *countedBody) Close() error { defer b.end(); return b.ReadCloser.Close() }

func (t countedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	end, err := t.jobs.begin()
	if err != nil {
		return nil, err
	}
	res, err := t.base.RoundTrip(r)
	if err != nil {
		end()
		return nil, err
	}
	res.Body = &countedBody{ReadCloser: res.Body, end: end}
	return res, nil
}

type certCache struct {
	mu     sync.Mutex
	api    string
	web    string
	loaded map[string]*claudelogin.Authority
	trust  func(string, string) claudelogin.CertificateStatus
}

func (c *certCache) get(ctx context.Context, host string) (*claudelogin.Authority, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.loaded[host]; a != nil {
		leaf, err := x509.ParseCertificate(a.Certificate.Certificate[0])
		if err == nil && time.Until(leaf.NotAfter) > 24*time.Hour {
			return a, nil
		}
	}
	dir, makeAuthority := c.api, claudelogin.EnsureAuthority
	if host == claudelogin.UsageHost {
		dir, makeAuthority = c.web, claudelogin.EnsureWebAuthority
	} else if host != claudelogin.Host {
		return nil, fmt.Errorf("unsupported certificate scope")
	}
	if dir == "" || !c.trust(dir, host).Trusted {
		return nil, fmt.Errorf("the existing %s certificate needs system SSL trust", host)
	}
	a, err := makeAuthority(ctx, dir)
	if err != nil {
		return nil, err
	}
	if c.loaded == nil {
		c.loaded = map[string]*claudelogin.Authority{}
	}
	c.loaded[host] = a
	return a, nil
}

// Run owns only the TLS proxy, the image compatibility relay and a private
// status socket. No provider discovery, login refresh or seat controller runs.
func Run(ctx context.Context, path, version string) error {
	return run(ctx, path, version, claudelogin.ReadCertificateAt)
}

func run(ctx context.Context, path, version string, trust func(string, string) claudelogin.CertificateStatus) error {
	initial, err := Load(path)
	if err != nil {
		return err
	}
	lockCtx, unlockWait := context.WithTimeout(ctx, 2*time.Second)
	defer unlockWait()
	unlock, err := filelock.LockContext(lockCtx, filepath.Join(filepath.Dir(path), "service.lock"))
	if err != nil {
		return fmt.Errorf("another proxy owns this configuration")
	}
	defer unlock()
	load := func() (Config, error) {
		c, err := Load(path)
		if err == nil && (c.Listen != initial.Listen || c.MediaListen != initial.MediaListen || c.APICertDir != initial.APICertDir || c.WebCertDir != initial.WebCertDir) {
			return c, fmt.Errorf("restart the proxy after changing listeners or certificate paths")
		}
		return c, err
	}
	certs := &certCache{api: resolvePath(path, initial.APICertDir), trust: trust}
	if initial.WebCertDir != "" {
		certs.web = resolvePath(path, initial.WebCertDir)
	}
	a, err := certs.get(ctx, claudelogin.Host)
	if err != nil {
		return err
	}
	jobs := &activity{}
	var ready atomic.Bool
	transport := &http.Transport{ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	tracked := countedTransport{base: transport, jobs: jobs}
	p := &claudelogin.Proxy{Authority: a, AuthorityForHost: certs.get, Local: tracked,
		Official: countedTransport{base: http.DefaultTransport, jobs: jobs},
		Config:   func() (claudelogin.Config, error) { c, err := load(); return c.ProxyConfig(), err },
		Remote: claudelogin.RemoteOptions{Begin: func() func() { return func() {} }, Resolve: func(ctx context.Context, selected string) (*claudelogin.RemoteTarget, error) {
			c, err := load()
			if err != nil || !c.Enabled {
				return nil, fmt.Errorf("proxy configuration unavailable")
			}
			present := false
			for _, v := range c.Models {
				present = present || v == selected
			}
			if !present {
				return nil, fmt.Errorf("mapping was removed")
			}
			id, model, _ := strings.Cut(selected, "/")
			u, key, err := upstream(path, c, id)
			if err != nil {
				return nil, err
			}
			return &claudelogin.RemoteTarget{URL: u, Key: key, Model: model}, nil
		}}}
	if initial.GatewayUsage && initial.UsageOrganization != "" {
		p.WebAuthority, err = certs.get(ctx, claudelogin.UsageHost)
		if err != nil {
			return err
		}
	}
	socket := filepath.Join(filepath.Dir(path), "status.sock")
	_ = os.Remove(socket) // service.lock establishes that no other owner serves it
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return err
	}
	control := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"version": version, "running": ready.Load(), "activeRequests": jobs.count(), "listen": initial.Listen})
	})}
	go control.Serve(ln)
	defer control.Close()
	serveCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if initial.MediaListen != "" {
		mediaLn, err := net.Listen("tcp", initial.MediaListen)
		if err != nil {
			return err
		}
		media := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: mediaRelay(path, tracked)}
		go media.Serve(mediaLn)
		defer media.Close()
	}
	done := make(chan error, 1)
	go func() {
		done <- claudelogin.ServeReady(serveCtx, initial.ProxyConfig(), p, func() { ready.Store(true) })
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		drain, stop := context.WithTimeout(context.Background(), 50*time.Second)
		defer stop()
		err := jobs.drain(drain)
		cancel()
		<-done
		return err
	}
}

func mediaRelay(path string, transport http.RoundTripper) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() || r.Header.Get("Origin") != "" {
			http.Error(w, "local clients only", 403)
			return
		}
		allowed := r.URL.Path == "/mcp/magpie/image" || r.URL.Path == "/v1/images/generations" || r.URL.Path == "/v1/images/edits" || r.URL.Path == "/v1/models" || r.URL.Path == "/v1/videos" || strings.HasPrefix(r.URL.Path, "/v1/videos/")
		if !allowed || (r.Method != "POST" && r.Method != "GET") {
			http.NotFound(w, r)
			return
		}
		c, err := Load(path)
		if err != nil {
			http.Error(w, "proxy configuration unavailable", 503)
			return
		}
		u, key, err := upstream(path, c, c.ImageUpstream)
		if err != nil {
			http.Error(w, "image gateway unavailable", 503)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
		proxy := &httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = u.Host
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Set("Authorization", "Bearer "+key)
		}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "image gateway unavailable", 502)
		}}
		proxy.ServeHTTP(w, r)
	})
}

func Status(ctx context.Context, path string) (map[string]any, error) {
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(filepath.Dir(path), "status.sock"))
	}}
	defer t.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://proxy/status", nil)
	res, err := (&http.Client{Transport: t, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("standalone proxy is not running")
	}
	defer res.Body.Close()
	var v map[string]any
	err = json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&v)
	return v, err
}
