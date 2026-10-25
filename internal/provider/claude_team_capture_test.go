package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func teamUsageFixture() string {
	now := time.Now().UTC()
	five := now.Add(3*time.Hour).Format("3:04pm") + " (UTC)"
	week := now.Add(72*time.Hour).Format("Jan 2 at 3:04pm") + " (UTC)"
	return "Current session: 17% used · resets " + five + "\nCurrent week (all models): 38% used · resets " + week
}

func TestClaudeTeamJoinCapturesAndRetainsQuotaWithoutSeatWrites(t *testing.T) {
	tm, f, c := teamFixture(t)
	calls := 0
	old := UsageClaudeVia(func(context.Context, string) (string, error) { calls++; return teamUsageFixture(), nil })
	t.Cleanup(func() { UsageClaudeVia(old) })
	if !tm.status().Members[0].QuotaPending {
		t.Fatal("joining did not queue quota collection")
	}
	tm.collectQuotaDue(context.Background())
	if calls != 3 {
		t.Fatalf("joining collected %d accounts, want 3", calls)
	}
	before := tm.disk.Observed[teamID(2)]
	if len(tm.disk.Limits[teamID(2)]) != 2 || before.IsZero() {
		t.Fatal("missing complete snapshot")
	}
	var disk claudeTeamDisk
	b, err := os.ReadFile(tm.statePath())
	if err != nil || json.Unmarshal(b, &disk) != nil || len(disk.Limits[teamID(2)]) != 2 {
		t.Fatal("snapshot was not saved", err)
	}
	f.seats[0].SeatTier = "unassigned"
	if err := tm.synchronize(c, f.seats); err != nil {
		t.Fatal(err)
	}
	UsageClaudeVia(func(context.Context, string) (string, error) { return "", errors.New("secret must not escape") })
	if err := tm.refreshQuota(context.Background(), teamID(2)); err == nil {
		t.Fatal("quota failure was hidden")
	}
	if len(tm.disk.Limits[teamID(2)]) != 2 || tm.disk.Observed[teamID(2)] != before || strings.Contains(tm.disk.QuotaErrors[teamID(2)], "secret") {
		t.Fatal("failure erased snapshot or exposed stderr")
	}
	oldWindows := tm.disk.Limits[teamID(2)]
	UsageClaudeVia(func(context.Context, string) (string, error) { return "Current session: 0% used", nil })
	if err := tm.refreshQuota(context.Background(), teamID(2)); err == nil || tm.disk.Limits[teamID(2)][1] != oldWindows[1] {
		t.Fatal("partial result replaced complete snapshot")
	}
	UsageClaudeVia(func(context.Context, string) (string, error) { return teamUsageFixture(), nil })
	tm.mu.Lock()
	tm.queueQuotaLocked(teamID(2))
	tm.mu.Unlock()
	tm.collectQuotaDue(context.Background())
	if tm.disk.QuotaErrors[teamID(2)] != "" {
		t.Fatal("successful retry kept the failure")
	}
	if len(f.writes) != 0 {
		t.Fatal("quota collection moved a seat")
	}
}

func TestClaudeTeamExternalPremiumAndLoginQueueQuota(t *testing.T) {
	tm, f, c := teamFixture(t)
	tm.quotaDue = map[string]bool{}
	// A web assignment is observed without an internal transfer/history entry.
	f.seats[1].SeatTier = "team_tier_1"
	if err := tm.synchronize(c, f.seats); err != nil {
		t.Fatal(err)
	}
	if !tm.quotaDue[teamID(3)] {
		t.Fatal("external Premium assignment missed quota capture")
	}
	tm.quotaDue = map[string]bool{}
	tm.quotaBusy = teamID(2) // an older-token read must not swallow re-login
	if _, err := addLogin(readLogins()[0]); err != nil {
		t.Fatal(err)
	}
	if !tm.quotaDue[teamID(2)] {
		t.Fatal("successful login missed quota capture")
	}
	if len(f.writes) != 0 {
		t.Fatal("capture events moved seats")
	}
}

func TestClaudeTeamCaptureKeepsModelWindow(t *testing.T) {
	tm, _, _ := teamFixture(t)
	window := ClaudeLimit{Kind: "seven_day_opus", Used: 1, ResetsAt: time.Now().Add(time.Hour).Unix()}
	tm.disk.Limits[teamID(2)] = []ClaudeLimit{window}
	old := UsageClaudeVia(func(context.Context, string) (string, error) { return teamUsageFixture(), nil })
	t.Cleanup(func() { UsageClaudeVia(old) })
	if err := tm.refreshQuota(context.Background(), teamID(2)); err != nil {
		t.Fatal(err)
	}
	if len(tm.disk.Limits[teamID(2)]) != 3 || tm.disk.Limits[teamID(2)][2] != window {
		t.Fatal("global read erased a model-specific restriction")
	}
}

func TestClaudeTeamCompleteQuota(t *testing.T) {
	for _, tc := range []struct {
		text     string
		complete bool
	}{
		{teamUsageFixture(), true},
		{"Current session: 0% used\nCurrent week (all models): 0% used", true},
		{"Current session: 17% used\nCurrent week (all models): 38% used", false},
		{"Current session: 0% used", false},
	} {
		ws, _ := parseClaudeUsage(tc.text, time.Now())
		if got := completeClaudeTeamQuota(ws); got != tc.complete {
			t.Fatalf("complete=%v for %q", got, tc.text)
		}
	}
}
