package claudeproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/claudelogin"
)

// Initialize prepares a fresh client. It generates certificates but never
// installs trust or changes Desktop; those are explicit installation steps.
func Initialize(ctx context.Context, destination, address, key, providerID, organization string) error {
	u, err := endpoint(address)
	if err != nil {
		return err
	}
	if len(key) < 16 || strings.ContainsAny(key, "\r\n") {
		return fmt.Errorf("invalid gateway key")
	}
	root := filepath.Dir(destination)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		return fmt.Errorf("configuration directory already exists or cannot be inspected")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(u.String(), "/")+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot read the NAS model catalog")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("NAS catalog returned HTTP %d", res.StatusCode)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&list) != nil {
		return fmt.Errorf("invalid NAS model catalog")
	}
	byProvider := map[string]map[string]string{}
	for _, m := range list.Data {
		id, model, ok := strings.Cut(m.ID, "/")
		if !ok || !strings.HasPrefix(model, "claude-") {
			continue
		}
		if byProvider[id] == nil {
			byProvider[id] = map[string]string{}
		}
		byProvider[id][model] = "nas/" + m.ID
	}
	if providerID == "" {
		for id := range byProvider {
			if providerID != "" {
				return fmt.Errorf("NAS has several Claude providers; choose one with --provider")
			}
			providerID = id
		}
	}
	models := byProvider[providerID]
	if len(models) == 0 {
		return fmt.Errorf("the selected NAS provider has no Claude models")
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(root), ".claude-proxy-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	c := Config{Schema: 1, Enabled: true, Listen: "127.0.0.1:3426", Models: models, GatewayUsage: true, UsageOrganization: organization, APICertDir: "certs/api", WebCertDir: "certs/web", Upstreams: map[string]Upstream{"nas": {URL: strings.TrimRight(u.String(), "/"), KeyFile: "keys/nas"}}, ImageUpstream: "nas", MediaListen: "127.0.0.1:3425"}
	if err := c.ProxyConfig().Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(stage, "keys"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "keys/nas"), []byte(key+"\n"), 0o600); err != nil {
		return err
	}
	if _, err := claudelogin.EnsureAuthority(ctx, filepath.Join(stage, c.APICertDir)); err != nil {
		return err
	}
	if organization != "" {
		if _, err := claudelogin.EnsureWebAuthority(ctx, filepath.Join(stage, c.WebCertDir)); err != nil {
			return err
		}
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, filepath.Base(destination)), append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(stage, root)
}

func Certificates(path string) (map[string]claudelogin.CertificateStatus, error) {
	c, err := Load(path)
	if err != nil {
		return nil, err
	}
	out := map[string]claudelogin.CertificateStatus{"api": claudelogin.ReadCertificateAt(resolvePath(path, c.APICertDir), claudelogin.Host)}
	if c.GatewayUsage && c.UsageOrganization != "" {
		out["web"] = claudelogin.ReadCertificateAt(resolvePath(path, c.WebCertDir), claudelogin.UsageHost)
	}
	return out, nil
}
