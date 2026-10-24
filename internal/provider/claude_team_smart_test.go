package provider

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestClaudeTeamSmartSpendsExpiringAllowanceFirst(t *testing.T) {
	tm, _, c := teamFixture(t)
	now := time.Now()
	seats := []claudeTeamSeat{{teamID(2), "team_tier_1"}, {teamID(3), "unassigned"}, {teamID(4), "unassigned"}}
	tm.disk.Limits[teamID(2)] = []ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: now.Add(time.Hour).Unix()}}
	set := func(id string, five, week float64, fh, wh time.Duration) {
		tm.disk.Limits[id] = []ClaudeLimit{{Kind: "five_hour", Used: five, ResetsAt: now.Add(fh).Unix()}, {Kind: "seven_day", Used: week, ResetsAt: now.Add(wh).Unix()}}
	}
	check := func(want string) {
		t.Helper()
		_, got, err := tm.choose(c, seats, "")
		if err != nil || got != want {
			t.Fatalf("standby %s, want %s: %v", got, want, err)
		}
	}
	// Even near exhaustion, the week expiring first must be used before
	// its remaining allowance disappears. The seat threshold is the cutoff.
	set(teamID(3), .1, .99, 4*time.Hour, 2*time.Hour)
	set(teamID(4), .1, .1, 4*time.Hour, 48*time.Hour)
	check(teamID(3))
	// A near-empty but usable 5h window must not hide an earlier week.
	set(teamID(3), .99, .1, 4*time.Hour, time.Hour)
	set(teamID(4), .1, .1, 4*time.Hour, 8*time.Hour)
	check(teamID(3))
	// When headroom is ample, use the allowance that would expire sooner.
	set(teamID(3), .1, .1, 4*time.Hour, 4*time.Hour)
	check(teamID(3))
	// Unknown accounts no longer take a seat ahead of known usable capacity.
	delete(tm.disk.Limits, teamID(3))
	check(teamID(4))
	// A reset is a usable estimate, not a rewrite of the real observation.
	set(teamID(3), 1, .1, -time.Hour, 12*time.Hour)
	set(teamID(4), 1, 1, time.Hour, 48*time.Hour)
	check(teamID(3))
	if tm.disk.Limits[teamID(3)][0].Used != 1 {
		t.Fatal("rewrote historical observation")
	}
}

func TestClaudeTeamSmartRebalancesExpiringWeekWithoutChurn(t *testing.T) {
	tm, _, c := teamFixture(t)
	now := time.Now().Truncate(time.Hour).Add(10 * time.Minute)
	set := func(id string, week float64, reset time.Time) {
		tm.disk.Limits[id] = []ClaudeLimit{{Kind: "five_hour", Used: .1, ResetsAt: now.Add(4 * time.Hour).Unix()}, {Kind: "seven_day", Used: week, ResetsAt: reset.Unix()}}
	}
	set(teamID(2), .2, now.Add(72*time.Hour))
	set(teamID(3), .99, now.Add(2*time.Hour))
	set(teamID(4), .1, now.Add(48*time.Hour))
	seats := []claudeTeamSeat{{teamID(2), "team_tier_1"}, {teamID(3), "unassigned"}, {teamID(4), "unassigned"}}
	from, to, err := tm.choose(c, seats, "")
	if err != nil || from != teamID(2) || to != teamID(3) {
		t.Fatalf("did not spend expiring week: %s -> %s: %v", from, to, err)
	}
	// Once assigned, the earlier expiry stays assigned: no oscillation.
	seats[0].SeatTier, seats[1].SeatTier = "unassigned", "team_tier_1"
	from, _, err = tm.choose(c, seats, "")
	if err != nil || from != "" {
		t.Fatalf("churned healthy earliest seat: %s %v", from, err)
	}
	// No speculative handoff when a parked account has incomplete data.
	delete(tm.disk.Limits, teamID(2))
	delete(tm.disk.Limits, teamID(4))
	from, _, err = tm.choose(c, seats, "")
	if err != nil || from != "" {
		t.Fatalf("unknown quota evicted healthy account: %s %v", from, err)
	}
	// A custom threshold remains the hard cutoff even near expiry.
	c.Threshold = 95
	seats[0].SeatTier, seats[1].SeatTier = "team_tier_1", "unassigned"
	set(teamID(2), .1, now.Add(72*time.Hour))
	set(teamID(4), .1, now.Add(48*time.Hour))
	_, to, err = tm.choose(c, seats, "")
	if err != nil || to != teamID(4) {
		t.Fatalf("ignored threshold: %s %v", to, err)
	}
	// Explicit in-order policy never starts voluntary expiry rebalancing.
	c.Routing = "order"
	from, _, err = tm.choose(c, seats, "")
	if err != nil || from != "" {
		t.Fatalf("overrode explicit policy: %s %v", from, err)
	}
}

func TestClaudeTeamQuotaKnown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		five, week ClaudeLimit
		want       bool
	}{
		{"not started", ClaudeLimit{Used: 0}, ClaudeLimit{Used: 0}, true},
		{"positive without reset", ClaudeLimit{Used: .1}, ClaudeLimit{}, false},
		{"nan", ClaudeLimit{Used: math.NaN()}, ClaudeLimit{}, false},
		{"infinite", ClaudeLimit{Used: math.Inf(1)}, ClaudeLimit{}, false},
		{"negative", ClaudeLimit{Used: -.1}, ClaudeLimit{}, false},
		{"above full", ClaudeLimit{Used: 1.1, ResetsAt: 100}, ClaudeLimit{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.five.Kind = "five_hour"
			tc.week.Kind = "seven_day"
			if got := claudeTeamQuotaKnown([]ClaudeLimit{tc.five, tc.week}); got != tc.want {
				t.Fatalf("known=%v want=%v", got, tc.want)
			}
		})
	}
	if claudeTeamQuotaKnown([]ClaudeLimit{{Kind: "five_hour"}}) {
		t.Fatal("partial snapshot is complete")
	}
}

func TestClaudeTeamSmartKeepsOtherPremiumSeats(t *testing.T) {
	tm, _, c := teamFixture(t)
	now := time.Now().Truncate(time.Hour)
	for n, week := range map[int]time.Duration{2: 24 * time.Hour, 3: 2 * time.Hour, 4: 72 * time.Hour} {
		tm.disk.Limits[teamID(n)] = []ClaudeLimit{{Kind: "five_hour", Used: .1, ResetsAt: now.Add(4 * time.Hour).Unix()}, {Kind: "seven_day", Used: .1, ResetsAt: now.Add(week).Unix()}}
	}
	seats := []claudeTeamSeat{{teamID(2), "team_tier_1"}, {teamID(3), "unassigned"}, {teamID(4), "team_tier_1"}, {teamID(9), "team_tier_1"}}
	from, to, err := tm.choose(c, seats, "")
	if err != nil || from != teamID(4) || to != teamID(3) {
		t.Fatalf("wrong Premium displaced: %s -> %s %v", from, to, err)
	}
	// With the same weekly hour, 5h expiry decides; equal hours retain seats.
	tm.disk.Limits[teamID(3)][1].ResetsAt = tm.disk.Limits[teamID(2)][1].ResetsAt
	tm.disk.Limits[teamID(4)][1].ResetsAt = tm.disk.Limits[teamID(2)][1].ResetsAt
	tm.disk.Limits[teamID(3)][0].ResetsAt = now.Add(time.Hour).Unix()
	from, to, err = tm.choose(c, seats, "")
	if err != nil || from != teamID(2) || to != teamID(3) {
		t.Fatalf("ignored 5h expiry: %s -> %s %v", from, to, err)
	}
	tm.disk.Limits[teamID(3)][0].ResetsAt = tm.disk.Limits[teamID(2)][0].ResetsAt + 60
	from, _, err = tm.choose(c, seats, "")
	if err != nil || from != "" {
		t.Fatalf("same-hour drift moved a seat: %s %v", from, err)
	}
}

func TestClaudeTeamSmartExpiryTransferDrainsBeforeAdminWrite(t *testing.T) {
	tm, f, _ := teamFixture(t)
	oldUsage := UsageClaudeVia(func(context.Context, string) (string, error) { return "", errors.New("offline quota capture fixture") })
	t.Cleanup(func() { UsageClaudeVia(oldUsage) })
	now := time.Now()
	for n, week := range map[int]time.Duration{2: 24 * time.Hour, 3: 2 * time.Hour, 4: 72 * time.Hour} {
		tm.disk.Limits[teamID(n)] = []ClaudeLimit{{Kind: "five_hour", Used: .1, ResetsAt: now.Add(4 * time.Hour).Unix()}, {Kind: "seven_day", Used: .1, ResetsAt: now.Add(week).Unix()}}
	}
	release, ok := tm.track("u4@example.com · Team")
	if !ok {
		t.Fatal("active seat rejected")
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { tm.op.Lock(); defer tm.op.Unlock(); done <- tm.reconcile(ctx, "") }()
	for {
		tm.mu.Lock()
		closed, ch := tm.closed[teamID(4)], tm.changed
		tm.mu.Unlock()
		if closed {
			break
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("expiry rebalance did not drain")
		}
	}
	f.mu.Lock()
	writes := len(f.writes)
	f.mu.Unlock()
	if writes != 0 {
		t.Fatal("healthy request interrupted by expiry transfer")
	}
	if r, ok := tm.track("u4@example.com · Team"); ok {
		r()
		t.Fatal("draining source accepted new request")
	}
	other, ok := tm.track("u2@example.com · Team")
	if !ok {
		t.Fatal("other Premium stopped")
	}
	other()
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) != 2 || f.writes[0] != teamID(4)+":unassigned" || f.writes[1] != teamID(3)+":team_tier_1" {
		t.Fatalf("wrong expiry transfer: %v", f.writes)
	}
}
