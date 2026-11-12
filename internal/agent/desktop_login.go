package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/claudelogin"
	"github.com/yetone/magpie/internal/desktopdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
)

const loginProfileID = "00000000-0000-4000-8000-6d706c6f676e"

// SetDesktopLogin serializes CLI and GUI changes, with the same trust and
// rollback checks. It never installs a certificate or restarts Desktop.
func SetDesktopLogin(next claudelogin.Config) error {
	if err := next.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	unlock, err := filelock.LockContext(ctx, claudelogin.Path()+".edit.lock")
	if err != nil {
		return err
	}
	defer unlock()
	old, err := claudelogin.Load()
	if err != nil {
		return err
	}
	if next.Enabled && next.GatewayUsage && next.UsageOrganization != "" && !claudelogin.ReadWebCertificate().Trusted {
		return fmt.Errorf("trust the separate claude.ai quota CA before enabling organization usage")
	}
	if next.Enabled && !claudelogin.ReadCertificate().Trusted {
		return fmt.Errorf("trust this installation's public CA before enabling Claude login routing")
	}
	if next.Listen != old.Listen {
		busy, err := claudelogin.ListenerBusy()
		if err != nil {
			return err
		}
		if old.Enabled || busy || DesktopLoginWired() {
			return fmt.Errorf("disable Claude login routing and restart Magpie before changing its proxy port")
		}
	}
	wired := DesktopLoginWired()
	if next.Enabled && !wired {
		if err = DesktopLoginProxy(true, "http://"+next.Listen); err != nil {
			return err
		}
	}
	if !next.Enabled && wired {
		if err = DesktopLoginProxy(false, ""); err != nil {
			return err
		}
	}
	if err = claudelogin.Save(next); err != nil {
		if !wired && next.Enabled {
			return errors.Join(err, DesktopLoginProxy(false, ""))
		}
		if wired && !next.Enabled {
			return errors.Join(err, DesktopLoginProxy(true, "http://"+old.Listen))
		}
		return err
	}
	return nil
}

type loginEdit struct {
	Path   string          `json:"path"`
	Key    string          `json:"key"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after"`
}

func loginJournalPath() string { return filepath.Join(appdir.Config(), "claude-login-wiring.json") }

// DesktopLoginProxy uses Desktop 3.6556.2's native first-party egress
// setting. It doesn't replace or sign the app, read credentials, or enable
// any feature flag. A journal is durable before the first config change.
func DesktopLoginProxy(on bool, proxy string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("keep Claude login wiring is currently verified only for macOS")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if on {
		for _, policy := range []string{"/Library/Managed Preferences/com.anthropic.claudefordesktop.plist", filepath.Join("/Library/Managed Preferences", os.Getenv("USER"), "com.anthropic.claudefordesktop.plist")} {
			if _, err := os.Stat(policy); err == nil {
				return fmt.Errorf("Claude is managed by %s; configure its egress proxy through that policy", policy)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return desktopLoginProxyAt(desktopSets(desktopdir.OS, home, os.Getenv)[0], on, proxy)
}

func desktopLoginProxyAt(p desktopPaths, on bool, proxy string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	unlock, err := filelock.LockContext(ctx, loginJournalPath()+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	b, err := os.ReadFile(loginJournalPath())
	var journal []loginEdit
	if err == nil {
		if err = json.Unmarshal(b, &journal); err != nil {
			return fmt.Errorf("Claude login rollback journal is unreadable: %w", err)
		}
		if err = validateLoginJournal(p, journal); err != nil {
			return err
		}
		if on {
			return fmt.Errorf("Claude login proxy is already wired; disable it before changing the listener")
		}
		return restoreLoginEdits(journal)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !on {
		return nil
	}
	// Remote policy bootstraps can override the local profile. Never remove
	// an organization's managed policy just to make the proxy take effect.
	if _, ok := edit.GetJSONRaw(p.meta, "hybridPointer"); ok {
		return fmt.Errorf("Claude uses a managed bootstrap profile; configure its egress proxy through that policy")
	}
	profile := filepath.Join(p.library, loginProfileID+".json")
	changes := []struct {
		path, key string
		value     any
	}{
		{profile, "egressProxyUrl", proxy},
		{p.meta, "appliedId", loginProfileID},
		{p.config, "deploymentMode", "1p"},
		{p.config3p, "deploymentMode", "1p"},
	}
	for _, c := range changes {
		raw, err := edit.Read(c.path)
		if err != nil {
			return err
		}
		// PatchJSON validates a malformed file without writing it.
		if _, err = edit.PatchJSON(raw, []edit.KV{{Path: c.key, Value: c.value}}, nil); err != nil {
			return err
		}
		before, had := edit.GetJSONRaw(c.path, c.key)
		after, _ := json.Marshal(c.value)
		item := loginEdit{Path: c.path, Key: c.key, After: after}
		if had {
			item.Before = json.RawMessage(before)
		}
		journal = append(journal, item)
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(loginJournalPath()), 0o700); err != nil {
		return err
	}
	// Journal values contain only proxy/mode/profile keys, no login tokens.
	f, err := os.OpenFile(loginJournalPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	for _, c := range journal {
		if err = edit.SetJSON(c.Path, edit.KV{Path: c.Key, Value: c.After}); err != nil {
			rollback := restoreLoginEdits(journal)
			return errors.Join(err, rollback)
		}
	}
	return nil
}

// Only the four keys this version owns may be restored. A corrupt journal
// must never become a general file writer, or be silently discarded.
func validateLoginJournal(p desktopPaths, journal []loginEdit) error {
	expected := map[string]string{
		filepath.Join(p.library, loginProfileID+".json"): "egressProxyUrl",
		p.meta: "appliedId", p.config: "deploymentMode", p.config3p: "deploymentMode",
	}
	if len(journal) != len(expected) {
		return fmt.Errorf("Claude login rollback journal has an unexpected number of edits; it was kept")
	}
	for _, item := range journal {
		key, ok := expected[item.Path]
		if !ok || key != item.Key || !json.Valid(item.After) || (len(item.Before) > 0 && !json.Valid(item.Before)) {
			return fmt.Errorf("Claude login rollback journal contains an unexpected edit; it was kept")
		}
		delete(expected, item.Path)
	}
	return nil
}

func restoreLoginEdits(journal []loginEdit) error {
	// Validate all reads before restoring any key. A new value from the user
	// is preserved and reported, rather than restoring over it.
	for _, c := range journal {
		raw, err := edit.Read(c.Path)
		if err != nil {
			return err
		}
		if _, err = edit.PatchJSON(raw, nil, nil); err != nil {
			return err
		}
		current, had := edit.GetJSONRaw(c.Path, c.Key)
		if had && current != string(c.After) && current != string(c.Before) {
			return fmt.Errorf("Claude login rollback conflict in %s (%s); your value was kept", c.Path, c.Key)
		}
	}
	for i := len(journal) - 1; i >= 0; i-- {
		c := journal[i]
		current, had := edit.GetJSONRaw(c.Path, c.Key)
		if !had || current != string(c.After) {
			continue
		}
		var err error
		if len(c.Before) == 0 {
			err = edit.DelJSON(c.Path, c.Key)
		} else {
			err = edit.SetJSON(c.Path, edit.KV{Path: c.Key, Value: c.Before})
		}
		if err != nil {
			return err
		}
	}
	return os.Remove(loginJournalPath())
}

func DesktopLoginWired() bool { _, err := os.Stat(loginJournalPath()); return err == nil }

func desktopLoginApplied(p desktopPaths) bool {
	id, _ := edit.GetJSON(p.meta, "appliedId")
	return id == loginProfileID
}

func desktopLoginCheck(p desktopPaths) string {
	c, err := claudelogin.Load()
	if err != nil {
		return err.Error()
	}
	mode, _ := edit.GetJSON(p.config, "deploymentMode")
	mode3p, _ := edit.GetJSON(p.config3p, "deploymentMode")
	proxy, _ := edit.GetJSON(filepath.Join(p.library, loginProfileID+".json"), "egressProxyUrl")
	if !c.Enabled || mode != "1p" || mode3p != "1p" || proxy != "http://"+c.Listen {
		return "Claude's native login proxy configuration changed; check Keep Claude signed in in Settings"
	}
	return ""
}
