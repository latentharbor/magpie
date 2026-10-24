package claudelogin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
)

// webUsage leaves the official login and subscription responses unchanged.
// Only a successful usage read for the explicitly configured organization is
// transformed. The destination and its credential scope remain claude.ai.
func (p *Proxy) webUsage(w http.ResponseWriter, r *http.Request) {
	if r.Host != UsageHost && r.Host != UsageHost+":443" {
		http.Error(w, "unexpected authority", 403)
		return
	}
	managed := false
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/organizations/") && strings.HasSuffix(r.URL.Path, "/usage") {
		cfg, err := p.Config()
		if err != nil || cfg.Validate() != nil {
			http.Error(w, "Claude login configuration cannot be read", 503)
			return
		}
		managed = cfg.Enabled && cfg.GatewayUsage && cfg.UsageOrganization != "" && r.URL.Path == "/api/organizations/"+cfg.UsageOrganization+"/usage"
	}
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.Out.URL.Scheme = "https"
		pr.Out.URL.Host = UsageHost
		pr.Out.Host = UsageHost
		if managed {
			pr.Out.Header.Del("If-None-Match")
			pr.Out.Header.Del("If-Modified-Since")
			pr.Out.Header.Set("Accept-Encoding", "gzip")
		}
	}, Transport: p.Official, ModifyResponse: func(res *http.Response) error {
		if !managed || res.StatusCode != http.StatusOK {
			return nil
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
		if err != nil || len(raw) > maxBody {
			return fmt.Errorf("usage reply too large or unreadable")
		}
		switch res.Header.Get("Content-Encoding") {
		case "", "identity":
		case "gzip":
			gz, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				return err
			}
			defer gz.Close()
			raw, err = io.ReadAll(io.LimitReader(gz, maxBody+1))
			if err != nil || len(raw) > maxBody {
				return fmt.Errorf("decoded usage reply too large or unreadable")
			}
		default:
			return fmt.Errorf("unsupported usage encoding")
		}
		b, err := gatewayWebUsage(raw)
		if err != nil {
			return err
		}
		res.Body = io.NopCloser(bytes.NewReader(b))
		res.ContentLength = int64(len(b))
		res.Header.Set("Content-Length", strconv.Itoa(len(b)))
		res.Header.Set("Cache-Control", "no-store")
		for _, key := range []string{"Content-Encoding", "ETag", "Content-MD5", "Digest"} {
			res.Header.Del(key)
		}
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "official usage reply unavailable", http.StatusBadGateway)
	}}
	proxy.ServeHTTP(w, r)
}

func gatewayWebUsage(raw []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("invalid official usage reply")
	}
	for _, key := range []string{"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet"} {
		var window map[string]json.RawMessage
		if json.Unmarshal(obj[key], &window) == nil && window != nil && window["utilization"] != nil {
			window["utilization"] = json.RawMessage("0")
			obj[key], _ = json.Marshal(window)
		}
	}
	var limits []map[string]json.RawMessage
	if json.Unmarshal(obj["limits"], &limits) == nil {
		for _, limit := range limits {
			var kind string
			json.Unmarshal(limit["kind"], &kind)
			if (kind == "session" || kind == "weekly_all" || kind == "weekly_scoped") && limit["percent"] != nil {
				limit["percent"] = json.RawMessage("0")
				if limit["severity"] != nil {
					limit["severity"] = json.RawMessage(`"normal"`)
				}
			}
		}
		if limits != nil {
			obj["limits"], _ = json.Marshal(limits)
		}
	}
	obj["magpie_gateway_managed"] = json.RawMessage("true")
	return json.Marshal(obj)
}

// Existing TLS tunnels retain their authority across configuration disable until
// Desktop restarts. Newly enabled quota routing loads its separately trusted CA
// on the next CONNECT, without restarting a busy inference gateway.
func (p *Proxy) usageAuthority(ctx context.Context) (*Authority, error) {
	p.mu.Lock()
	a := p.WebAuthority
	p.mu.Unlock()
	if a != nil {
		return a, nil
	}
	c, err := p.Config()
	if err != nil {
		return nil, nil // Keep unrelated official control-plane tunnels opaque.
	}
	if !c.Enabled || !c.GatewayUsage || c.UsageOrganization == "" {
		return nil, nil
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if !ReadWebCertificate().Trusted {
		return nil, fmt.Errorf("separate claude.ai CA is not trusted")
	}
	a, err = EnsureWebAuthority(ctx, WebCertDir())
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.WebAuthority == nil {
		p.WebAuthority = a
	}
	a = p.WebAuthority
	p.mu.Unlock()
	return a, nil
}
