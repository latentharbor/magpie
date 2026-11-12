package claudelogin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRejectsUnsafeOriginsAndWildcardRoutes(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:3426", "localhost:3426", "127.0.0.1:0", "127.0.0.1:000", "127.0.0.1:http", "[::]:3426", "127.0.0.1:65536"} {
		if err := (Config{Listen: listen}).Validate(); err == nil {
			t.Errorf("accepted listener %q", listen)
		}
	}
	for _, model := range []string{"*", "claude-*", " opus", "opus\n"} {
		if err := (Config{Listen: "127.0.0.1:3426", Models: map[string]string{model: "group/opus"}}).Validate(); err == nil {
			t.Errorf("accepted model %q", model)
		}
	}
	for _, origin := range []string{"https://127.0.0.1:3425", "http://localhost:3425", "http://127.0.0.1:3425/v1", "http://127.0.0.1:3425/?key=secret", "http://user:pass@127.0.0.1:3425", "http://example.org"} {
		if _, err := GatewayURL(origin); err == nil {
			t.Errorf("accepted gateway %q", origin)
		}
	}
}

func TestCertificateStatusCannotTrustMismatchedPublicCA(t *testing.T) {
	// Status must not generate a certificate on a disabled installation.
	if got := ReadCertificate(); got.Exists {
		t.Fatal("unexpected initial CA")
	}
	if _, err := os.Stat(CertDir()); !os.IsNotExist(err) {
		t.Fatal("status created CA directory")
	}
	if _, err := EnsureAuthority(context.Background(), CertDir()); err != nil {
		t.Fatal(err)
	}
	got := ReadCertificate()
	if !got.Exists || got.SHA256 == "" {
		t.Fatalf("missing fingerprint: %+v", got)
	}
	dir := t.TempDir()
	if _, err := EnsureAuthority(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	other, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(CertDir(), "ca.pem"), other, 0600); err != nil {
		t.Fatal(err)
	}
	got = ReadCertificate()
	if got.Trusted || got.Error == "" {
		t.Fatalf("mismatched authority accepted: %+v", got)
	}
}
