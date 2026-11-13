package claudeproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPreferencesKeepKeysAndRejectStaleEdits(t *testing.T) {
	path, _ := fixtureConfig(t, "http://127.0.0.1:9001")
	before, _ := os.ReadFile(path)
	var raw map[string]any
	json.Unmarshal(before, &raw)
	raw["futureOption"] = "keep"
	before, _ = json.Marshal(raw)
	os.WriteFile(path, before, 0o600)
	p, err := ReadPreferences(path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	if bytes.Contains(b, []byte("device-fixture-key")) || !p.Servers[0].HasKey {
		t.Fatal("credential disclosed or missing")
	}
	p.Servers[0].Name = "Home"
	updated, err := SavePreferences(context.Background(), path, p)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision == p.Revision || updated.Servers[0].Name != "Home" {
		t.Fatal("edit was not saved")
	}
	c, _ := Load(path)
	_, key, err := upstream(path, c, "nas")
	if err != nil || key != "device-fixture-key" {
		t.Fatal("blank key did not preserve credential")
	}
	if _, err := SavePreferences(context.Background(), path, p); err == nil {
		t.Fatal("stale editor overwrote saved settings")
	}
	result, _ := os.ReadFile(path)
	if !bytes.Contains(result, []byte(`"futureOption": "keep"`)) {
		t.Fatal("future option lost")
	}
	backup, err := os.ReadFile(filepath.Join(filepath.Dir(path), "backups", "config-"+p.Revision+".json"))
	if err != nil || !bytes.Equal(backup, before) {
		t.Fatal("previous config not backed up")
	}
}

func TestPreferencesStageKeysAndValidateBeforeWrite(t *testing.T) {
	path, original := fixtureConfig(t, "http://127.0.0.1:9001")
	p, _ := ReadPreferences(path)
	p.Servers[0].Key = "new-private-device-key"
	p.Models["claude-new"] = "missing/model"
	if _, err := SavePreferences(context.Background(), path, p); err == nil {
		t.Fatal("dangling mapping accepted")
	}
	unchanged, _ := Load(path)
	if unchanged.Upstreams["nas"].KeyFile != original.Upstreams["nas"].KeyFile {
		t.Fatal("invalid edit changed key")
	}
	delete(p.Models, "claude-new")
	updated, err := SavePreferences(context.Background(), path, p)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Servers[0].Key != "" {
		t.Fatal("saved key echoed back")
	}
	c, _ := Load(path)
	if c.Upstreams["nas"].KeyFile == original.Upstreams["nas"].KeyFile {
		t.Fatal("in-flight key overwritten")
	}
	old, _ := os.ReadFile(resolvePath(path, original.Upstreams["nas"].KeyFile))
	if strings.TrimSpace(string(old)) != "device-fixture-key" {
		t.Fatal("old key was changed")
	}
	st, _ := os.Stat(resolvePath(path, c.Upstreams["nas"].KeyFile))
	if st.Mode().Perm() != 0o600 {
		t.Fatal("key is not private")
	}
	st, _ = os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatal("config is not private")
	}
	updated.Servers = nil
	if _, err := SavePreferences(context.Background(), path, updated); err == nil {
		t.Fatal("referenced server removed")
	}
}

func TestPreferencesCorruptFileIsNotAnEmptyInstallation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte("broken"), 0o600)
	if _, err := ReadPreferences(path); err == nil {
		t.Fatal("corrupt config treated as empty")
	}
	if _, err := SavePreferences(context.Background(), path, Preferences{}); err == nil {
		t.Fatal("corrupt config overwritten")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "broken" {
		t.Fatal("original evidence lost")
	}
}

func TestProbeServerUsesCatalogWithoutFollowingRedirects(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	nas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer device-fixture-key" {
			t.Error("wrong catalog request")
		}
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer nas.Close()
	path, _ := fixtureConfig(t, nas.URL)
	p, _ := ReadPreferences(path)
	if _, err := ProbeServer(context.Background(), path, p.Servers[0]); err == nil {
		t.Fatal("redirect accepted")
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("credential followed a redirect")
	}
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer temporary-device-key" {
			t.Error("unsaved key not used")
		}
		w.Write([]byte(`{"data":[{"id":"team/opus"},{"id":"team/opus"},{"id":"codex/gpt-image-2"}]}`))
	}))
	defer good.Close()
	models, err := ProbeServer(context.Background(), path, ServerPreference{URL: good.URL, Key: "temporary-device-key"})
	if err != nil || len(models) != 2 {
		t.Fatalf("catalog: %v, %v", models, err)
	}
	unchanged, _ := ReadPreferences(path)
	if unchanged.Servers[0].URL != nas.URL {
		t.Fatal("connection check saved draft settings")
	}
}
