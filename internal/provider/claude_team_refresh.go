package provider

import (
	"context"
	"errors"
	"slices"
	"time"
)

// RefreshClaudeTeamOrganization queues a read on the owning controller.
// Repeated clicks coalesce; refreshing never requests a seat transfer.
func RefreshClaudeTeamOrganization(org string) error {
	t, err := claudeTeamForOrg(org, false)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.running {
		return errors.New("Claude Team controller is unavailable")
	}
	if t.refreshing || t.refreshRequested {
		return nil
	}
	t.refreshRequested = true
	t.notify()
	return nil
}

// Called under op, independently of reconciliation: a read must not decide
// to move seats. A pending transaction must finish before publishing seats.
func (t *claudeTeamController) refreshSnapshot(ctx context.Context) error {
	t.mu.Lock()
	c, pending := t.cfg, t.disk.Pending != nil
	t.refreshing = true
	t.signalLocked()
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.refreshing = false
		t.signalLocked()
		t.mu.Unlock()
	}()
	if pending {
		return errors.New("Claude Team seat recovery is pending; retry refresh after recovery")
	}
	read, cancel := context.WithTimeout(ctx, 30*time.Second)
	seats, err := newClaudeTeamAdmin(c).Members(read)
	cancel()
	if err != nil {
		return err // retain the last verified assignments and quota snapshots
	}
	if err := t.synchronizeSeats(c, seats, c.Enabled); err != nil {
		return err
	}
	var failures []error
	for _, seat := range seats {
		if seat.SeatTier != "team_tier_1" || !slices.Contains(c.Members, seat.AccountID) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		t.mu.Lock()
		delete(t.quotaDue, seat.AccountID)
		t.quotaAttempted[seat.AccountID] = true
		t.quotaBusy = seat.AccountID
		t.mu.Unlock()
		probe, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := t.refreshQuota(probe, seat.AccountID)
		cancel()
		if err != nil {
			failures = append(failures, err)
		}
		t.mu.Lock()
		t.quotaBusy = ""
		t.signalLocked()
		t.mu.Unlock()
	}
	return errors.Join(failures...)
}
