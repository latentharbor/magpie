package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
)

type distributionZipEntry struct {
	name string
	mode os.FileMode
	text string
}

func stockDistributionEntries() []distributionZipEntry {
	return []distributionZipEntry{
		{"magpie.app/Contents/Info.plist", 0o644, "plist"},
		{"magpie.app/Contents/MacOS/magpie", 0o755, "executable"},
	}
}

func distributionZip(t *testing.T, entries []distributionZipEntry) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "app.zip")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(e.mode)
		entry, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(e.text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestDistributionArchiveRejectsUnsafePathsBeforeExtraction(t *testing.T) {
	for _, extra := range []distributionZipEntry{
		{"../escaped", 0o644, "bad"},
		{"/absolute", 0o644, "bad"},
		{"magpie.app/Contents/../other", 0o644, "bad"},
		{"magpie.app/Contents/Other:fork", 0o644, "bad"},
		{"magpie.app\\Contents\\other", 0o644, "bad"},
		{"other.app/Contents/executable", 0o755, "bad"},
		{"magpie.app/Contents/info.plist", 0o644, "alias"},
		{"magpie.app/Contents/link", os.ModeSymlink | 0o777, "../../../escaped"},
		{"__MACOSX/magpie.app/not-metadata", 0o644, "bad"},
		{"__MACOSX/._magpie.app", os.ModeSymlink | 0o777, "../../../escaped"},
	} {
		t.Run(extra.name, func(t *testing.T) {
			archive := distributionZip(t, append(stockDistributionEntries(), extra))
			out := filepath.Join(t.TempDir(), "app")
			if err := unpackDistribution(archive, out); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("unsafe archive wrote files before validation completed")
			}
		})
	}
	for _, entries := range [][]distributionZipEntry{
		stockDistributionEntries()[:1],
		{{"magpie.app/Contents/Info.plist", 0o644, "plist"}, {"magpie.app/Contents/MacOS/magpie", 0o644, "not executable"}},
	} {
		if unpackDistribution(distributionZip(t, entries), filepath.Join(t.TempDir(), "out")) == nil {
			t.Fatal("missing or nonexecutable app accepted")
		}
	}
}

func TestDistributionArchiveAllowsAppleDoubleWithoutRestoringIt(t *testing.T) {
	entries := append(stockDistributionEntries(),
		distributionZipEntry{"magpie.app/Contents/._Info.plist", 0o644, "inline metadata"},
		distributionZipEntry{"__MACOSX/", os.ModeDir | 0o755, ""},
		distributionZipEntry{"__MACOSX/._magpie.app", 0o644, "metadata"},
		distributionZipEntry{"__MACOSX/magpie.app/Contents/._Info.plist", 0o644, "metadata"})
	out := filepath.Join(t.TempDir(), "app")
	if err := unpackDistribution(distributionZip(t, entries), out); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(out, "magpie.app/Contents/MacOS/magpie")); err != nil || string(data) != "executable" {
		t.Fatalf("stock executable not extracted: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(out, "__MACOSX")); !os.IsNotExist(err) {
		t.Fatal("AppleDouble metadata restored")
	}
	if _, err := os.Stat(filepath.Join(out, "magpie.app/Contents/._Info.plist")); !os.IsNotExist(err) {
		t.Fatal("inline AppleDouble sidecar restored")
	}
}

func TestDistributionArchiveBoundsExpandedSize(t *testing.T) {
	archive := distributionZip(t, stockDistributionEntries())
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	entry := bytes.Index(data, []byte("PK\x01\x02"))
	if entry < 0 {
		t.Fatal("no ZIP central directory")
	}
	// A forged size header must be rejected before decompression or writes;
	// no enormous test file is allocated.
	binary.LittleEndian.PutUint32(data[entry+24:], 2<<30+1)
	if err := os.WriteFile(archive, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := unpackDistribution(archive, out); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized expanded archive: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("oversized archive wrote files")
	}
}

func TestDistributionArchiveFromDitto(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS ditto compatibility")
	}
	dir := t.TempDir()
	app := filepath.Join(dir, "magpie.app")
	for _, e := range stockDistributionEntries() {
		dest := filepath.Join(dir, e.name)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, []byte(e.text), e.mode); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(dir, "ditto.zip")
	if b, err := proc.Command("/usr/bin/ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", app, archive).CombinedOutput(); err != nil {
		t.Fatalf("ditto: %s %v", b, err)
	}
	if err := unpackDistribution(archive, filepath.Join(dir, "out")); err != nil {
		t.Fatalf("standard ditto archive rejected: %v", err)
	}
}

func TestDistributionArchivePreservesRealAdHocSignature(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS real local codesign compatibility")
	}
	dir := t.TempDir()
	app := filepath.Join(dir, "magpie.app")
	for _, sub := range []string{"MacOS", "Resources"} {
		if err := os.MkdirAll(filepath.Join(app, "Contents", sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Use a local Mach-O fixture without executing it or creating a network
	// request. Signing this temporary copy needs no certificate or keychain.
	binary, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents/MacOS/magpie"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0"?><plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.yetone.magpie</string>
<key>CFBundleExecutable</key><string>magpie</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>1.0.1</string>
<key>CFBundleVersion</key><string>1.0.1</string>
</dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents/Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := proc.Command("/usr/bin/codesign", "--force", "--deep", "--sign", "-", app).CombinedOutput(); err != nil {
		t.Fatalf("ad-hoc sign temporary fixture: %s %v", b, err)
	}
	archive := filepath.Join(dir, "signed.zip")
	if b, err := proc.Command("/usr/bin/ditto", "-c", "-k", "--keepParent", app, archive).CombinedOutput(); err != nil {
		t.Fatalf("ditto: %s %v", b, err)
	}
	out := filepath.Join(dir, "out")
	if err := unpackDistribution(archive, out); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(out, "magpie.app")
	if err := distributionBundle(context.Background(), extracted, "1.0.1"); err != nil {
		t.Fatal(err)
	}
	if b, err := proc.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", "--verbose=4", extracted).CombinedOutput(); err != nil {
		t.Fatalf("archive extraction broke the actual ad-hoc signature: %s %v", b, err)
	}
}

// Stage, not only a validator helper, must reject a correctly signed archive
// with the wrong identity before accepting even an otherwise same-team app.
func TestDistributionStageChecksAppIdentityBeforeSigner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plutil and codesign fixtures are Unix shell scripts")
	}
	key, rel, _ := distributionFixture(t)
	archive := distributionZip(t, stockDistributionEntries())
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	a := rel.Assets[AppAsset()]
	sum := sha256.Sum256(data)
	a.SHA256, a.Size = hex.EncodeToString(sum[:]), int64(len(data))
	rel.Assets[AppAsset()] = a
	raw, _ := json.Marshal(rel)
	rel, err = verifyDistribution(raw, fixtureSignature(key, raw))
	if err != nil {
		t.Fatal(err)
	}
	old := client
	t.Cleanup(func() { client = old })
	client = &http.Client{Transport: distributionTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != a.URL {
			t.Fatalf("unexpected request: %s", r.URL)
		}
		return distributionResponse(r, 200, string(data), ""), nil
	})}
	for _, field := range []string{"", "CFBundleIdentifier", "CFBundleExecutable", "CFBundlePackageType", "CFBundleShortVersionString", "CFBundleVersion"} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			programs := t.TempDir()
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$2\" = '%s' ]; then echo WRONG; exit 0; fi\ncase \"$2\" in\nCFBundleIdentifier) echo com.yetone.magpie;;\nCFBundleExecutable) echo magpie;;\nCFBundlePackageType) echo APPL;;\nCFBundleShortVersionString|CFBundleVersion) echo 1.0.1;;\n*) exit 1;;\nesac\n", field)
			testenv.Program(t, filepath.Join(programs, "plutil"), script)
			testenv.Program(t, filepath.Join(programs, "codesign"), "#!/bin/sh\nif [ \"$1\" = --verify ]; then exit 0; fi\necho TeamIdentifier=TEAMONE >&2\n")
			t.Setenv("PATH", programs)
			staged, err := Stage(context.Background(), rel, filepath.Join(dir, "magpie.app"))
			if field == "" {
				if err != nil || staged == "" {
					t.Fatalf("valid same-team distribution app rejected: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), field) {
					t.Fatalf("wrong %s reached signer/installation: staged=%s err=%v", field, staged, err)
				}
				if _, err := os.Stat(filepath.Join(dir, ".magpie-update")); !os.IsNotExist(err) {
					t.Fatal("rejected staged app retained")
				}
			}
		})
	}
}
