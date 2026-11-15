package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestClaudeTeamRefreshRecoversCommittedTransferFirst(t *testing.T) {
	tm, admin, c := teamFixture(t)
	old := UsageClaudeVia(func(context.Context, string) (string, error) { return teamUsageFixture(), nil })
	t.Cleanup(func() { UsageClaudeVia(old) })
	p := &claudeTeamPending{OrganizationID: c.OrganizationID, From: teamID(2), To: teamID(3), Baseline: premiumSet(admin.seats), Reason: "exhausted"}
	admin.seats[0].SeatTier, admin.seats[1].SeatTier = "unassigned", "team_tier_1"
	tm.disk.Pending, tm.refreshRequested = p, true
	if err := tm.route(c, map[string]bool{p.From: false, p.To: false}); err != nil { t.Fatal(err) }
	tm.controllerStep(context.Background())
	if tm.disk.Pending != nil || tm.err != "" || tm.admissions.Closed(p.To) {
		t.Fatalf("refresh stranded a committed seat transfer: pending=%v error=%s", tm.disk.Pending != nil, tm.err)
	}
	if len(admin.writes) != 0 { t.Fatal("recovery repeated an already committed assignment") }
	if len(tm.disk.History) != 1 || tm.disk.History[0].Result != "assigned" { t.Fatal("verified assignment was not published") }
}

func TestClaudeTeamPendingRecoveryKeepsActualFailureAndSkipsQuota(t *testing.T) {
	tm, admin, c := teamFixture(t)
	quotaCalls := 0
	old := UsageClaudeVia(func(context.Context, string) (string, error) { quotaCalls++; return teamUsageFixture(), nil })
	t.Cleanup(func() { UsageClaudeVia(old) })
	tm.disk.Pending = &claudeTeamPending{OrganizationID: c.OrganizationID, From: teamID(2), To: teamID(3), Baseline: premiumSet(admin.seats)}
	tm.refreshRequested = true
	admin.readErr = errors.New("fixture member read unavailable")
	tm.controllerStep(context.Background())
	if tm.disk.Pending == nil || !strings.Contains(tm.err, admin.readErr.Error()) || strings.Contains(tm.err, "retry refresh") {
		t.Fatalf("refresh replaced the actual recovery failure: %s", tm.err)
	}
	if quotaCalls != 0 || len(admin.writes) != 0 { t.Fatal("uncertain assignment was used for quota or repeated mutation") }
}

type slowRecoveryAdmin struct { *fakeTeamAdmin }
func (a slowRecoveryAdmin) Members(ctx context.Context) ([]claudeTeamSeat, error) {
	if err := ctx.Err(); err != nil { return nil, err }
	return a.fakeTeamAdmin.Members(ctx)
}
func (a slowRecoveryAdmin) SetTier(ctx context.Context, id, tier string) error {
	select {
	case <-ctx.Done(): return ctx.Err()
	case <-time.After(10 * time.Second): return a.fakeTeamAdmin.SetTier(ctx, id, tier)
	}
}

func TestClaudeTeamLateDrainStillHasTimeToConfirmAssignment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm, admin, _ := teamFixture(t)
		old := UsageClaudeVia(func(context.Context, string) (string, error) { return teamUsageFixture(), nil })
		t.Cleanup(func() { UsageClaudeVia(old) })
		newClaudeTeamAdmin = func(ClaudeTeamConfig) claudeTeamAdmin { return slowRecoveryAdmin{admin} }
		release, admitted := tm.admissions.Enter(teamID(2))
		if !admitted { t.Fatal("fixture source was not admitted") }
		defer release()
		tm.manual = teamID(2)
		done := make(chan struct{})
		go func() { defer close(done); tm.controllerStep(context.Background()) }()
		synctest.Wait()
		if !tm.admissions.Closed(teamID(2)) { t.Fatal("source did not begin draining") }
		time.Sleep(115 * time.Second)
		release()
		<-done
		if tm.err != "" || tm.disk.Pending != nil || tm.seats[teamID(3)] != "team_tier_1" {
			t.Fatalf("late drain consumed the assignment confirmation budget: %s", tm.err)
		}
		if !teamExpected(admin.seats, teamID(2), teamID(3), "assigned", []string{teamID(2), teamID(4), teamID(9)}) {
			t.Fatal("late transfer changed unrelated assignments")
		}
	})
}
