package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/appdir"
)

// ClaudeHTTPCredentials is the selected account's OAuth access and public
// identity for an explicitly selected HTTP transport. It contains no refresh
// token: Claude Code remains the only owner of refresh-token rotation.
type ClaudeHTTPCredentials struct {
	Token, AccountUUID, OrganizationUUID, DeviceID string
	LoginVersion                                   string
}

var errClaudeHTTPExpired = errors.New("Claude HTTP OAuth access token expired; refresh this account in Claude Code, then retry")

var claudeHTTPRefreshing = struct {
	sync.Mutex
	pending map[string]chan struct{}
}{pending: map[string]chan struct{}{}}

// ClaudeHTTPAuth rereads the selected credential store on each call. Only an
// expired, otherwise complete sign-in delegates a bounded /usage to native
// Claude Code, which owns refresh locking. Concurrent requests share that
// attempt; no refresh token is handled by the HTTP transport.
func ClaudeHTTPAuth(ctx context.Context, a *Account) (ClaudeHTTPCredentials, error) {
	creds, err := readClaudeHTTPAuth(ctx, a)
	if !errors.Is(err, errClaudeHTTPExpired) {
		return creds, err
	}
	key := claudeProfilePath() + "\x00" + strings.ToLower(a.User)
	claudeHTTPRefreshing.Lock()
	if done := claudeHTTPRefreshing.pending[key]; done != nil {
		claudeHTTPRefreshing.Unlock()
		select {
		case <-ctx.Done():
			return ClaudeHTTPCredentials{}, ctx.Err()
		case <-done:
			return readClaudeHTTPAuth(ctx, a)
		}
	}
	done := make(chan struct{})
	claudeHTTPRefreshing.pending[key] = done
	claudeHTTPRefreshing.Unlock()
	defer func() {
		claudeHTTPRefreshing.Lock()
		delete(claudeHTTPRefreshing.pending, key)
		close(done)
		claudeHTTPRefreshing.Unlock()
	}()
	// Another native process may have renewed between the initial read and
	// admission. A missing expiry is incomplete metadata, never permission to
	// invoke a refresh; readClaudeHTTPAuth classifies it separately.
	if creds, err = readClaudeHTTPAuth(ctx, a); !errors.Is(err, errClaudeHTTPExpired) {
		return creds, err
	}
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if a.token != nil {
		_ = inSavedClaudeDir(a.User, func(dir string) error {
			_, err := runClaudeUsage(ViaLogin(work, "claude", a.User), dir)
			return err
		})
	} else {
		_, _ = runClaudeUsage(ViaLogin(work, "claude", a.User), appdir.Getenv("CLAUDE_CONFIG_DIR"))
	}
	// A denied /usage may still have renewed access successfully. Conversely a
	// successful helper is no proof: only the authoritative store authorizes it.
	return readClaudeHTTPAuth(ctx, a)
}

func readClaudeHTTPAuth(ctx context.Context, a *Account) (ClaudeHTTPCredentials, error) {
	if err := ctx.Err(); err != nil {
		return ClaudeHTTPCredentials{}, err
	}
	if a == nil || a.Agent != "claude" {
		return ClaudeHTTPCredentials{}, errors.New("Claude HTTP transport requires a Claude account")
	}
	if discoveryOff() {
		return ClaudeHTTPCredentials{}, errAccountDiscoveryOff
	}
	var c claudeCredentials
	var profile []byte
	if a.token != nil {
		// The saved directory is authoritative after Claude Code has refreshed
		// there. Keep its handoff guard and synchronize logins.json as usual.
		err := inSavedClaudeDir(a.User, func(dir string) error {
			var ok bool
			c, ok = readClaudeDir(dir)
			if !ok {
				return errors.New("Claude HTTP transport could not read the saved OAuth sign-in")
			}
			loginsMu.Lock()
			defer loginsMu.Unlock()
			for _, l := range readLogins() {
				if l.Agent == "claude" && strings.EqualFold(l.User, a.User) {
					profile = slices.Clone(l.Profile)
					return nil
				}
			}
			return errors.New("Claude HTTP transport could not read the saved account identity")
		})
		if err != nil {
			return ClaudeHTTPCredentials{}, err
		}
	} else {
		var ok bool
		// Avoid the discovery cache: the CLI may have rotated or cleared the
		// credential since the provider list was built.
		c, _, ok = readClaudeCredential()
		if !ok {
			return ClaudeHTTPCredentials{}, errors.New("Claude HTTP transport could not read Claude Code's OAuth sign-in")
		}
		acct, ok := claudeProfileAccount()
		if !ok {
			return ClaudeHTTPCredentials{}, errors.New("Claude HTTP transport could not read Claude Code's account identity")
		}
		email, _ := acct["emailAddress"].(string)
		if !strings.EqualFold(claudeUser(strings.TrimSpace(email), c.OAuth.SubscriptionType, acct), strings.TrimSpace(a.User)) {
			return ClaudeHTTPCredentials{}, errors.New("Claude Code's signed-in account changed; select the current account and retry")
		}
		profile, _ = json.Marshal(acct)
		loginsMu.Lock()
		for _, l := range readLogins() {
			if l.Agent == "claude" && strings.EqualFold(l.User, a.User) && l.Refused == c.version() && l.Lapsed != "" {
				loginsMu.Unlock()
				return ClaudeHTTPCredentials{}, errors.New(l.Lapsed)
			}
		}
		loginsMu.Unlock()
	}
	if !slices.Contains(c.OAuth.Scopes, "user:inference") {
		return ClaudeHTTPCredentials{}, errors.New("Claude HTTP transport requires an OAuth sign-in with user:inference scope")
	}
	if c.OAuth.ExpiresAt <= 0 {
		return ClaudeHTTPCredentials{}, errors.New("Claude HTTP OAuth access token has no expiry; refresh this account in Claude Code, then retry")
	}
	id, ok := claudeSessionIdentity(profile)
	if !ok || id.OrganizationID == "" {
		return ClaudeHTTPCredentials{}, errors.New("Claude HTTP transport requires the selected account's accountUuid and organizationUuid")
	}
	// userID belongs to this installation, shared by its own saved accounts.
	// A saved profile's userID is not a device identity and is never substituted.
	state, err := readClaudeProfile()
	if err != nil {
		return ClaudeHTTPCredentials{}, fmt.Errorf("Claude HTTP transport could not read this device's Claude Code state: %w", err)
	}
	device, _ := state["userID"].(string)
	if strings.TrimSpace(device) == "" {
		return ClaudeHTTPCredentials{}, errors.New("Claude HTTP transport requires this device's userID in Claude Code's .claude.json")
	}
	if err := ctx.Err(); err != nil {
		return ClaudeHTTPCredentials{}, err
	}
	if c.OAuth.ExpiresAt <= time.Now().UnixMilli() {
		return ClaudeHTTPCredentials{}, errClaudeHTTPExpired
	}
	return ClaudeHTTPCredentials{Token: c.OAuth.AccessToken, AccountUUID: id.AccountID, OrganizationUUID: id.OrganizationID, DeviceID: device, LoginVersion: c.version()}, nil
}
