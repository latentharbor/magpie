package provider

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestClaudeTeamEstimateDoesNotReplaceObservations(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	lastQuotas.Lock()
	lastQuotas.m, lastQuotas.loaded = nil, false
	lastQuotas.Unlock()
	t.Cleanup(func() {
		lastQuotas.Lock()
		lastQuotas.m, lastQuotas.loaded = nil, false
		lastQuotas.Unlock()
	})
	now := time.Now()
	reset := now.Add(-time.Hour)
	observed := QuotaWindow{Name: "7 days", Used: 100, ResetsAt: &reset}
	q := SubscriptionQuota{Provider: "claude", User: "team@example.com", Windows: []QuotaWindow{observed}}
	keepLast(q, "")
	before, err := os.ReadFile(lastQuotasPath())
	if err != nil {
		t.Fatal(err)
	}
	q.Windows = claudeTeamWindowsAt([]ClaudeLimit{{Kind: "seven_day", Used: 1, ResetsAt: reset.Unix()}}, now)
	if got := keepLast(q, ""); got.Windows[0].Used != 0 || !got.Windows[0].Estimated {
		t.Fatalf("estimate not returned for display: %+v", got)
	}
	after, err := os.ReadFile(lastQuotasPath())
	if err != nil || string(before) != string(after) {
		t.Fatal("estimate replaced the last observed reading")
	}
	if _, ok := pointOf(q.Windows[0], now, now); ok {
		t.Fatal("estimate became a historical observation")
	}
	if _, ok := pointOf(observed, now, now); !ok {
		t.Fatal("an actual reading was discarded")
	}
	noteQuotaHistory([]SubscriptionQuota{q}, now)
	if len(readQuotaHist()) != 0 {
		t.Fatal("an estimate was recorded in history")
	}
	at := now.Add(-2 * time.Hour)
	q.ReadAt, q.Windows = &at, []QuotaWindow{observed}
	noteQuotaHistory([]SubscriptionQuota{q}, now.Add(time.Minute))
	if len(readQuotaHist()) != 1 {
		t.Fatal("the estimate suppressed an earlier actual observation")
	}
}

func TestClaudeTeamElapsedWindowsKeepWeeklyAnchor(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 48, 29, 0, time.UTC)
	week := 7 * 24 * time.Hour
	for _, age := range []time.Duration{0, time.Second, 2*week + 3*time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			reset := now.Add(-age)
			raw := []ClaudeLimit{
				{Kind: "five_hour", Used: 1, ResetsAt: reset.Unix()},
				{Kind: "seven_day", Used: 1, ResetsAt: reset.Unix()},
				{Kind: "seven_day_opus", Used: 1, ResetsAt: reset.Unix()},
			}
			before := append([]ClaudeLimit(nil), raw...)
			a := allowanceOf(claudeLimitWindows(raw), now)
			used, _ := a.For("", now)
			wantWeek := reset.Add((age/week + 1) * week)
			next := a.Renewal("", now)
			if used != 0 || len(next) != 2 || !next[0].Equal(wantWeek) || !next[1].Equal(now.Add(5*time.Hour)) {
				t.Fatalf("after reset used=%v next=%v; want fixed week %v and prospective 5h", used, next, wantWeek)
			}
			opus := a.Renewal("claude-opus", now)
			if len(opus) != 3 || !opus[0].Equal(wantWeek) || !opus[1].Equal(wantWeek) {
				t.Fatalf("model-specific week lost its anchor: %v", opus)
			}
			if _, due := a.Pace("", now); !due.Equal(wantWeek) {
				t.Fatalf("pace lost the fixed weekly expiry: %v", due)
			}
			if !reflect.DeepEqual(raw, before) {
				t.Fatal("projection overwrote observed usage")
			}
		})
	}
}

func TestClaudeTeamElapsedQuotaDisplayDoesNotProbeOrRewrite(t *testing.T) {
	tm, _, _ := teamFixture(t)
	now := time.Now()
	reset := now.Add(-time.Hour).Unix()
	user := "u3@example.com · Team"
	tm.note(user, []ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: reset}, {Kind: "seven_day", Used: 1, ResetsAt: reset}})
	tm.mu.Lock()
	observed := tm.disk.Observed[teamID(3)]
	tm.mu.Unlock()
	ws, err := claudeWindows(context.Background(), user, false)
	if err != nil || len(ws) != 2 {
		t.Fatalf("cached display: %v %v", ws, err)
	}
	if ws[0].Used != 0 || ws[0].ResetsAt != nil || !ws[0].Estimated ||
		ws[1].Used != 0 || ws[1].ResetsAt == nil || ws[1].ResetsAt.Unix() != reset+7*24*3600 || !ws[1].Estimated {
		t.Fatalf("display kept spent windows or invented a running 5h: %+v", ws)
	}
	for _, m := range tm.status().Members {
		if m.AccountID == teamID(3) && (len(m.Windows) != 2 || m.Windows[0].Used != 0 || !m.Windows[1].Estimated || m.Limits[0].Used != 1) {
			t.Fatalf("member API disagrees with quota display: %+v", m)
		}
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.disk.Limits[teamID(3)][0].Used != 1 || tm.disk.Limits[teamID(3)][1].ResetsAt != reset || !tm.disk.Observed[teamID(3)].Equal(observed) {
		t.Fatal("display rewrote historical usage or observation time")
	}
}

func TestClaudeTeamProjectionLeavesUnknownAndFutureWindowsAlone(t *testing.T) {
	now := time.Now()
	raw := []ClaudeLimit{{Kind: "five_hour", Used: 1}, {Kind: "seven_day", Used: .8, ResetsAt: now.Add(time.Hour).Unix()}}
	ws := claudeTeamWindowsAt(raw, now)
	if len(ws) != 2 || ws[0].Used != 100 || ws[0].Estimated || ws[1].Used != 80 || ws[1].Estimated {
		t.Fatalf("missing/future reset was cleared: %+v", ws)
	}
	past := now.Add(-6 * 24 * time.Hour)
	// Identical names/spans from other vendors keep the existing rolling rule.
	a := allowanceOf([]QuotaWindow{{Name: "7 days", Used: 100, ResetsAt: &past, Span: 7 * 24 * time.Hour}}, now)
	if got := a.Renewal("", now); len(got) != 1 || !got[0].Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("changed another vendor's reset policy: %v", got)
	}
}

func TestClaudeTeamSmartUsesFixedWeeklyResetAfterExpiry(t *testing.T) {
	tm, _, c := teamFixture(t)
	now := time.Now()
	week := 7 * 24 * time.Hour
	tm.disk.Limits[teamID(2)] = []ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: now.Add(time.Hour).Unix()}}
	// A's old week ended six days ago: its next expiry is tomorrow,
	// before B's two remaining days, rather than a week from now.
	tm.disk.Limits[teamID(3)] = []ClaudeLimit{{Kind: "five_hour"}, {Kind: "seven_day", Used: 1, ResetsAt: now.Add(-week + 24*time.Hour).Unix()}}
	tm.disk.Limits[teamID(4)] = []ClaudeLimit{{Kind: "five_hour"}, {Kind: "seven_day", Used: .2, ResetsAt: now.Add(48 * time.Hour).Unix()}}
	seats := []claudeTeamSeat{{teamID(2), "team_tier_1"}, {teamID(3), "unassigned"}, {teamID(4), "unassigned"}}
	_, to, err := tm.choose(c, seats, "")
	if err != nil || to != teamID(3) {
		t.Fatalf("Smart lost the fixed weekly expiry: to=%s err=%v", to, err)
	}
}
