package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeTeamSessionCreatesAnEditableProviderBeforeMembers(t *testing.T) {
	_, f, old := teamFixture(t)
	org := teamID(80)
	v, err := CreateClaudeTeam(context.Background(), "fixture-admin", org)
	if err != nil {
		t.Fatal(err)
	}
	if v.Config.Enabled || len(v.Members) != 0 || v.SessionSet != true || v.Config.SessionKey != "" {
		t.Fatalf("new provider must be editable without enabling inference: %+v", v)
	}
	ps := claudeTeamProviders(claudeProvider(&Account{Agent: "claude"}))
	found := false
	for _, p := range ps {
		if ClaudeTeamOf(p) == org {
			found = true
			if p.TeamRouteOn() || len(p.AlsoOn()) != 0 {
				t.Fatal("empty organization admitted an inference account")
			}
		}
	}
	if !found {
		t.Fatal("session was saved but the empty provider disappeared")
	}
	if p, err := Find(claudeTeamPrefix + org); err != nil || p.Account == nil || p.TeamRouteOn() {
		t.Fatal("the ordinary provider list cannot edit the empty organization", err)
	}
	if len(f.writes) != 0 {
		t.Fatal("adding a session moved a seat")
	}
	b, _ := json.Marshal(ClaudeTeamSubscriptions())
	if strings.Contains(string(b), "fixture-admin") {
		t.Fatal("session escaped in public subscription data")
	}
	// Re-adding an existing organization preserves its settings.
	if err := Delete(claudeTeamPrefix + old.OrganizationID); err != nil {
		t.Fatal(err)
	}
	v, err = CreateClaudeTeam(context.Background(), "fixture-new-admin", old.OrganizationID)
	if err != nil || !v.Config.Enabled || len(v.Config.Members) != len(old.Members) {
		t.Fatal("re-adding reset the existing organization", err)
	}
	if _, err := Find(claudeTeamPrefix + old.OrganizationID); err != nil {
		t.Fatal("re-added provider stayed hidden", err)
	}
	t.Run("a different configuration cannot inherit the empty provider", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		if got := ClaudeTeamSubscriptions(); len(got) != 0 {
			t.Fatal("subscriptions escaped their configuration directory")
		}
	})
}

func TestClaudeTeamSessionDiscoveryDoesNotPersistOrExposeCredential(t *testing.T) {
	tm, f, _ := teamFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/organizations" {
			t.Error("unexpected discovery request", r.Method, r.URL.Path)
		}
		key, err := r.Cookie("sessionKey")
		if err != nil || key.Value != "fixture-admin" {
			t.Error("administrator session not used")
		}
		w.Write([]byte(`[{"uuid":"` + teamID(80) + `","name":"New Team","ignored":"fixture-admin"},{"uuid":"invalid","name":"bad"}]`))
	}))
	defer server.Close()
	before := tm.status()
	old := readClaudeTeamOrganizations
	readClaudeTeamOrganizations = func(ctx context.Context, key string) ([]ClaudeTeamOrganizationInfo, error) {
		c := &claudeTeamHTTP{key: key, base: server.URL, client: server.Client()}
		return c.organizations(ctx)
	}
	defer func() { readClaudeTeamOrganizations = old }()
	organizations, err := DiscoverClaudeTeams(context.Background(), "sessionKey=fixture-admin")
	if err != nil || len(organizations) != 1 || organizations[0].ID != teamID(80) {
		t.Fatal(organizations, err)
	}
	b, _ := json.Marshal(organizations)
	if strings.Contains(string(b), "fixture-admin") || len(f.writes) != 0 ||
		len(tm.status().Config.Members) != len(before.Config.Members) || len(ClaudeTeamSubscriptions()) != 1 {
		t.Fatal("discovery persisted or leaked the credential, changed members or moved seats")
	}
}
