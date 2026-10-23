package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ClaudeHTTPCredentials is the selected account's OAuth access and public
// identity for an explicitly selected HTTP transport. It contains no refresh
// token: Claude Code remains the only owner of refresh-token rotation.
type ClaudeHTTPCredentials struct {
	Token, AccountUUID, OrganizationUUID, DeviceID string
}

// ClaudeHTTPAuth reads the selected Claude account without running Claude Code
// or refreshing its credentials. Every call reads the current credential store,
// so a native refresh is picked up on the next request. Expired or incomplete
// credentials fail explicitly; rotating them without Claude Code's native lock
// would risk spending the refresh token while another process holds it.
func ClaudeHTTPAuth(ctx context.Context, a *Account) (ClaudeHTTPCredentials, error) {
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
	if c.OAuth.ExpiresAt <= time.Now().UnixMilli() {
		return ClaudeHTTPCredentials{}, errors.New("Claude HTTP OAuth access token expired or has no expiry; refresh this account in Claude Code, then retry (HTTP transport does not rotate shared refresh tokens)")
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
	return ClaudeHTTPCredentials{Token: c.OAuth.AccessToken, AccountUUID: id.AccountID, OrganizationUUID: id.OrganizationID, DeviceID: device}, nil
}
