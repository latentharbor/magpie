package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeTeam429QuotaChecksCoalesce(t *testing.T) {
	for _, result := range []string{"exhausted", "healthy", "unknown"} {
		t.Run(result, func(t *testing.T) {
			tm, f, _ := teamFixture(t)
			user, id := "u2@example.com · Team", teamID(2)
			original := []ClaudeLimit{{Kind: "five_hour", Used: .99, ResetsAt: time.Now().Add(time.Hour).Unix()},
				{Kind: "seven_day", Used: .2, ResetsAt: time.Now().Add(24 * time.Hour).Unix()}}
			NoteClaudeLimits(user, original)
			entered, finish := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(finish) })
			var reads atomic.Int32
			old := UsageClaudeVia(func(ctx context.Context, _ string) (string, error) {
				if reads.Add(1) == 1 {
					close(entered)
				}
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-finish:
				}
				switch result {
				case "unknown":
					return "", errors.New("fixture secret")
				case "exhausted":
					return strings.Replace(teamUsageFixture(), "17%", "100%", 1), nil
				default:
					return teamUsageFixture(), nil
				}
			})
			t.Cleanup(func() { UsageClaudeVia(old) })
			t.Cleanup(release)
			var wg sync.WaitGroup
			wg.Go(func() { RecheckClaudeTeamQuota(context.Background(), user) })
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("quota verification never started")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			returned := make(chan struct{})
			go func() { RecheckClaudeTeamQuota(ctx, user); close(returned) }()
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("cancelled waiter blocked on another quota read")
			}
			for range 12 {
				wg.Go(func() { RecheckClaudeTeamQuota(context.Background(), user) })
			}
			release()
			wg.Wait()
			RecheckClaudeTeamQuota(context.Background(), user)
			if reads.Load() != 1 {
				t.Fatalf("one burst made %d quota reads", reads.Load())
			}
			if spent := ClaudeTeamExhausted(user); spent != (result == "exhausted") {
				t.Fatalf("spent=%v for %s", spent, result)
			}
			if result == "unknown" && (tm.disk.Limits[id][0] != original[0] || strings.Contains(tm.disk.QuotaErrors[id], "secret")) {
				t.Fatal("unknown quota erased observation or exposed helper error")
			}
			if len(f.writes) != 0 {
				t.Fatal("verification itself wrote a seat")
			}
		})
	}
}

func TestClaudeTeam429QuotaCheckCancels(t *testing.T) {
	tm, f, _ := teamFixture(t)
	user, id := "u2@example.com · Team", teamID(2)
	original := ClaudeLimit{Kind: "five_hour", Used: .99, ResetsAt: time.Now().Add(time.Hour).Unix()}
	NoteClaudeLimits(user, []ClaudeLimit{original})
	entered := make(chan struct{})
	old := UsageClaudeVia(func(ctx context.Context, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	})
	t.Cleanup(func() { UsageClaudeVia(old) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() { RecheckClaudeTeamQuota(ctx, user); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("verification never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("verification ignored request cancellation")
	}
	if tm.disk.Limits[id][0] != original || ClaudeTeamExhausted(user) || len(f.writes) != 0 {
		t.Fatal("cancelled verification changed quota or seats")
	}
}

func teamUsageFixture() string {
	now := time.Now().UTC()
	five := now.Add(3*time.Hour).Format("3:04pm") + " (UTC)"
	week := now.Add(72*time.Hour).Format("Jan 2 at 3:04pm") + " (UTC)"
	return "Current session: 17% used · resets " + five + "\nCurrent week (all models): 38% used · resets " + week
}

func TestClaudeTeamSlowQuotaReadKeepsNewerExhaustion(t *testing.T) {
	tm, admin, _ := teamFixture(t)
	user, id := "u2@example.com · Team", teamID(2)
	entered, finish := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(finish) })
	old := UsageClaudeVia(func(ctx context.Context, _ string) (string, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-finish:
			return teamUsageFixture(), nil
		}
	})
	t.Cleanup(func() { UsageClaudeVia(old) })
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tm.refreshQuota(ctx, id) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("quota read never started")
	}
	// An inference reports exhaustion after /usage began but before it
	// returns its older, healthy snapshot. Neither the journal nor the
	// ordinary quota cache may be rolled back by that late completion.
	NoteClaudeLimits(user, []ClaudeLimit{
		{Kind: "five_hour", Used: 1, ResetsAt: time.Now().Add(time.Hour).Unix()},
		{Kind: "seven_day", Used: .2, ResetsAt: time.Now().Add(24 * time.Hour).Unix()},
	})
	tm.mu.Lock()
	observed := tm.disk.Observed[id]
	tm.mu.Unlock()
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	tm.mu.Lock()
	keptObservation := tm.disk.Observed[id].Equal(observed)
	tm.mu.Unlock()
	if !ClaudeTeamExhausted(user) || !keptObservation {
		t.Fatal("late quota read replaced newer exhaustion or its observation time")
	}
	kept := KeptClaudeLimits(user)
	if len(kept) == 0 || kept[0].Used != 1 {
		t.Fatal("late quota read rolled back the ordinary quota cache")
	}
	if len(admin.writes) != 0 {
		t.Fatal("quota collection itself moved a seat")
	}
	// A later explicit reading still replaces the old observation. A
	// rejected stale read must not permanently latch the account at 100%.
	UsageClaudeVia(func(context.Context, string) (string, error) {
		return "Current session: 0% used\nCurrent week (all models): 0% used", nil
	})
	if err := tm.refreshQuota(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ClaudeTeamExhausted(user) {
		t.Fatal("new reading could not clear the old exhausted window")
	}
}

func TestClaudeTeamCardReadKeepsNewerExhaustion(t *testing.T) {
	_, admin, _ := teamFixture(t)
	user := "u2@example.com · Team"
	old := UsageClaudeVia(func(context.Context, string) (string, error) {
		NoteClaudeLimits(user, []ClaudeLimit{
			{Kind: "five_hour", Used: 1, ResetsAt: time.Now().Add(time.Hour).Unix()},
			{Kind: "seven_day", Used: .2, ResetsAt: time.Now().Add(24 * time.Hour).Unix()},
		})
		return teamUsageFixture(), nil
	})
	t.Cleanup(func() { UsageClaudeVia(old) })
	ctx := context.WithValue(Asked(context.Background()), cardRefreshKey{}, cardRefresh{"claude", user})
	ws, err := claudeWindows(ctx, user, false)
	if err != nil || len(ws) == 0 || ws[0].Used != 100 || !ClaudeTeamExhausted(user) {
		t.Fatalf("card read replaced a newer exhausted Team window: %v %v", ws, err)
	}
	if len(admin.writes) != 0 {
		t.Fatal("card read itself moved a seat")
	}
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
