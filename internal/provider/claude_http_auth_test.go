package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
		version := (claudeCredentials{OAuth: claudeAuth{AccessToken: token, RefreshToken: "never-spent"}}).version()
		if err != nil || got != (ClaudeHTTPCredentials{Token: token, AccountUUID: "own-account", OrganizationUUID: "own-org", DeviceID: strings.Repeat("a", 64), LoginVersion: version}) {
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
		if err != nil || got != (ClaudeHTTPCredentials{Token: token, AccountUUID: "side-account", OrganizationUUID: "side-org", DeviceID: strings.Repeat("a", 64), LoginVersion: ClaudeLoginVersion(l.User)}) {
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
	version := (claudeCredentials{OAuth: claudeAuth{AccessToken: "configured-access"}}).version()
	if err != nil || got != (ClaudeHTTPCredentials{Token: "configured-access", AccountUUID: "configured-account", OrganizationUUID: "configured-org", DeviceID: "configured-device", LoginVersion: version}) {
		t.Fatalf("configured sign-in = %+v, %v", got, err)
	}
}

func TestClaudeHTTPAuthNativeRefreshSharesAttemptAndAllowsCancellation(t *testing.T) {
	home, a := claudeHTTPFixture(t)
	dir := filepath.Join(home, "configured")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeFile(t, filepath.Join(dir, ".claude.json"), map[string]any{
		"userID": "device", "oauthAccount": map[string]any{"emailAddress": a.User, "accountUuid": "own-account", "organizationUuid": "own-org"},
	})
	put := func(token string, expires time.Time) {
		b, _ := json.Marshal(map[string]any{"claudeAiOauth": claudeAuth{
			AccessToken: token, RefreshToken: "native-only", ExpiresAt: expires.UnixMilli(), Scopes: []string{"user:inference"},
		}})
		if err := writePrivate(filepath.Join(dir, ".credentials.json"), b); err != nil {
			t.Error(err)
		}
	}
	put("old", time.Now().Add(-time.Hour))
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	old := UsageClaudeVia(func(ctx context.Context, got string) (string, error) {
		if got != dir {
			t.Errorf("native refresh directory=%q, want configured directory", got)
		}
		calls.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
		}
		put("native-renewed", time.Now().Add(time.Hour))
		// Native access refresh succeeded even if /usage itself was denied.
		return "", errors.New("private helper stderr")
	})
	t.Cleanup(func() { UsageClaudeVia(old) })
	var wg sync.WaitGroup
	answer := func() {
		defer wg.Done()
		got, err := ClaudeHTTPAuth(context.Background(), a)
		if err != nil || got.Token != "native-renewed" {
			t.Errorf("native renewal not read: token matches=%v error=%v", got.Token == "native-renewed", err)
		}
	}
	wg.Add(1)
	go answer()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ClaudeHTTPAuth(ctx, a); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	for range 16 {
		wg.Add(1)
		go answer()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("native refresh attempts=%d, want one", calls.Load())
	}
}

func TestClaudeHTTPAuthSavedRefreshUsesNativeDirectoryAndSyncs(t *testing.T) {
	_, _ = claudeHTTPFixture(t)
	l := savedLogin{Agent: "claude", User: "saved@example.com", On: true,
		Auth: mustJSONRaw(t, map[string]any{"claudeAiOauth": claudeAuth{
			AccessToken: "expired", RefreshToken: "native-owned", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(), Scopes: []string{"user:inference"},
		}}),
		Profile: mustJSONRaw(t, map[string]any{"emailAddress": "saved@example.com", "accountUuid": "saved-account", "organizationUuid": "saved-org"}),
	}
	writeFile(t, loginsPath(), []savedLogin{l})
	old := UsageClaudeVia(func(ctx context.Context, dir string) (string, error) {
		if dir != claudeAccountDir(l.User) {
			t.Fatal("refresh left the selected saved directory")
		}
		writeFile(t, filepath.Join(dir, ".credentials.json"), map[string]any{"claudeAiOauth": claudeAuth{
			AccessToken: "saved-renewed", RefreshToken: "native-rotated", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Scopes: []string{"user:inference"},
		}})
		return "", nil
	})
	t.Cleanup(func() { UsageClaudeVia(old) })
	a := &Account{Agent: "claude", User: l.User, token: func(context.Context) (string, error) {
		t.Fatal("HTTP invoked the inference-token helper")
		return "", nil
	}}
	got, err := ClaudeHTTPAuth(context.Background(), a)
	if err != nil || got.Token != "saved-renewed" || got.AccountUUID != "saved-account" {
		t.Fatalf("saved refresh failed: token matches=%v error=%v", got.Token == "saved-renewed", err)
	}
	kept, _ := parseClaudeCredentials(readLogins()[0].Auth)
	if kept.OAuth.RefreshToken != "native-rotated" {
		t.Fatal("native refresh was not synchronized back to the saved login")
	}
}

func TestClaudeHTTPAuthNativeHelperSuccessCannotAuthorizeExpiredAccess(t *testing.T) {
	_, a := claudeHTTPFixture(t)
	writeFile(t, claudeCredentialsPath(), map[string]any{"claudeAiOauth": claudeAuth{
		AccessToken: "expired", RefreshToken: "native-only", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(), Scopes: []string{"user:inference"},
	}})
	calls := 0
	old := UsageClaudeVia(func(context.Context, string) (string, error) { calls++; return "Current session: 0% used", nil })
	t.Cleanup(func() { UsageClaudeVia(old) })
	if _, err := ClaudeHTTPAuth(context.Background(), a); !errors.Is(err, errClaudeHTTPExpired) || calls != 1 {
		t.Fatalf("successful helper authorized stale access: calls=%d error=%v", calls, err)
	}
}
