package provider

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

func completeClaudeTeamQuota(ws []QuotaWindow) bool {
	for _, name := range []string{"5 hours", "7 days"} {
		i := slices.IndexFunc(ws, func(w QuotaWindow) bool { return w.Name == name })
		if i < 0 {
			return false
		}
		w := ws[i]
		if math.IsNaN(w.Used) || math.IsInf(w.Used, 0) || w.Used < 0 || w.Used > 100 || w.Used > 0 && (w.ResetsAt == nil || w.ResetsAt.IsZero()) {
			return false
		}
	}
	return true
}

// queueQuotaLocked is local only. A read never assigns a cloud seat, and a
// pending read never gates inference. mu is held; a controller wake runs it.
func (t *claudeTeamController) queueQuotaLocked(id string) {
	// A re-login during a read must get another attempt with the new token.
	t.quotaDue[id], t.quotaAttempted[id] = true, true
}

// New/re-authenticated accounts already selected in a Team are captured as
// soon as login succeeds. An account selected later is queued by config save.
func captureClaudeTeamLogin(l savedLogin) {
	id, ok := savedSessionIdentity(l)
	if !ok || id.Agent != "claude" {
		return
	}
	for _, t := range claudeTeamControllers() {
		t.mu.Lock()
		selected := strings.EqualFold(t.cfg.OrganizationID, id.OrganizationID) && slices.Contains(t.cfg.Members, strings.ToLower(id.AccountID))
		if selected {
			t.users[strings.ToLower(l.User)] = strings.ToLower(id.AccountID)
			t.queueQuotaLocked(strings.ToLower(id.AccountID))
		}
		t.mu.Unlock()
		if selected {
			t.notify()
		}
	}
}

func (t *claudeTeamController) collectQuotaDue(ctx context.Context) {
	t.mu.Lock()
	ids := make([]string, 0, len(t.quotaDue))
	for id := range t.quotaDue {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	slices.Sort(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		t.mu.Lock()
		delete(t.quotaDue, id)
		selected := slices.Contains(t.cfg.Members, id)
		if selected {
			t.quotaBusy = id
		}
		t.mu.Unlock()
		if !selected {
			continue
		}
		probe, cancel := context.WithTimeout(ctx, 20*time.Second)
		_ = t.refreshQuota(probe, id)
		cancel()
		t.mu.Lock()
		t.quotaBusy = ""
		t.signalLocked()
		t.mu.Unlock()
	}
}

// Retry requests only a quota read. Even an unassigned account may be retried
// after an external assignment; failure leaves its saved snapshot untouched.
func RetryClaudeTeamQuota(org, id string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	t, err := claudeTeamForOrg(org, false)
	if err != nil {
		return err
	}
	t.mu.Lock()
	if !slices.Contains(t.cfg.Members, id) {
		t.mu.Unlock()
		return errors.New("quota collection requires a selected Team member")
	}
	t.queueQuotaLocked(id)
	t.mu.Unlock()
	t.notify()
	startClaudeTeam(t)
	return nil
}
