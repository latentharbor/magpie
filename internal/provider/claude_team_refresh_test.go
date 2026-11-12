package provider

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestClaudeTeamRefreshObservesRevocationAndOnlyReadsPremium(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			tm, admin, _ := teamFixture(t)
			old := UsageClaudeVia(func(context.Context, string) (string, error) { return teamUsageFixture(), nil })
			t.Cleanup(func() { UsageClaudeVia(old) })
			tm.collectQuotaDue(context.Background())
			limits, observed := tm.disk.Limits[teamID(2)], tm.disk.Observed[teamID(2)]
			originalLogins := readLogins()
			tm.cfg.Enabled = enabled
			// A web revocation and reassignment do not appear in the local view
			// until the explicit read; outside-pool Premium must not be probed.
			admin.seats[0].SeatTier = "unassigned"
			admin.seats[1].SeatTier = "team_tier_1"
			var users []string
			UsageClaudeVia(func(_ context.Context, dir string) (string, error) {
				users = append(users, dir)
				if dir == claudeAccountDir("u4@example.com · Team") {
					return "", errors.New("private helper output")
				}
				return strings.Replace(teamUsageFixture(), "17%", "29%", 1), nil
			})
			if err := tm.refreshSnapshot(context.Background()); err == nil {
				t.Fatal("failed Premium quota read was hidden")
			}
			if len(users) != 2 || users[0] != claudeAccountDir("u3@example.com · Team") || users[1] != claudeAccountDir("u4@example.com · Team") {
				t.Fatalf("refresh read non-Premium or outside-pool accounts: %v", users)
			}
			if tm.seats[teamID(2)] != "unassigned" || tm.seats[teamID(3)] != "team_tier_1" {
				t.Fatal("refresh retained stale cloud assignments")
			}
			if !reflect.DeepEqual(tm.disk.Limits[teamID(2)], limits) || tm.disk.Observed[teamID(2)] != observed {
				t.Fatal("revocation discarded the historical quota snapshot")
			}
			if tm.disk.Limits[teamID(3)][0].Used != .29 || tm.disk.QuotaErrors[teamID(4)] == "" {
				t.Fatal("refresh did not publish fresh quota and per-account failure")
			}
			if len(admin.writes) != 0 || tm.refreshing || tm.quotaBusy != "" {
				t.Fatal("refresh moved a seat or remained busy")
			}
			if !enabled {
				for i, l := range readLogins() {
					old := originalLogins[i]
					if l.On != old.On || l.Paused != old.Paused || l.TeamOff != old.TeamOff {
						t.Fatal("refresh changed disabled-provider switches")
					}
				}
			}
			admin.readErr = errors.New("member read failed")
			if err := tm.refreshSnapshot(context.Background()); err == nil || tm.seats[teamID(2)] != "unassigned" || len(users) != 2 {
				t.Fatal("failed member read erased state or proceeded to quota")
			}
		})
	}
}

func TestClaudeTeamRefreshCoalescesAndRequiresAnOwner(t *testing.T) {
	tm, _, c := teamFixture(t)
	if err := RefreshClaudeTeamOrganization(c.OrganizationID); err == nil {
		t.Fatal("refresh was accepted without a controller owner")
	}
	tm.running = true
	for range 2 {
		if err := RefreshClaudeTeamOrganization(c.OrganizationID); err != nil {
			t.Fatal(err)
		}
	}
	if !tm.status().Refreshing || tm.requested || tm.manual != "" {
		t.Fatal("refresh was lost or requested a seat transfer")
	}
	tm.refreshRequested, tm.refreshing = false, true
	if err := RefreshClaudeTeamOrganization(c.OrganizationID); err != nil || tm.refreshRequested {
		t.Fatal("a click during refresh queued duplicate reads", err)
	}
	tm.refreshing = false
	tm.disk.Pending = &claudeTeamPending{}
	if err := tm.refreshSnapshot(context.Background()); err == nil {
		t.Fatal("refresh bypassed pending seat recovery")
	}
}
