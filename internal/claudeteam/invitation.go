package claudeteam

import (
	"context"
	"errors"
	"slices"
	"time"
)

const WeeklyInvitation = "weekly-invite"

var ErrInvitationPending = errors.New("Claude Team replacement is awaiting invitation acceptance and verified Premium access; sign in to the invited account on claude.ai")

type InvitationAdmin interface {
	Admin
	RemoveMember(context.Context, string) error
	InvitePremium(context.Context, string) error
}

// A 5h reset reuses a current Premium, and Fable's scoped cap never evicts a
// member still able to serve other models. Only the actual selected Premium
// cohort's global weekly exhaustion starts a batch, for any cohort size.
func WeeklyBatch(c Config, seats []Seat, limits map[string][]Limit, now time.Time) []string {
	var batch []string
	for _, s := range seats {
		if s.SeatTier != "team_tier_1" || !slices.Contains(c.Members, s.AccountID) {
			continue
		}
		if !Spent([]Limit{weeklyLimit(limits[s.AccountID])}, c.Threshold, now) {
			return nil
		}
		batch = append(batch, s.AccountID)
	}
	return batch
}

func weeklyLimit(limits []Limit) Limit {
	for _, w := range limits {
		if w.Kind == "seven_day" {
			return w
		}
	}
	return Limit{}
}

// Each cloud write is preceded by a durable stage. Recovery only reads: it
// never repeats a possibly accepted invitation, removal or tier update. In
// particular, a pending invite may reserve the free seat before acceptance.
type InvitationTransaction struct {
	Transaction
	Membership InvitationAdmin
}

func invitationExpected(seats []Seat, p Pending, phase string) bool {
	states := map[string]string{}
	for _, s := range seats {
		if _, duplicate := states[s.AccountID]; duplicate || s.SeatTier == "" {
			return false
		}
		states[s.AccountID] = s.SeatTier
	}
	want := slices.Clone(p.Baseline)
	if phase != "original" {
		want = slices.DeleteFunc(want, func(id string) bool { return id == p.From })
	}
	if phase == "assigned" {
		want = append(want, p.To)
		slices.Sort(want)
	}
	if !slices.Equal(PremiumSet(seats), want) {
		return false
	}
	switch phase {
	case "original":
		return states[p.From] == "team_tier_1" && states[p.To] == ""
	case "released":
		return states[p.From] == "unassigned" && states[p.To] == ""
	case "removed":
		return states[p.From] == "" && states[p.To] == ""
	case "assigned":
		return states[p.From] == "" && states[p.To] == "team_tier_1"
	}
	return false
}

func (t InvitationTransaction) valid(p Pending) bool {
	return t.Transaction.valid(p) && p.Kind == WeeklyInvitation && validInvitationEmail(p.InviteEmail)
}

func (t InvitationTransaction) verify(ctx context.Context, p Pending, phase string) ([]Seat, error) {
	seats, err := t.Membership.Members(ctx)
	if err != nil {
		return nil, err
	}
	if !invitationExpected(seats, p, phase) {
		return nil, errors.New("Claude Team invitation state is not confirmed; cloud writes stopped")
	}
	return seats, nil
}

func (t InvitationTransaction) stage(p Pending, stage string) (Pending, error) {
	p.Stage = stage
	return p, t.Record(p)
}

func (t InvitationTransaction) Transfer(ctx context.Context, seats []Seat, p Pending) error {
	if !t.valid(p) || !invitationExpected(seats, p, "original") {
		return errors.New("invitation replacement needs a Premium source and a target already outside the organization")
	}
	// Unknown roles are not permission to remove an administrator or owner.
	i := slices.IndexFunc(seats, func(s Seat) bool { return s.AccountID == p.From })
	if i < 0 || seats[i].Role != "user" {
		return errors.New("only a verified ordinary member can be removed by invitation replacement")
	}
	// The saved identity supplies the email. A removed account may need to
	// sign in again after the invitation; its old OAuth grant cannot prove
	// readiness before it is allowed back into this organization.
	if err := t.Close(p.From, p.To); err != nil {
		return err
	}
	if err := t.Drain(ctx, p.From); err != nil {
		_ = t.Restore(seats)
		return err
	}
	current, err := t.verify(ctx, p, "original")
	if err != nil {
		return err
	}
	i = slices.IndexFunc(current, func(s Seat) bool { return s.AccountID == p.From })
	if i < 0 || current[i].Role != "user" {
		return errors.New("source membership role changed while draining; cloud writes stopped")
	}
	if p, err = t.stage(p, "releasing"); err != nil {
		return err
	}
	if err = t.Membership.SetTier(ctx, p.From, "unassigned"); err != nil {
		return t.failedWrite(ctx, p, "release", err)
	}
	if _, err = t.verify(ctx, p, "released"); err != nil {
		return t.failedWrite(ctx, p, "confirm_release", err)
	}
	if p, err = t.stage(p, "removing"); err != nil {
		return err
	}
	if err = t.Membership.RemoveMember(ctx, p.From); err != nil {
		return t.failedWrite(ctx, p, "remove", err)
	}
	if _, err = t.verify(ctx, p, "removed"); err != nil {
		return t.failedWrite(ctx, p, "confirm_removal", err)
	}
	if p, err = t.stage(p, "inviting"); err != nil {
		return err
	}
	if err = t.Membership.InvitePremium(ctx, p.InviteEmail); err != nil {
		return t.failedWrite(ctx, p, "invite", err)
	}
	if p, err = t.stage(p, "awaiting_acceptance"); err != nil {
		return err
	}
	return t.Recover(ctx, p)
}

func (t InvitationTransaction) failedWrite(ctx context.Context, p Pending, phase string, cause error) error {
	p.Failure = safeFailure(phase, cause)
	if err := t.Record(p); err != nil {
		return errFailureJournal
	}
	return t.Recover(ctx, p)
}

func (t InvitationTransaction) Recover(ctx context.Context, p Pending) error {
	if !t.valid(p) {
		return errors.New("unfinished invitation does not match this organization and saved account pool")
	}
	seats, err := t.Membership.Members(ctx)
	if err != nil {
		return err
	}
	if invitationExpected(seats, p, "assigned") {
		if err := t.Ready(p.To); err != nil {
			return err
		}
		return t.complete(seats, p, "assigned")
	}
	if invitationExpected(seats, p, "original") && p.Stage == "releasing" {
		return t.complete(seats, p, "unchanged")
	}
	if !invitationExpected(seats, p, "released") && !invitationExpected(seats, p, "removed") {
		return errors.New("Claude Team membership changed concurrently; invitation recovery requires review")
	}
	// Other verified Premium members keep serving during external acceptance.
	if err := t.Restore(seats); err != nil {
		return err
	}
	if p.Failure != nil {
		return &TransferFailure{Outcome: "pending", Failure: *p.Failure}
	}
	return ErrInvitationPending
}
