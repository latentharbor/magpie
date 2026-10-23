package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/gateway"
)

// dshHermesPlist is DeepSeek Hermes' Info.plist as its 1.0.0 macOS build
// ships it (DeepSeek.Hermes-1.0.0-arm64-mac.zip on
// github.com/motongv/deepseek-hermes-desktop's releases), cut to the keys
// around the ones read, in their order and indentation.
const dshHermesPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>CFBundleDisplayName</key>
    <string>DeepSeek Hermes</string>
    <key>CFBundleExecutable</key>
    <string>DeepSeek Hermes</string>
    <key>CFBundleIconFile</key>
    <string>icon.icns</string>
    <key>CFBundleIdentifier</key>
    <string>com.deepseek.hermes.desktop</string>
    <key>CFBundleInfoDictionaryVersion</key>
    <string>6.0</string>
    <key>CFBundleName</key>
    <string>DeepSeek Hermes</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0.0</string>
    <key>CFBundleVersion</key>
    <string>1.0.0</string>
  </dict>
</plist>`

// dshHermesHome is a home with no dsh CLI and no app, apps the folder an
// app is installed into.
func dshHermesHome(t *testing.T) (home, dir, apps string) {
	t.Helper()
	home, dir, _ = dshRouteHome(t)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no dsh CLI
	apps = t.TempDir()
	was, wasReg := appFolders, windowsRegisteredApp
	appFolders = func() []string { return []string{apps} }
	windowsRegisteredApp = func(string) (string, string) { return "", "" }
	t.Cleanup(func() { appFolders, windowsRegisteredApp = was, wasReg })
	return home, dir, apps
}

// WuSix on Discord: magpie's Agents page didn't find DeepSeek Hermes, so
// its models were added in the app by hand. DeepSeek Hermes is DeepSeek
// Harness's web UI in a window (dsh 0.1.0-rc.6 bundled, ~/.dsh), so it is
// the dsh row: found installed and never opened, and opened with no dsh
// CLI it is no leftover ~/.dsh (it was 未找到 CLI, its settings hidden).
func TestDshHermesFound(t *testing.T) {
	judgeFor(t, "darwin")
	home, dir, apps := dshHermesHome(t)
	if dsh(home).Detected() {
		t.Fatal("neither app nor CLI, yet detected")
	}
	installApp(t, apps, "DeepSeek Hermes.app", dshHermesPlist)
	a := dsh(home)
	a.Bin = "magpie-test-gone-dsh"
	if !a.Detected() || a.CLIMissing() {
		t.Fatalf("DeepSeek Hermes never opened: detected %v, CLI missing %v", a.Detected(), a.CLIMissing())
	}
	// its profile is web's: it runs `dsh web`
	if web := filepath.Join(dir, "profiles", "web", "cordis.patch.yml"); a.Path != web {
		t.Fatalf("names %s, not the web profile", a.Path)
	}

	// opened: dsh's initProfile made the web profile
	web := filepath.Join(dir, "profiles", "web")
	writeT(t, filepath.Join(web, "cordis.patch.yml"), dshDesktopPatch)
	writeT(t, filepath.Join(web, "package.json"), "{}\n")
	a = dsh(home)
	a.Bin = "magpie-test-gone-dsh"
	if !a.Detected() || a.CLIMissing() {
		t.Fatalf("DeepSeek Hermes opened: detected %v, CLI missing %v", a.Detected(), a.CLIMissing())
	}

	// another app under that name is not it
	os.RemoveAll(filepath.Join(apps, "DeepSeek Hermes.app"))
	installApp(t, apps, "DeepSeek Hermes.app", strings.Replace(dshHermesPlist, "com.deepseek.hermes.desktop", "com.example.other", 1))
	if !a.CLIMissing() {
		t.Fatal("another bundle id taken for DeepSeek Hermes")
	}
}

// On Windows DeepSeek Hermes' installer (electron-builder's NSIS, read from
// DeepSeek.Hermes.Setup.1.0.0.exe) puts DeepSeek Hermes.exe under
// %LOCALAPPDATA%\Programs\DeepSeek Hermes, or a folder the user picks,
// registered under HKCU\Software\df9f5892-…'s InstallLocation.
func TestDshHermesWindows(t *testing.T) {
	home, _, apps := dshHermesHome(t)
	picked := t.TempDir()
	reg := map[string][2]string{}
	windowsRegisteredApp = func(guid string) (string, string) { r := reg[guid]; return r[0], r[1] }
	if dsh(home).Detected() {
		t.Fatal("nothing installed, yet detected")
	}
	writeT(t, filepath.Join(apps, "DeepSeek Hermes", "DeepSeek Hermes.exe"), "MZ")
	if !dsh(home).Detected() {
		t.Fatal("DeepSeek Hermes under Programs not found")
	}
	os.RemoveAll(filepath.Join(apps, "DeepSeek Hermes"))
	reg["df9f5892-ad1c-5623-a260-30fb0acc7e55"] = [2]string{picked, "1.0.0"}
	if dsh(home).Detected() {
		t.Fatal("a registered folder without the app taken for it")
	}
	writeT(t, filepath.Join(picked, "DeepSeek Hermes.exe"), "MZ")
	if !dsh(home).Detected() {
		t.Fatal("DeepSeek Hermes in its registered folder not found")
	}
}

// Picking a model of magpie's with DeepSeek Hermes installed and never
// opened writes the web profile it will start on, and the key in .env
// alone: the dsh it bundles (0.1.0-rc.6) refuses to start on the version 1
// key store magpie makes for DeepSeek Harness Desktop ("the value for
// "version" … must be a string"), and reads .env. A store magpie made
// before goes; one with the user's keys stays.
func TestDshHermesWired(t *testing.T) {
	home, dir, apps := dshHermesHome(t)
	installApp(t, apps, "DeepSeek Hermes.app", dshHermesPlist)
	web := filepath.Join(dir, "profiles", "web", "cordis.patch.yml")
	creds := filepath.Join(dir, ".credentials.yaml")
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }

	a := dsh(home)
	if err := a.Pick("model", "magpie/deepseek/flash"); err != nil {
		t.Fatal(err)
	}
	s := read(web)
	if !strings.HasPrefix(s, strings.TrimSuffix(dshDesktopPatch, "[]\n")) || !strings.Contains(s, "apiKeyEnv: "+dshKeyRef) || !strings.Contains(s, "provider: "+dshRoute) {
		t.Fatalf("the web profile magpie made:\n%s", s)
	}
	if isFile(dshDesktopFile(dir)) {
		t.Fatal("made DeepSeek Harness Desktop's profile, which DeepSeek Hermes doesn't read")
	}
	if isFile(creds) {
		t.Fatalf("wrote dsh's key store, which DeepSeek Hermes' dsh can't start on:\n%s", read(creds))
	}
	if v, _ := edit.GetEnvFile(filepath.Join(dir, ".env"), dshKeyRef); v != keyAt(gateway.URL()) {
		t.Fatal("no key in .env")
	}
	a = dsh(home)
	if !a.Wired() || a.Check() != "" {
		t.Fatalf("not wired: wired %v, check %q", a.Wired(), a.Check())
	}
	if n := a.Notice(); !strings.Contains(n, noticeDsh6.String()) || strings.Contains(n, "Harness Desktop") {
		t.Fatalf("notice: %q", n)
	}

	// a store magpie made before, with nothing but its key: it goes
	writeT(t, creds, "version: 1\nrefs:\n  "+dshKeyRef+": "+keyAt(gateway.URL())+"\n")
	if err := a.Pick("model", "magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if isFile(creds) {
		t.Fatalf("magpie's own key store outlived a pick:\n%s", read(creds))
	}
	// the user's own keys there stay
	own := "version: 1\nrefs:\n  MINE_API_KEY: sk-mine\n  " + dshKeyRef + ": " + keyAt(gateway.URL()) + "\n"
	writeT(t, creds, own)
	if err := a.Pick("model", "magpie/deepseek/flash"); err != nil {
		t.Fatal(err)
	}
	if read(creds) != own {
		t.Fatalf("the user's key store changed:\n%s", read(creds))
	}
	os.Remove(creds)

	// Disconnect before the app ever opened: what magpie made goes
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles")); !os.IsNotExist(err) {
		t.Fatalf("the profile magpie made outlived Disconnect: %v\n%s", err, read(web))
	}

	// with DeepSeek Harness Desktop here too, its profile and store as before
	installApp(t, apps, "DeepSeek Harness.app", dshAppPlist)
	a = dsh(home)
	if err := a.Pick("model", "magpie/deepseek/flash"); err != nil {
		t.Fatal(err)
	}
	if !isFile(dshDesktopFile(dir)) || !strings.Contains(read(creds), dshKeyRef+": ") {
		t.Fatalf("DeepSeek Harness Desktop's profile %v, store:\n%s", isFile(dshDesktopFile(dir)), read(creds))
	}
	if n := a.Notice(); !strings.Contains(n, noticeDsh5.String()) {
		t.Fatalf("notice with both apps: %q", n)
	}
}
