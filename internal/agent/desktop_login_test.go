package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/claudelogin"
	"github.com/yetone/magpie/internal/edit"
)

func loginFixture(t *testing.T) desktopPaths {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	// appdir uses UserConfigDir on macOS, which is derived from HOME.
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return desktopPathsOf(filepath.Join(dir, "Claude"), filepath.Join(dir, "Claude-3p"))
}

func TestDesktopLoginNativeProfileRoundTrip(t *testing.T) {
	p := loginFixture(t)
	for path, value := range map[string]string{p.config: `{"deploymentMode":"3p","unrelated":true}`, p.config3p: `{"deploymentMode":"3p","mcpServers":{"kept":{}}}`, p.meta: `{"appliedId":"previous-profile","entries":[{"id":"previous-profile","name":"existing"}]}`} {
		if err := edit.WriteAtomic(path, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := desktopLoginProxyAt(p, true, "http://127.0.0.1:3426"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{p.config, p.config3p} {
		if value, _ := edit.GetJSONRaw(path, "deploymentMode"); value != `"1p"` {
			t.Fatalf("native login not preserved: %s", value)
		}
	}
	profile := filepath.Join(p.library, loginProfileID+".json")
	raw, _ := os.ReadFile(profile)
	if !strings.Contains(string(raw), `"egressProxyUrl"`) || strings.Contains(string(raw), "apiKey") || strings.Contains(string(raw), "inferenceProvider") {
		t.Fatalf("not proxy-only: %s", raw)
	}
	if err := edit.SetJSON(p.config, edit.KV{Path: "newSetting", Value: "keep me"}); err != nil {
		t.Fatal(err)
	}
	if err := desktopLoginProxyAt(p, false, ""); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{p.config, p.config3p} {
		if value, _ := edit.GetJSONRaw(path, "deploymentMode"); value != `"3p"` {
			t.Fatalf("mode not restored: %s", value)
		}
	}
	if value, _ := edit.GetJSONRaw(p.meta, "appliedId"); value != `"previous-profile"` {
		t.Fatal(value)
	}
	if value, _ := edit.GetJSONRaw(p.config, "newSetting"); value != `"keep me"` {
		t.Fatal("unrelated change lost")
	}
	if DesktopLoginWired() {
		t.Fatal("journal not cleared")
	}
}

func TestDesktopLoginPreflightAndRollbackConflict(t *testing.T) {
	p := loginFixture(t)
	if err := edit.WriteAtomic(p.config, []byte(`{"broken":`)); err != nil {
		t.Fatal(err)
	}
	if err := desktopLoginProxyAt(p, true, "http://127.0.0.1:3426"); err == nil {
		t.Fatal("malformed config accepted")
	}
	if DesktopLoginWired() {
		t.Fatal("preflight wrote journal")
	}
	if _, err := os.Stat(p.meta); !os.IsNotExist(err) {
		t.Fatal("preflight modified native profile")
	}
	if err := edit.WriteAtomic(p.config, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := edit.WriteAtomic(p.meta, []byte(`{"hybridPointer":{"bootstrapUrl":"https://managed.invalid"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := desktopLoginProxyAt(p, true, "http://127.0.0.1:3426"); err == nil {
		t.Fatal("managed profile overwritten")
	}
	if err := edit.WriteAtomic(p.meta, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := desktopLoginProxyAt(p, true, "http://127.0.0.1:3426"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetJSON(p.config, edit.KV{Path: "deploymentMode", Value: "user-edited"}); err != nil {
		t.Fatal(err)
	}
	if err := desktopLoginProxyAt(p, false, ""); err == nil {
		t.Fatal("rollback overwrote user change")
	}
	if value, _ := edit.GetJSONRaw(p.config, "deploymentMode"); value != `"user-edited"` {
		t.Fatal(value)
	}
	if !DesktopLoginWired() {
		t.Fatal("conflicting journal lost")
	}
}

func TestDesktopLoginRejectsCorruptJournal(t *testing.T) {
	p := loginFixture(t)
	for _, body := range []string{`[]`, `null`, `[{"path":"/tmp/unowned","key":"anything","after":true}]`, `broken`} {
		if err := edit.WriteAtomic(loginJournalPath(), []byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := desktopLoginProxyAt(p, false, ""); err == nil {
			t.Fatalf("accepted journal %s", body)
		}
		after, _ := os.ReadFile(loginJournalPath())
		if string(after) != body {
			t.Fatal("corrupt journal was discarded")
		}
	}
}

func TestNativeLoginDoesNotPublishThirdPartyCapabilities(t *testing.T) {
	p := loginFixture(t)
	if err := edit.WriteAtomic(p.meta, []byte(`{"appliedId":"`+desktopProfileID+`","entries":[{"id":"`+desktopProfileID+`","name":"magpie"}]}`)); err != nil {
		t.Fatal(err)
	}
	if !desktopWired(p) {
		t.Fatal("fixture is not third party")
	}
	if err := desktopLoginProxyAt(p, true, "http://127.0.0.1:3426"); err != nil {
		t.Fatal(err)
	}
	if desktopWired(p) {
		t.Fatal("native login still treated as third party: global model capabilities would override the native UI")
	}
	if !desktopLoginApplied(p) {
		t.Fatal("native profile not detected")
	}
	if err := desktopLoginProxyAt(p, false, ""); err != nil {
		t.Fatal(err)
	}
	if !desktopWired(p) {
		t.Fatal("third-party state not restored")
	}
}

func TestDesktopLoginConnectionStatusAndDrift(t *testing.T) {
	home, p := desktopSandbox(t)
	if err := desktopLoginProxyAt(p, true, "http://127.0.0.1:3426"); err != nil {
		t.Fatal(err)
	}
	if err := claudelogin.Save(claudelogin.Config{Enabled: true, Listen: "127.0.0.1:3426", Models: map[string]string{"opus": "group/opus"}}); err != nil {
		t.Fatal(err)
	}
	a := claudeDesktop(home)
	record(a.ID, "provider", "magpie")
	if !a.Wired() || a.Values()["provider"] != "magpie" {
		t.Fatal("native proxy is reported as disconnected")
	}
	if d := a.Drift(); d != nil {
		t.Fatalf("native proxy has false drift: %+v", d)
	}
	if !strings.Contains(a.Notice(), "official login") {
		t.Fatal("native proxy notice describes third-party mode")
	}
	if desktopWired(p) {
		t.Fatal("native proxy publishes third-party capabilities")
	}
	if err := edit.SetJSON(p.config, edit.KV{Path: "deploymentMode", Value: "3p"}); err != nil {
		t.Fatal(err)
	}
	if d := a.Drift(); d == nil || d.Kind != "unwired" {
		t.Fatalf("real native proxy drift was hidden: %+v", d)
	}
}
