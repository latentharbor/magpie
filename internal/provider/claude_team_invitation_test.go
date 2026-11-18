package provider

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/claudeteam"
)

type invitingTeamAdmin struct {
	*fakeTeamAdmin
	removed []string
	invited []string
}

func (a *invitingTeamAdmin) RemoveMember(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.removed = append(a.removed, id)
	a.seats = slices.DeleteFunc(a.seats, func(s claudeTeamSeat) bool { return s.AccountID == id })
	return nil
}
func (a *invitingTeamAdmin) InvitePremium(_ context.Context, email string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.invited = append(a.invited, email)
	return nil
}

func TestClaudeTeamWeeklyInvitationDoesNotMoveSeatsForFiveHourOrFableExhaustion(t *testing.T) {
	tm, f, c := teamFixture(t)
	usage := UsageClaudeVia(func(context.Context, string) (string, error) { return "", errors.New("fixture quota unavailable") })
	t.Cleanup(func() { UsageClaudeVia(usage) })
	a := &invitingTeamAdmin{fakeTeamAdmin:f}
	newClaudeTeamAdmin = func(ClaudeTeamConfig) claudeTeamAdmin { return a }
	f.mu.Lock()
	f.seats = slices.DeleteFunc(f.seats, func(s claudeTeamSeat) bool { return s.AccountID == teamID(3) })
	for i := range f.seats { f.seats[i].Role = "user" }
	f.mu.Unlock()
	c.ReplacementMode = claudeteam.WeeklyInvitation
	if err := tm.save(context.Background(), c); err != nil { t.Fatal(err) }
	future := time.Now().Add(24*time.Hour).Unix()
	for _, user := range []string{"u2@example.com · Team", "u4@example.com · Team"} {
		tm.note(user, []ClaudeLimit{{Kind:"five_hour",Used:1,ResetsAt:future},{Kind:"seven_day",Used:.4,ResetsAt:future},{Kind:"seven_day_overage_included",Used:1,ResetsAt:future}})
	}
	tm.note("u3@example.com · Team", []ClaudeLimit{{Kind:"five_hour",Used:0},{Kind:"seven_day",Used:0},{Kind:"seven_day_overage_included",Used:0}})
	if err:=tm.reconcile(context.Background(),"");err!=nil { t.Fatal(err) }
	if ObserveClaudeTeamModel("u2@example.com · Team","claude-fable-5-2")!=true { t.Fatal("model exhaustion was not retained") }
	if err:=tm.reconcile(context.Background(),"");err!=nil { t.Fatal(err) }
	if len(f.writes)!=0 || len(a.removed)!=0 || len(a.invited)!=0 || tm.disk.Pending!=nil || len(tm.disk.ReplacementBatch)!=0 {
		t.Fatal("five-hour/Fable cap consumed a member change")
	}
	tm.note("u2@example.com · Team", []ClaudeLimit{{Kind:"seven_day",Used:1,ResetsAt:future}})
	if err:=tm.reconcile(context.Background(),"");err!=nil || len(f.writes)!=0 { t.Fatal("partly exhausted cohort was replaced",err) }
	tm.note("u4@example.com · Team", []ClaudeLimit{{Kind:"seven_day",Used:1,ResetsAt:future}})
	if err:=tm.reconcile(context.Background(),"");!errors.Is(err,claudeteam.ErrInvitationPending) {t.Fatal("weekly batch did not start a guarded invitation",err)}
	if !slices.Equal(f.writes,[]string{teamID(2)+":unassigned"}) || !slices.Equal(a.removed,[]string{teamID(2)}) || !slices.Equal(a.invited,[]string{"u3@example.com"}) {
		t.Fatal("weekly handoff changed another member or used the display name as an email",f.writes,a.removed,a.invited)
	}
	if p:=tm.disk.Pending;p==nil || p.Kind!=claudeteam.WeeklyInvitation || p.Stage!="awaiting_acceptance" {t.Fatal("invitation was not durably retained")}
	if v:=tm.status();!slices.ContainsFunc(v.Members,func(m ClaudeTeamMember)bool{return m.AccountID==teamID(3) && m.InvitationPending && !m.On}) {t.Fatal("unaccepted target was enabled or hidden")}
	// Reading a pending invitation many times must never release/reinvite again.
	for range 10 { _=tm.reconcile(context.Background(),"") }
	if len(f.writes)!=1 || len(a.invited)!=1 {t.Fatal("pending invitation repeated cloud writes")}
	f.mu.Lock()
	f.seats=append(f.seats,claudeTeamSeat{AccountID:teamID(3),SeatTier:"team_tier_1",Role:"user"})
	f.mu.Unlock()
	if err:=tm.reconcile(context.Background(),"");err!=nil {t.Fatal("accepted invitation failed to restore native admission",err)}
	if tm.disk.Pending!=nil || !slices.Equal(tm.disk.ReplacementBatch,[]string{teamID(4)}) {t.Fatal("the first new member discarded the rest of the exhausted batch")}
	// Its freshly observed week cannot cancel replacement of the old cohort.
	tm.note("u3@example.com · Team", []ClaudeLimit{{Kind:"five_hour",Used:0},{Kind:"seven_day",Used:0}})
	f.mu.Lock(); seats:=slices.Clone(f.seats); f.mu.Unlock()
	if ready,err:=tm.prepareWeeklyBatch(c,seats);err!=nil || !ready || !slices.Equal(tm.disk.ReplacementBatch,[]string{teamID(4)}) {t.Fatal("fresh replacement stopped the old batch",err)}
}
