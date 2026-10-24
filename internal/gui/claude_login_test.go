package gui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/claudelogin"
)

func TestClaudeLoginAPIIsReadOnlyUntilExplicitPrepare(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	mux := http.NewServeMux()
	claudeLoginRoutes(mux)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	if w := call("GET", "/api/claude-login", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(claudelogin.CertDir()); !os.IsNotExist(err) {
		t.Fatal("GET created authority")
	}
	body := `{"enabled":true,"listen":"127.0.0.1:3426","models":{"claude-opus-5-5":"group/opus"}}`
	if w := call("POST", "/api/claude-login", body); w.Code == 200 {
		t.Fatal("untrusted routing enabled")
	}
	if _, err := os.Stat(claudelogin.Path()); !os.IsNotExist(err) {
		t.Fatal("failed enable wrote configuration")
	}
	for _, bad := range []string{body + ` {}`, `{"listen":"127.0.0.1:3426","unknown":true}`} {
		if w := call("POST", "/api/claude-login", bad); w.Code == 200 {
			t.Fatal("invalid API request accepted")
		}
	}
	if w := call("POST", "/api/claude-login/prepare", `{}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"exists":true`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := claudelogin.ReadCertificate(); got.Trusted {
		t.Fatal("preparation trusted test CA")
	}
	body = strings.Replace(body, `"enabled":true`, `"enabled":false`, 1)
	if w := call("POST", "/api/claude-login", body); w.Code != 200 {
		t.Fatalf("save disabled mappings: %s", w.Body)
	}
	got, err := claudelogin.Load()
	if err != nil || got.Models["claude-opus-5-5"] != "group/opus" || got.Enabled {
		t.Fatalf("%+v %v", got, err)
	}
}
