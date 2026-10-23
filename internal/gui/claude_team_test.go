package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestClaudeTeamAPIKeepsAdministratorCredentialPrivate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	mux := http.NewServeMux()
	claudeTeamRoutes(mux)
	org := "00000000-0000-4000-8000-000000000001"
	key := "fixture-private-admin-cookie"
	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/claude-team/config", strings.NewReader(body)))
		return w
	}
	w := post(`{"enabled":false,"organizationId":"` + org + `","sessionKey":"` + key + `","threshold":95,"members":[]}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), key) {
		t.Fatalf("save leaked or failed: %d %s", w.Code, w.Body)
	}
	file := filepath.Join(filepath.Dir(provider.Path()), "claude-team.json")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var saved provider.ClaudeTeamConfig
	if err = json.Unmarshal(b, &saved); err != nil || saved.SessionKey != key {
		t.Fatal("credential wasn't stored privately")
	}
	st, err := os.Stat(file)
	if err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", st, err)
	}
	for _, path := range []string{"config", "members"} {
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/claude-team/"+path, nil))
		if w.Code != 200 || strings.Contains(w.Body.String(), key) || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("GET unsafe: %d %s", w.Code, w.Body)
		}
	}
	w = post(`{"enabled":false,"organizationId":"` + org + `","sessionKey":"","threshold":100,"members":[]}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	b, _ = os.ReadFile(file)
	_ = json.Unmarshal(b, &saved)
	if saved.SessionKey != key {
		t.Fatal("blank key discarded administrator session")
	}
	before := string(b)
	w = post(`{"enabled":true,"organizationId":"invalid","sessionKey":"secret-invalid","members":[]}`)
	if w.Code == 200 || strings.Contains(w.Body.String(), "secret-invalid") {
		t.Fatal("invalid config accepted or reflected")
	}
	b, _ = os.ReadFile(file)
	if string(b) != before {
		t.Fatal("invalid save changed configuration")
	}
	for _, body := range []string{`{broken`, `{"organizationId":"invalid","accountId":"invalid"}`, `{"organizationId":"` + org + `","accountId":"00000000-0000-4000-8000-000000000002"}`} {
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/claude-team/quota", strings.NewReader(body)))
		if w.Code < 400 || strings.Contains(w.Body.String(), key) {
			t.Fatalf("invalid quota retry was accepted or leaked a credential: %d %s", w.Code, w.Body)
		}
	}
	// A retry only queues the selected local identity. No administrator or
	// inference transport is installed in this isolated test.
	account := "00000000-0000-4000-8000-000000000002"
	login := `[{"agent":"claude","user":"fixture@example.test","auth":{},"profile":{"emailAddress":"fixture@example.test","accountUuid":"` + account + `","organizationUuid":"` + org + `"}}]`
	if err := os.WriteFile(filepath.Join(filepath.Dir(provider.Path()), "logins.json"), []byte(login), 0600); err != nil {
		t.Fatal(err)
	}
	w = post(`{"enabled":false,"organizationId":"` + org + `","threshold":100,"members":["` + account + `"]}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/claude-team/quota", strings.NewReader(`{"organizationId":"`+org+`","accountId":"`+account+`"}`)))
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"quotaPending":true`) || strings.Contains(w.Body.String(), key) {
		t.Fatalf("retry did not queue a private quota read: %d %s", w.Code, w.Body)
	}
}
