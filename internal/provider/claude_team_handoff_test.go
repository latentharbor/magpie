package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/routes"
)

func TestClaudeTeamStandbyClearedBeforeAssignment(t *testing.T) {
	tm, admin, c := teamFixture(t)
	now := time.Now()
	for _, n := range []int{2, 3, 4} {
		tm.disk.Limits[teamID(n)] = []ClaudeLimit{
			{Kind: "five_hour", Used: .1, ResetsAt: now.Add(time.Hour).Unix()},
			{Kind: "seven_day", Used: .5, ResetsAt: now.Add(time.Duration(n) * time.Hour).Unix()},
		}
		tm.disk.Observed[teamID(n)] = now
	}
	tm.disk.Limits[teamID(2)][0].Used = 1
	admin.seats[2].SeatTier = "unassigned"
	dir, err := claudeSavedDir("u3@example.com · Team")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`), 0600); err != nil {
		t.Fatal(err)
	}
	from, to, err := tm.choose(c, admin.seats, "")
	if err != nil || from != teamID(2) || to != teamID(4) {
		t.Fatalf("cleared standby took a seat: %s -> %s: %v", from, to, err)
	}
	if len(admin.writes) != 0 || claudeLapseOf("u3@example.com · Team") == "" {
		t.Fatal("candidate validation changed seats or failed to retain reauth notice")
	}
	if err := tm.transfer(context.Background(), c, admin, admin.seats, teamID(2), teamID(3)); err == nil || len(admin.writes) != 0 {
		t.Fatal("direct transfer released source for a cleared candidate")
	}
}

func TestClaudeTeamSmartTrustAndDwell(t *testing.T) {
	tm, _, c := teamFixture(t)
	now := time.Now()
	seats := []claudeTeamSeat{{teamID(2), "team_tier_1"}, {teamID(3), "unassigned"}, {teamID(4), "unassigned"}}
	for _, n := range []int{2, 3, 4} {
		tm.disk.Limits[teamID(n)] = []ClaudeLimit{
			{Kind: "five_hour", Used: .1, ResetsAt: now.Add(4 * time.Hour).Unix()},
			{Kind: "seven_day", Used: .5, ResetsAt: now.Add(48 * time.Hour).Unix()},
		}
		tm.disk.Observed[teamID(n)] = now
	}
	tm.disk.Limits[teamID(3)][1].ResetsAt = now.Add(2 * time.Hour).Unix()
	check := func(want string) {
		t.Helper()
		from, _, err := tm.choose(c, seats, "")
		if err != nil || from != want {
			t.Fatalf("source %s, want %s: %v", from, want, err)
		}
	}
	check(teamID(2))
	tm.disk.QuotaErrors = map[string]string{teamID(3): "capture failed"}
	check("")
	delete(tm.disk.QuotaErrors, teamID(3))
	tm.disk.Observed[teamID(3)] = now.Add(-25 * time.Hour)
	check("")
	tm.disk.Observed[teamID(3)] = now
	tm.disk.History = []ClaudeTeamEvent{{At: now, Result: "assigned"}}
	check("")
	// Dwell must never strand a truly exhausted account.
	tm.disk.Limits[teamID(2)][0].Used = 1
	check(teamID(2))
	// A failed earlier-expiry snapshot loses to a reliable later one.
	tm.disk.QuotaErrors[teamID(3)] = "capture failed"
	_, to, err := tm.choose(c, seats, "")
	if err != nil || to != teamID(4) {
		t.Fatalf("failed snapshot preferred over reliable standby: %s %v", to, err)
	}
}

func TestClaudeTeamVoluntaryExpiryNeedsBenefit(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		active, standby []time.Duration
		want            bool
	}{
		{"distant week", []time.Duration{60 * time.Hour}, []time.Duration{12 * time.Hour}, false},
		{"urgent week", []time.Duration{60 * time.Hour}, []time.Duration{2 * time.Hour}, true},
		{"tiny gain", []time.Duration{130 * time.Minute}, []time.Duration{110 * time.Minute}, false},
		{"urgent five hour", []time.Duration{24 * time.Hour, 4 * time.Hour}, []time.Duration{24 * time.Hour, 30 * time.Minute}, true},
		{"distant five hour", []time.Duration{24 * time.Hour, 4 * time.Hour}, []time.Duration{24 * time.Hour, 2 * time.Hour}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seat := func(ds []time.Duration) routes.Seat {
				s := routes.Seat{}
				for _, d := range ds {
					s.Soon = append(s.Soon, now.Add(d))
				}
				return s
			}
			if got := claudeTeamExpiryWorthwhile(seat(tc.active), seat(tc.standby), now); got != tc.want {
				t.Fatalf("worthwhile=%v, want %v", got, tc.want)
			}
		})
	}
}
