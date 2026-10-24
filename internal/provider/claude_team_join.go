package provider

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/filelock"
)

// Upgrade existing standalone logins once per organization. The journal marker
// makes later, deliberate pool exclusions survive a restart. Only newly joined
// accounts are captured; this never cycles a seat or re-reads the whole pool.
func (t *claudeTeamController) adoptSavedLogins() error {
	t.mu.Lock()
	initialized, org := t.disk.MembershipInitialized, t.cfg.OrganizationID
	t.mu.Unlock()
	if initialized || org == "" {
		return nil
	}
	for _, l := range readLogins() {
		id, ok := savedSessionIdentity(l)
		if !ok || id.Agent != "claude" || !strings.EqualFold(id.OrganizationID, org) || !claudeTeamUUID.MatchString(id.AccountID) {
			continue
		}
		t.mu.Lock()
		selected := slices.Contains(t.cfg.Members, strings.ToLower(id.AccountID))
		t.mu.Unlock()
		if !selected {
			if err := t.joinLogin(l, strings.ToLower(id.AccountID)); err != nil {
				return err
			}
		}
	}
	t.op.Lock()
	defer t.op.Unlock()
	t.mu.Lock()
	t.disk.MembershipInitialized = true
	t.mu.Unlock()
	if err := t.persist(); err != nil {
		t.mu.Lock()
		t.disk.MembershipInitialized = false
		t.mu.Unlock()
		return err
	}
	return nil
}

// joinClaudeTeamLogin adopts a saved Claude profile into its configured
// organization. Login never needs a successful administrator request or a seat
// transfer: new members stay closed until reconciliation verifies their seat.
func joinClaudeTeamLogin(l savedLogin) error {
	id, ok := savedSessionIdentity(l)
	if !ok || id.Agent != "claude" || !claudeTeamUUID.MatchString(id.OrganizationID) || !claudeTeamUUID.MatchString(id.AccountID) {
		return nil
	}
	t, err := claudeTeamForOrg(id.OrganizationID, false)
	if err != nil {
		return nil // a standalone subscription, not a new Team configuration
	}
	if err := t.joinLogin(l, strings.ToLower(id.AccountID)); err != nil {
		return err
	}
	t.notify()
	startClaudeTeam(t)
	return nil
}

func (t *claudeTeamController) joinLogin(l savedLogin, id string) error {
	t.op.Lock()
	defer t.op.Unlock()
	if err := t.selectPath(); err != nil {
		return err
	}
	t.mu.Lock()
	running := t.running
	t.mu.Unlock()
	if !running {
		unlock, held := filelock.Lock(t.configPath()+".controller.lock", 0)
		if !held {
			return errors.New("another process owns the Claude Team controller; use its configuration API")
		}
		defer unlock()
	}
	c, err := t.readConfig()
	if err != nil {
		return err
	}
	identity, ok := savedSessionIdentity(l)
	if !ok || !strings.EqualFold(identity.OrganizationID, c.OrganizationID) {
		return errors.New("Claude login belongs to another organization")
	}
	if err := t.loadState(c); err != nil {
		return err
	}
	joining := !slices.Contains(c.Members, id)
	t.mu.Lock()
	pending := t.disk.Pending != nil
	t.mu.Unlock()
	if joining && pending {
		return errors.New("reconcile the unfinished Claude Team transfer before adding an account")
	}
	if joining {
		c.Members = append(slices.Clone(c.Members), id)
		if err := validateClaudeTeam(&c); err != nil {
			return err
		}
		if c.Enabled {
			t.mu.Lock()
			t.disk.Original[id] = claudeTeamFlags{l.On, l.Paused, l.TeamOff}
			t.closed[id] = true
			t.mu.Unlock()
			if err := t.persist(); err != nil {
				return err
			}
		}
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		if err := writePrivate(t.configPath(), append(b, '\n')); err != nil {
			return err
		}
	}
	t.mu.Lock()
	t.cfg = c
	t.users[strings.ToLower(l.User)] = id
	t.queueQuotaLocked(id)
	t.mu.Unlock()
	if joining && c.Enabled {
		return t.route(c, map[string]bool{id: false})
	}
	return nil
}
