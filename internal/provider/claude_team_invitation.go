package provider

import (
	"errors"
	"slices"
	"time"

	"github.com/yetone/magpie/internal/claudeteam"
)

func (t *claudeTeamController) snapshotSeatsLocked() []claudeTeamSeat {
	seats := make([]claudeTeamSeat, 0, len(t.seats))
	for id, tier := range t.seats {
		seats = append(seats, claudeTeamSeat{AccountID: id, SeatTier: tier})
	}
	return seats
}

func (t *claudeTeamController) weeklyReplacementRequestedLocked() bool {
	return len(t.disk.ReplacementBatch) > 0 || len(claudeteam.WeeklyBatch(t.cfg, t.snapshotSeatsLocked(), t.disk.Limits, time.Now())) > 0
}

// Retain the exhausted cohort across each sequential invitation. A newly
// accepted member's fresh week must not stop replacement of the old cohort.
func (t *claudeTeamController) prepareWeeklyBatch(c ClaudeTeamConfig, seats []claudeTeamSeat) (bool, error) {
	t.mu.Lock()
	before := slices.Clone(t.disk.ReplacementBatch)
	t.disk.ReplacementBatch = slices.DeleteFunc(slices.Clone(before), func(id string) bool {
		return !slices.Contains(c.Members, id) || !slices.ContainsFunc(seats, func(s claudeTeamSeat) bool { return s.AccountID == id && s.SeatTier == "team_tier_1" }) ||
			len(claudeteam.WeeklyBatch(c, []claudeTeamSeat{{AccountID: id, SeatTier: "team_tier_1"}}, t.disk.Limits, time.Now())) == 0
	})
	if len(t.disk.ReplacementBatch) == 0 {
		t.disk.ReplacementBatch = claudeteam.WeeklyBatch(c, seats, t.disk.Limits, time.Now())
	}
	dirty, ready := !slices.Equal(before, t.disk.ReplacementBatch), len(t.disk.ReplacementBatch) > 0
	t.mu.Unlock()
	if dirty {
		if err := t.persist(); err != nil {
			return false, err
		}
	}
	return ready, nil
}

func (t *claudeTeamController) invitationTransaction(c ClaudeTeamConfig, client claudeTeamAdmin) (claudeteam.InvitationTransaction, error) {
	admin, ok := client.(claudeteam.InvitationAdmin)
	if !ok {
		return claudeteam.InvitationTransaction{}, errors.New("Claude Team administrator does not support invitation replacement")
	}
	transaction := t.transaction(c, client)
	transaction.Record = func(p claudeTeamPending) error {
		t.mu.Lock()
		t.disk.Pending = &p
		if p.Failure != nil && p.Failure.Kind == "seat_movement_limit" {
			t.disk.MovementLimit = &claudeteam.MovementLimit{Workflow: claudeteam.WeeklyInvitation, RetryDate: p.Failure.RetryDate, Seats: len(p.Baseline)}
		}
		t.mu.Unlock()
		return t.persist()
	}
	return claudeteam.InvitationTransaction{Transaction: transaction, Membership: admin}, nil
}

func movementWorkflow(c ClaudeTeamConfig) string {
	if c.ReplacementMode == claudeteam.WeeklyInvitation {
		return claudeteam.WeeklyInvitation
	}
	return ""
}
