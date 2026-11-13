// Package claudeproxy is a standalone local client for a remote Magpie.
// It imports no provider registry, inference gateway, account manager or GUI.
package claudeproxy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/yetone/magpie/internal/claudelogin"
)

type Upstream struct {
	URL     string `json:"url"`
	KeyFile string `json:"keyFile"`
}

type Config struct {
	Schema            int                 `json:"schema"`
	Enabled           bool                `json:"enabled"`
	Listen            string              `json:"listen"`
	Models            map[string]string   `json:"models"`
	GatewayUsage      bool                `json:"gatewayUsage"`
	UsageOrganization string              `json:"usageOrganization,omitempty"`
	APICertDir        string              `json:"apiCertDir"`
	WebCertDir        string              `json:"webCertDir,omitempty"`
	Upstreams         map[string]Upstream `json:"upstreams"`
	ImageUpstream     string              `json:"imageUpstream,omitempty"`
	MediaListen       string              `json:"mediaListen,omitempty"`
}

func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claude-login-proxy", "config.json")
}

func (c Config) ProxyConfig() claudelogin.Config {
	return claudelogin.Config{Enabled: c.Enabled, Listen: c.Listen, Models: c.Models,
		GatewayUsage: c.GatewayUsage, UsageOrganization: c.UsageOrganization, RemoteInference: true}
}

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read proxy configuration: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("invalid proxy configuration")
	}
	if c.Schema != 1 || c.APICertDir == "" {
		return c, fmt.Errorf("configuration needs schema 1 and an API certificate directory")
	}
	if err := c.ProxyConfig().Validate(); err != nil {
		return c, err
	}
	for _, target := range c.Models {
		id, model, ok := strings.Cut(target, "/")
		if !ok || model == "" || c.Upstreams[id].URL == "" {
			return c, fmt.Errorf("a model mapping has no configured upstream")
		}
	}
	for _, up := range c.Upstreams {
		if _, err := endpoint(up.URL); err != nil || up.KeyFile == "" {
			return c, fmt.Errorf("upstream needs an HTTP(S) origin and a private key file")
		}
	}
	if c.ImageUpstream != "" && c.Upstreams[c.ImageUpstream].URL == "" {
		return c, fmt.Errorf("image upstream is not configured")
	}
	if c.MediaListen != "" {
		host, port, err := net.SplitHostPort(c.MediaListen)
		if err != nil || port == "0" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || c.MediaListen == c.Listen {
			return c, fmt.Errorf("media relay needs a separate loopback port")
		}
	}
	return c, nil
}

func endpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("invalid upstream origin")
	}
	return u, nil
}

func resolvePath(config, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(config), p)
}

func upstream(config string, c Config, id string) (*url.URL, string, error) {
	up, ok := c.Upstreams[id]
	if !ok {
		return nil, "", fmt.Errorf("upstream is not configured")
	}
	u, err := endpoint(up.URL)
	if err != nil {
		return nil, "", err
	}
	p := resolvePath(config, up.KeyFile)
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return nil, "", fmt.Errorf("gateway key file must be private (0600)")
	}
	b, err := os.ReadFile(p)
	k := strings.TrimSpace(string(b))
	if err != nil || len(k) < 16 || strings.ContainsAny(k, "\r\n") {
		return nil, "", fmt.Errorf("gateway key file is unreadable or invalid")
	}
	return u, k, nil
}
