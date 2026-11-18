package provider

import (
	"time"

	"github.com/yetone/magpie/internal/claudeteam"
)

func hasAutomaticPause(pauses map[string]claudeteam.AutomaticPause, id string) bool {
	_, ok := pauses[id]
	return ok
}

func (t *claudeTeamController) refreshAutomaticPauses(seats []claudeTeamSeat) error {
	t.mu.Lock()
	dirty := false
	count := len(premiumSet(seats))
	if r := t.disk.MovementLimit; r != nil && (!r.Active(time.Now()) || count > r.Seats) {
		t.disk.MovementLimit = nil
		dirty = true
	}
	for id, pause := range t.disk.AutomaticPauses {
		premium := false
		for _, s := range seats {
			if s.AccountID == id {
				premium = s.SeatTier == "team_tier_1"
				break
			}
		}
		if !premium || !pause.Active(time.Now()) || count > pause.Seats {
			delete(t.disk.AutomaticPauses, id)
			dirty = true
		}
	}
	t.mu.Unlock()
	if dirty {
		return t.persist()
	}
	return nil
}

// A cap on moving seats does not disable inference or discovery. Requests do
// not repeatedly wake administration for a known organization-wide refusal.
// Recovery, explicit refresh and the ordinary membership poll still run.
func (t *claudeTeamController) movementBlockedLocked(now time.Time) bool {
	r := t.disk.MovementLimit
	if r == nil || !r.Active(now) || r.Workflow != movementWorkflow(t.cfg) {
		return false
	}
	n := 0
	for _, tier := range t.seats {
		if tier == "team_tier_1" {
			n++
		}
	}
	return n <= r.Seats
}

func (t *claudeTeamController) checkMovementLimit(seats []claudeTeamSeat, manual string) error {
	t.mu.Lock()
	r := t.disk.MovementLimit
	if r == nil {
		t.mu.Unlock()
		return nil
	}
	if r.Workflow != movementWorkflow(t.cfg) {
		t.mu.Unlock()
		return nil
	}
	if !r.Active(time.Now()) || len(premiumSet(seats)) > r.Seats {
		t.disk.MovementLimit = nil
		t.mu.Unlock()
		return t.persist()
	}
	t.mu.Unlock()
	// A user may have bought capacity or selected a server-allowed recent
	// holder. One explicit rotation is a probe, never a bypass of the cap.
	if manual != "" {
		return nil
	}
	return r
}
