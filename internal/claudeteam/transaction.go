package claudeteam

import (
	"context"
	"errors"
	"slices"
	"time"
)

// Transaction's host performs only local admission, draining and persistence.
// No callback sends inference or obtains an account's credentials. Record must
// durably commit the journal before returning, and Complete clears it only
// after routing and the verified assignments have been durably saved.
type Transaction struct {
	Config Config
	Admin Admin
	Ready func(string) error
	Close func(from, to string) error
	Drain func(context.Context, string) error
	Restore func([]Seat) error
	Record func(Pending) error
	Complete func([]Seat, Pending, string) error
}

func (t Transaction) valid(p Pending) bool {
	return p.OrganizationID == t.Config.OrganizationID && p.From != p.To &&
		slices.Contains(t.Config.Members, p.From) && slices.Contains(t.Config.Members, p.To) &&
		slices.Contains(p.Baseline, p.From) && !slices.Contains(p.Baseline, p.To)
}

func (t Transaction) Transfer(ctx context.Context, seats []Seat, p Pending) error {
	if !t.valid(p) {
		return errors.New("unfinished Claude Team transfer does not match the configured pool")
	}
	if err := t.Ready(p.To); err != nil { return err }
	if err := t.Close(p.From, p.To); err != nil { return err }
	if err := t.Drain(ctx, p.From); err != nil {
		_ = t.Restore(seats)
		return err
	}
	if _, err := Verify(ctx, t.Admin, p, "original"); err != nil { return err }
	if err := t.Record(p); err != nil { return err }
	// A lost reply is not proof that the PUT failed. A complete member read
	// determines the state; an uncertain write is never repeated blindly.
	_ = t.Admin.SetTier(ctx, p.From, "unassigned")
	if _, err := Verify(ctx, t.Admin, p, "released"); err != nil { return t.Recover(ctx, p) }
	_ = t.Admin.SetTier(ctx, p.To, "team_tier_1")
	current, err := Verify(ctx, t.Admin, p, "assigned")
	if err != nil { return t.Recover(ctx, p) }
	return t.Complete(current, p, "assigned")
}

func (t Transaction) Recover(ctx context.Context, p Pending) error {
	if !t.valid(p) {
		return errors.New("unfinished Claude Team transfer does not match the configured pool")
	}
	seats, err := t.Admin.Members(ctx)
	if err != nil { return err }
	switch {
	case Expected(seats, p.From, p.To, "assigned", p.Baseline):
		return t.Complete(seats, p, "assigned")
	case Expected(seats, p.From, p.To, "original", p.Baseline):
		return t.Complete(seats, p, "unchanged")
	case Expected(seats, p.From, p.To, "released", p.Baseline):
		_ = t.Admin.SetTier(ctx, p.From, "team_tier_1")
		seats, err = Verify(ctx, t.Admin, p, "original")
		if err != nil { return err }
		return t.Complete(seats, p, "rolled_back")
	default:
		return errors.New("Claude Team assignments changed concurrently; check the organization before retrying")
	}
}

func PremiumSet(seats []Seat) []string {
	var out []string
	for _, s := range seats { if s.SeatTier == "team_tier_1" { out = append(out, s.AccountID) } }
	slices.Sort(out)
	return out
}

func Expected(seats []Seat, from, to, phase string, baseline []string) bool {
	states := map[string]string{}
	for _, s := range seats {
		if _, exists := states[s.AccountID]; exists { return false }
		states[s.AccountID] = s.SeatTier
	}
	want := slices.Clone(baseline)
	if phase != "original" { want = slices.DeleteFunc(want, func(s string) bool { return s == from }) }
	if phase == "assigned" { want = append(want, to); slices.Sort(want) }
	f, n := "team_tier_1", "unassigned"
	if phase != "original" { f = "unassigned" }
	if phase == "assigned" { n = "team_tier_1" }
	return states[from] == f && states[to] == n && slices.Equal(PremiumSet(seats), want)
}

func Verify(ctx context.Context, client Admin, p Pending, phase string) ([]Seat, error) {
	for range 5 {
		seats, err := client.Members(ctx)
		if err != nil { return nil, err }
		if Expected(seats, p.From, p.To, phase, p.Baseline) { return seats, nil }
		select {
		case <-ctx.Done(): return nil, ctx.Err()
		case <-time.After(250*time.Millisecond):
		}
	}
	return nil, errors.New("Claude Team assignment differs from the verified plan; automatic writes stopped")
}
