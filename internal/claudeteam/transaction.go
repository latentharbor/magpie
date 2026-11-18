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
	Config   Config
	Admin    Admin
	Ready    func(string) error
	Close    func(from, to string) error
	Drain    func(context.Context, string) error
	Restore  func([]Seat) error
	Record   func(Pending) error
	Complete func([]Seat, Pending, string) error
}

func (t Transaction) failed(p Pending,phase string,err error) (Pending,error) {
 p.Failure=safeFailure(phase,err)
 if e:=t.Record(p);e!=nil{return p,errFailureJournal}
 return p,nil
}
func (t Transaction) complete(seats []Seat,p Pending,outcome string) error {
 if outcome=="assigned" {p.Failure=nil}
 if outcome=="rolled_back"&&p.Failure==nil {p.Failure=safeFailure("recovery",nil)}
 if err:=t.Complete(seats,p,outcome);err!=nil{return err}
 if outcome!="assigned"&&p.Failure!=nil{return &TransferFailure{Outcome:outcome,Failure:*p.Failure}}
 return nil
}
func (t Transaction) update(ctx context.Context,p Pending,id,tier,phase,expected string) (Pending,[]Seat,error) {
 err:=t.Admin.SetTier(ctx,id,tier)
 if err!=nil {
  var journal error;p,journal=t.failed(p,phase,err);if journal!=nil{return p,nil,journal}
  // A definite refusal still gets one authoritative read, but does not spend
  // the eventual-consistency wait retrying a write already refused.
  var remote *AdminError
  if errors.As(err,&remote) {
   seats,e:=t.Admin.Members(ctx);if e==nil&&Expected(seats,p.From,p.To,expected,p.Baseline){return p,seats,nil}
   return p,nil,&TransferFailure{Failure:*p.Failure}
  }
 }
 seats,verify:=Verify(ctx,t.Admin,p,expected)
 if verify!=nil {
  if p.Failure==nil {phase:="confirm_release";if expected=="assigned"{phase="confirm_assignment"};var journal error;p,journal=t.failed(p,phase,verify);if journal!=nil{return p,nil,journal}}
  return p,nil,verify
 }
 return p,seats,nil
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
	if err := t.Ready(p.To); err != nil {
		return err
	}
	if err := t.Close(p.From, p.To); err != nil {
		return err
	}
	if err := t.Drain(ctx, p.From); err != nil {
		_ = t.Restore(seats)
		return err
	}
	if _, err := Verify(ctx, t.Admin, p, "original"); err != nil {
		return err
	}
	if err := t.Record(p); err != nil {
		return err
	}
	// A lost reply is not proof that the PUT failed. A complete member read
	// determines the state; an uncertain write is never repeated blindly.
	var err error
	p,_,err=t.update(ctx,p,p.From,"unassigned","release","released")
	if err != nil {
		if errors.Is(err,errFailureJournal){return err}
		return t.Recover(ctx, p)
	}
	p.Failure=nil
	p,current,err:=t.update(ctx,p,p.To,"team_tier_1","assign","assigned")
	if err != nil {
		if errors.Is(err,errFailureJournal){return err}
		return t.Recover(ctx, p)
	}
	return t.complete(current, p, "assigned")
}

func (t Transaction) Recover(ctx context.Context, p Pending) error {
	if !t.valid(p) {
		return errors.New("unfinished Claude Team transfer does not match the configured pool")
	}
	seats, err := t.Admin.Members(ctx)
	if err != nil {
		return err
	}
	switch {
	case Expected(seats, p.From, p.To, "assigned", p.Baseline):
		return t.complete(seats, p, "assigned")
	case Expected(seats, p.From, p.To, "original", p.Baseline):
		return t.complete(seats, p, "unchanged")
	case Expected(seats, p.From, p.To, "released", p.Baseline):
		if err:=t.Admin.SetTier(ctx,p.From,"team_tier_1");err!=nil {
   p.RecoveryFailure=safeFailure("restore",err);if journal:=t.Record(p);journal!=nil{return errFailureJournal}
  }
		seats, err = Verify(ctx, t.Admin, p, "original")
		if err != nil {
			return err
		}
		return t.complete(seats, p, "rolled_back")
	default:
		return errors.New("Claude Team assignments changed concurrently; check the organization before retrying")
	}
}

func PremiumSet(seats []Seat) []string {
	var out []string
	for _, s := range seats {
		if s.SeatTier == "team_tier_1" {
			out = append(out, s.AccountID)
		}
	}
	slices.Sort(out)
	return out
}

func Expected(seats []Seat, from, to, phase string, baseline []string) bool {
	states := map[string]string{}
	for _, s := range seats {
		if _, exists := states[s.AccountID]; exists {
			return false
		}
		states[s.AccountID] = s.SeatTier
	}
	want := slices.Clone(baseline)
	if phase != "original" {
		want = slices.DeleteFunc(want, func(s string) bool { return s == from })
	}
	if phase == "assigned" {
		want = append(want, to)
		slices.Sort(want)
	}
	f, n := "team_tier_1", "unassigned"
	if phase != "original" {
		f = "unassigned"
	}
	if phase == "assigned" {
		n = "team_tier_1"
	}
	return states[from] == f && states[to] == n && slices.Equal(PremiumSet(seats), want)
}

func Verify(ctx context.Context, client Admin, p Pending, phase string) ([]Seat, error) {
	bounded,cancel:=context.WithTimeout(ctx,10*time.Second);defer cancel()
	for attempts:=0;attempts<40;attempts++ {
		seats, err := client.Members(bounded)
		if err != nil {
			return nil, err
		}
		if Expected(seats, p.From, p.To, phase, p.Baseline) {
			return seats, nil
		}
		select {
		case <-bounded.Done():
			return nil, bounded.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil, errors.New("Claude Team assignment differs from the verified plan; automatic writes stopped")
}
