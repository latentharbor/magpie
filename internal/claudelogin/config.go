// Package claudelogin routes explicitly selected Claude inference through
// Magpie without changing Claude Desktop's authentication or application files.
package claudelogin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/edit"
)

const Host = "api.anthropic.com"
const UsageHost = "claude.ai"

type Config struct {
	Enabled           bool              `json:"enabled"`
	Listen            string            `json:"listen"`
	Models            map[string]string `json:"models"`
	UsageOrganization string            `json:"usageOrganization,omitempty"`
	GatewayUsage      bool              `json:"gatewayUsage,omitempty"`
}

func WebCertDir() string { return filepath.Join(appdir.Config(), "claude-login-web-ca") }

func Path() string    { return filepath.Join(appdir.Config(), "claude-login.json") }
func CertDir() string { return filepath.Join(appdir.Config(), "claude-login-ca") }

func Load() (Config, error) {
	c := Config{Listen: "127.0.0.1:3426", Models: map[string]string{}}
	b, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("Claude login configuration: %w", err)
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.UsageOrganization != "" {
		parts := strings.Split(c.UsageOrganization, "-")
		sizes := []int{8, 4, 4, 4, 12}
		if len(parts) != len(sizes) {
			return fmt.Errorf("usage organization must be a UUID")
		}
		for i, part := range parts {
			if len(part) != sizes[i] {
				return fmt.Errorf("usage organization must be a UUID")
			}
			for _, r := range part {
				if !strings.ContainsRune("0123456789abcdef", r) {
					return fmt.Errorf("usage organization must be a lowercase UUID")
				}
			}
		}
	}

	host, port, err := net.SplitHostPort(c.Listen)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("Claude login proxy must listen on a loopback IP and fixed port")
	}
	if _, err := net.ResolveTCPAddr("tcp", c.Listen); err != nil {
		return err
	}
	for model, target := range c.Models {
		if strings.TrimSpace(model) != model || model == "" || strings.ContainsAny(model, "*?\r\n") {
			return fmt.Errorf("Claude login model must be an exact model ID")
		}
		if target == "" || !strings.Contains(target, "/") || strings.ContainsAny(target, "\r\n") {
			return fmt.Errorf("Claude login target must be a Magpie provider/model or group/id")
		}
	}
	if c.Enabled && len(c.Models) == 0 {
		return fmt.Errorf("select at least one model before enabling Claude login proxy")
	}
	return nil
}

func Save(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := edit.WriteAtomic(Path(), append(b, '\n')); err != nil {
		return err
	}
	return os.Chmod(Path(), 0o600)
}

func GatewayURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("Claude login gateway must be a plain HTTP loopback origin")
	}
	return u, nil
}
