package claudeproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/claudelogin"
	"github.com/yetone/magpie/internal/filelock"
)

// Preferences is the native application's editing document. Keys are accepted
// on input only; reads expose whether one is saved, never the credential itself.
type Preferences struct {
	Revision          string            `json:"revision"`
	Exists            bool              `json:"exists"`
	Enabled           bool              `json:"enabled"`
	Listen            string            `json:"listen"`
	MediaListen       string            `json:"mediaListen"`
	Models            map[string]string `json:"models"`
	GatewayUsage      bool              `json:"gatewayUsage"`
	UsageOrganization string            `json:"usageOrganization"`
	ImageUpstream     string            `json:"imageUpstream"`
	Servers           []ServerPreference `json:"servers"`
}

type ServerPreference struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	HasKey bool   `json:"hasKey"`
	Key    string `json:"key,omitempty"`
}

func defaultConfig() Config {
	return Config{Schema: 1, Listen: "127.0.0.1:3426", MediaListen: "127.0.0.1:3425",
		APICertDir: "certs/api", WebCertDir: "certs/web", Models: map[string]string{}, Upstreams: map[string]Upstream{}}
}

func preferenceConfig(path string) (Config, []byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultConfig(), nil, nil
	}
	if err != nil {
		return Config{}, nil, fmt.Errorf("cannot read the existing configuration")
	}
	c, err := Load(path)
	return c, b, err
}

func configRevision(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ReadPreferences(path string) (Preferences, error) {
	c, b, err := preferenceConfig(path)
	if err != nil {
		return Preferences{}, err
	}
	p := Preferences{Revision: configRevision(b), Exists: b != nil, Enabled: c.Enabled,
		Listen: c.Listen, MediaListen: c.MediaListen, Models: c.Models, GatewayUsage: c.GatewayUsage,
		UsageOrganization: c.UsageOrganization, ImageUpstream: c.ImageUpstream, Servers: []ServerPreference{}}
	for id, up := range c.Upstreams {
		_, _, err := upstream(path, c, id)
		p.Servers = append(p.Servers, ServerPreference{ID: id, Name: up.Name, URL: up.URL, HasKey: err == nil})
	}
	sort.Slice(p.Servers, func(i, j int) bool { return p.Servers[i].ID < p.Servers[j].ID })
	return p, nil
}

var serverID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func validKey(key string) bool {
	return len(key) >= 16 && len(key) <= 4096 && !strings.ContainsAny(key, "\r\n")
}

// SavePreferences serializes editors, rejects stale documents and stages new
// key files before a single atomic config replacement. Existing key files are
// immutable so in-flight requests and saved configuration backups remain valid.
func SavePreferences(ctx context.Context, path string, p Preferences) (Preferences, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	unlock, err := filelock.LockContext(ctx, filepath.Join(filepath.Dir(path), "preferences.lock"))
	if err != nil {
		return Preferences{}, fmt.Errorf("another configuration save is in progress")
	}
	defer unlock()
	c, before, err := preferenceConfig(path)
	if err != nil {
		return Preferences{}, err
	}
	if p.Revision != configRevision(before) {
		return Preferences{}, fmt.Errorf("configuration changed elsewhere; reload before saving")
	}
	if p.Listen != c.Listen || p.MediaListen != c.MediaListen {
		return Preferences{}, fmt.Errorf("listener changes also require updating Desktop; use the existing local endpoints")
	}
	oldUpstreams := c.Upstreams
	c.Upstreams = map[string]Upstream{}
	keys := map[string]string{}
	for _, s := range p.Servers {
		if !serverID.MatchString(s.ID) || c.Upstreams[s.ID].URL != "" {
			return Preferences{}, fmt.Errorf("server IDs must be unique and contain only letters, numbers, underscores or hyphens")
		}
		address := strings.TrimRight(strings.TrimSpace(s.URL), "/")
		if _, err := endpoint(address); err != nil {
			return Preferences{}, fmt.Errorf("server address must be an HTTP(S) origin, without /v1, credentials or a query")
		}
		key := strings.TrimSpace(s.Key)
		if key != "" && !validKey(key) {
			return Preferences{}, fmt.Errorf("gateway keys need 16–4096 characters without line breaks")
		}
		up := Upstream{URL: address, Name: strings.TrimSpace(s.Name), KeyFile: oldUpstreams[s.ID].KeyFile}
		if key == "" {
			old := c
			old.Upstreams = oldUpstreams
			if _, _, err := upstream(path, old, s.ID); err != nil {
				return Preferences{}, fmt.Errorf("enter a gateway key for each new or unreadable server")
			}
		} else {
			keys[s.ID] = key
			up.KeyFile = "keys/pending"
		}
		c.Upstreams[s.ID] = up
	}
	c.Enabled, c.Models = p.Enabled, p.Models
	if c.Models == nil {
		c.Models = map[string]string{}
	}
	c.GatewayUsage, c.UsageOrganization, c.ImageUpstream = p.GatewayUsage, strings.TrimSpace(p.UsageOrganization), p.ImageUpstream
	if err := c.validate(); err != nil {
		return Preferences{}, err
	}
	root := filepath.Dir(path)
	var created []string
	committed := false
	defer func() {
		if !committed {
			for _, name := range created {
				os.Remove(name)
			}
		}
	}()
	if err := os.MkdirAll(filepath.Join(root, "keys"), 0o700); err != nil {
		return Preferences{}, err
	}
	for id, key := range keys {
		f, err := os.CreateTemp(filepath.Join(root, "keys"), "gateway-")
		if err != nil {
			return Preferences{}, err
		}
		created = append(created, f.Name())
		_, writeErr := f.WriteString(key + "\n")
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			return Preferences{}, fmt.Errorf("cannot save private gateway key")
		}
		up := c.Upstreams[id]
		up.KeyFile, _ = filepath.Rel(root, f.Name())
		c.Upstreams[id] = up
	}
	// A new installation prepares constrained certificates but never trusts them.
	if before == nil {
		if _, err := claudelogin.EnsureAuthority(ctx, resolvePath(path, c.APICertDir)); err != nil {
			return Preferences{}, err
		}
	}
	if c.GatewayUsage && c.UsageOrganization != "" {
		if _, err := claudelogin.EnsureWebAuthority(ctx, resolvePath(path, c.WebCertDir)); err != nil {
			return Preferences{}, err
		}
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		return Preferences{}, err
	}
	// Preserve future fields that this version doesn't edit.
	merged := map[string]json.RawMessage{}
	if before != nil {
		if err := json.Unmarshal(before, &merged); err != nil {
			return Preferences{}, err
		}
	}
	var known map[string]json.RawMessage
	json.Unmarshal(encoded, &known)
	for _, field := range []string{"usageOrganization", "imageUpstream"} {
		delete(merged, field)
	}
	for k, v := range known {
		merged[k] = v
	}
	encoded, err = json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return Preferences{}, err
	}
	if before != nil {
		backup := filepath.Join(root, "backups", "config-"+p.Revision+".json")
		if err := privateWrite(backup, before); err != nil {
			return Preferences{}, fmt.Errorf("cannot back up the previous configuration")
		}
	}
	if err := privateWrite(path, append(encoded, '\n')); err != nil {
		return Preferences{}, err
	}
	committed = true
	return ReadPreferences(path)
}

func privateWrite(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".proxy-settings-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

// ProbeServer checks exactly the address the user entered. It never follows a
// redirect with the device key or includes a remote error body in diagnostics.
func ProbeServer(ctx context.Context, path string, s ServerPreference) ([]string, error) {
	u, err := endpoint(strings.TrimRight(strings.TrimSpace(s.URL), "/"))
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(s.Key)
	if key == "" {
		c, _, err := preferenceConfig(path)
		if err != nil {
			return nil, err
		}
		_, key, err = upstream(path, c, s.ID)
		if err != nil {
			return nil, fmt.Errorf("enter a gateway key before checking this server")
		}
	}
	if !validKey(key) {
		return nil, fmt.Errorf("invalid gateway key")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(u.String(), "/")+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to the server; check its address, network and TLS certificate")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model catalog returned HTTP %d; check the address and gateway key", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if err != nil || len(b) > 8<<20 {
		return nil, fmt.Errorf("model catalog is too large or unreadable")
	}
	var catalog struct {
		Data []struct { ID string `json:"id"` } `json:"data"`
	}
	if json.Unmarshal(b, &catalog) != nil || catalog.Data == nil {
		return nil, fmt.Errorf("server did not return a model catalog")
	}
	seen := map[string]bool{}
	models := []string{}
	for _, model := range catalog.Data {
		if model.ID != "" && !strings.ContainsAny(model.ID, "\r\n") && !seen[model.ID] {
			models = append(models, model.ID)
			seen[model.ID] = true
		}
	}
	sort.Strings(models)
	return models, nil
}
