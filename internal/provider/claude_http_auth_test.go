package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func claudeHTTPFixture(t *testing.T) (string, *Account) {
	t.Helper()
	home := claudeHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	old := claudeKeychain
	claudeKeychain = false
	t.Cleanup(func() { claudeKeychain = old })
	claudeExecutable = func() string {
		t.Fatal("HTTP authentication looked for Claude Code executable")
		return ""
	}
	noAnthropic(t)
	claudeSignIn(t, home, time.Now().Add(time.Hour))
	writeFile(t, claudeProfilePath(), map[string]any{
		"userID":       strings.Repeat("a", 64),
		"oauthAccount": map[string]any{"emailAddress": "own@example.com", "accountUuid": "own-account", "organizationUuid": "own-org"},
	})
	return home, &Account{Agent: "claude", User: "own@example.com", Plan: "max"}
}

func TestClaudeHTTPAuthOwnAndNativeRotation(t *testing.T) {
	_, a := claudeHTTPFixture(t)
	// Seed discovery's cache, then rotate on disk without clearing it.
	if _, _, ok := claudeCredential(); !ok {
		t.Fatal("fixture sign-in missing")
	}
	for _, token := range []string{"first-access", "rotated-access"} {
		writeFile(t, claudeCredentialsPath(), map[string]any{"claudeAiOauth": claudeAuth{
			AccessToken: token, RefreshToken: "never-spent", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			Scopes: []string{"user:inference"}, SubscriptionType: "max",
		}})
		got, err := ClaudeHTTPAuth(context.Background(), a)
		if err != nil || got != (ClaudeHTTPCredentials{Token: token, AccountUUID: "own-account", OrganizationUUID: "own-org", DeviceID: strings.Repeat("a", 64)}) {
			t.Fatalf("current sign-in = %+v, %v", got, err)
		}
	}
	if tok, own, err := a.Token(context.Background()); err != nil || tok != "" || own {
		t.Fatalf("native Token contract changed: %q, %v, %v", tok, own, err)
	}
	writeFile(t, claudeCredentialsPath(), map[string]any{"claudeAiOauth": claudeAuth{}})
	if _, err := ClaudeHTTPAuth(context.Background(), a); err == nil {
		t.Fatal("cleared native credential served from discovery cache")
	}
}

func TestClaudeHTTPAuthSavedUsesItsIdentityAndKeepsNativeRotation(t *testing.T) {
	_, _ = claudeHTTPFixture(t)
	l := savedLogin{Agent: "claude", User: "side@example.com", On: true,
		Auth:    mustJSONRaw(t, map[string]any{"claudeAiOauth": claudeAuth{AccessToken: "saved-access", RefreshToken: "saved-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Scopes: []string{"user:inference"}}}),
		Profile: mustJSONRaw(t, map[string]any{"emailAddress": "side@example.com", "accountUuid": "side-account", "organizationUuid": "side-org", "userID": "another-device"}),
	}
	writeFile(t, loginsPath(), []savedLogin{l})
	a := &Account{Agent: "claude", User: l.User, token: func(context.Context) (string, error) { t.Fatal("HTTP called Account.Token"); return "", nil }}
	for _, token := range []string{"saved-access", "native-rotated"} {
		if token == "native-rotated" {
			writeFile(t, filepath.Join(claudeAccountDir(l.User), ".credentials.json"), map[string]any{"claudeAiOauth": claudeAuth{AccessToken: token, RefreshToken: "rotated-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Scopes: []string{"user:inference"}}})
		}
		got, err := ClaudeHTTPAuth(context.Background(), a)
		if err != nil || got != (ClaudeHTTPCredentials{Token: token, AccountUUID: "side-account", OrganizationUUID: "side-org", DeviceID: strings.Repeat("a", 64)}) {
			t.Fatalf("saved sign-in = %+v, %v", got, err)
		}
	}
	kept := readLogins()
	c, _ := parseClaudeCredentials(kept[0].Auth)
	if c.OAuth.AccessToken != "native-rotated" || c.OAuth.RefreshToken != "rotated-refresh" {
		t.Fatal("native rotation not synchronized to saved login")
	}
}

func TestClaudeHTTPAuthRefusesExpiredAndIncompleteCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		auth       claudeAuth
	}{
		{"expired", "refresh this account in Claude Code", claudeAuth{AccessToken: "expired", RefreshToken: "do-not-spend", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(), Scopes: []string{"user:inference"}}},
		{"unknown expiry", "has no expiry", claudeAuth{AccessToken: "unknown", RefreshToken: "do-not-spend", Scopes: []string{"user:inference"}}},
		{"missing scope", "user:inference", claudeAuth{AccessToken: "profile-only", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Scopes: []string{"user:profile"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, a := claudeHTTPFixture(t)
			writeFile(t, claudeCredentialsPath(), map[string]any{"claudeAiOauth": tc.auth})
			before, err := os.ReadFile(claudeCredentialsPath())
			if err != nil {
				t.Fatal(err)
			}
			got, err := ClaudeHTTPAuth(context.Background(), a)
			if err == nil || !strings.Contains(err.Error(), tc.want) || got.Token != "" {
				t.Fatalf("unusable sign-in = %+v, %v", got, err)
			}
			after, err := os.ReadFile(claudeCredentialsPath())
			if err != nil || string(before) != string(after) {
				t.Fatal("HTTP auth changed native credentials")
			}
		})
	}
}

func TestClaudeHTTPAuthRequiresOwnDeviceAndSelectedAccount(t *testing.T) {
	_, a := claudeHTTPFixture(t)
	a.User = "another@example.com"
	if _, err := ClaudeHTTPAuth(context.Background(), a); err == nil || !strings.Contains(err.Error(), "account changed") {
		t.Fatalf("mismatched own account: %v", err)
	}
	a.User = "own@example.com"
	writeFile(t, claudeProfilePath(), map[string]any{"oauthAccount": map[string]any{"emailAddress": a.User, "accountUuid": "own-account", "organizationUuid": "own-org", "userID": "profile-user"}})
	if _, err := ClaudeHTTPAuth(context.Background(), a); err == nil || !strings.Contains(err.Error(), "this device's userID") {
		t.Fatalf("invented device identity: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ClaudeHTTPAuth(ctx, a); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := ClaudeHTTPAuth(context.Background(), nil); err == nil {
		t.Fatal("accepted absent account")
	}
}

func TestClaudeHTTPAuthUsesConfiguredClaudeDirectory(t *testing.T) {
	home, a := claudeHTTPFixture(t)
	dir := filepath.Join(home, "configured-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeFile(t, filepath.Join(dir, ".credentials.json"), map[string]any{"claudeAiOauth": claudeAuth{
		AccessToken: "configured-access", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Scopes: []string{"user:inference"},
	}})
	writeFile(t, filepath.Join(dir, ".claude.json"), map[string]any{
		"userID": "configured-device", "oauthAccount": map[string]any{"emailAddress": a.User, "accountUuid": "configured-account", "organizationUuid": "configured-org"},
	})
	got, err := ClaudeHTTPAuth(context.Background(), a)
	if err != nil || got != (ClaudeHTTPCredentials{Token: "configured-access", AccountUUID: "configured-account", OrganizationUUID: "configured-org", DeviceID: "configured-device"}) {
		t.Fatalf("configured sign-in = %+v, %v", got, err)
	}
}
