package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/claudeteam"
)

type weeklyLimitedAdmin struct {
	*fakeTeamAdmin
	limited bool
}

func (a *weeklyLimitedAdmin) SetTier(ctx context.Context, id, tier string) error {
	if a.limited && id == teamID(3) && tier == "team_tier_1" {
		a.mu.Lock()
		a.writes = append(a.writes, id+":"+tier)
		a.mu.Unlock()
		body := `{"error":{"type":"permission_error","message":"This organization has reached its weekly limit on moving this seat type between people. An open seat may still go to someone who had this seat type recently. You can try again after 2099-11-24 (UTC), or contact support."}}`
		return claudeteam.ResponseError("PUT", &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))})
	}
	return a.fakeTeamAdmin.SetTier(ctx, id, tier)
}

func TestClaudeTeamMovementLimitPreservesPremiumInference(t *testing.T) {
	tm, f, c := teamFixture(t)
	usage := UsageClaudeVia(func(context.Context, string) (string, error) { return "", errors.New("fixture quota unavailable") })
	t.Cleanup(func() { UsageClaudeVia(usage) })
	admin := &weeklyLimitedAdmin{fakeTeamAdmin: f, limited: true}
	newClaudeTeamAdmin = func(ClaudeTeamConfig) claudeTeamAdmin { return admin }
	tm.note("u2@example.com · Team", []ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: time.Now().Add(time.Hour).Unix()}})
	if err := tm.reconcile(context.Background(), ""); err == nil {
		t.Fatal("organization cap was swallowed")
	}
	if tm.disk.MovementLimit == nil || tm.disk.MovementLimit.RetryDate != "2099-11-24" {
		t.Fatal("organization restriction not persisted")
	}
	if len(tm.disk.Cooldowns) != 0 {
		t.Fatal("global seat cap blamed an individual account")
	}
	before := len(f.writes)
	for range 50 {
		tm.note("u2@example.com · Team", []ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: time.Now().Add(time.Hour).Unix()}})
		_ = tm.reconcile(context.Background(), "")
	}
	if len(f.writes) != before {
		t.Fatal("known seat-movement cap kept re-upgrading the source")
	}
	release, ok := tm.track("u4@example.com · Team")
	if !ok {
		t.Fatal("seat cap disabled another Premium inference account")
	}
	release()
	admin.limited = false
	if err := tm.reconcile(context.Background(), teamID(2)); err != nil {
		t.Fatal(err)
	}
	if f.seats[1].SeatTier != "team_tier_1" || tm.disk.Pending != nil {
		t.Fatal("explicit server-allowed retry failed")
	}
	// A successful exception for a recent holder is not proof that the global
	// weekly cap vanished. A verified increase in actual seats can reopen it.
	if tm.disk.MovementLimit == nil {
		t.Fatal("recent-holder exception erased the organization restriction")
	}
	f.seats = append(f.seats, claudeTeamSeat{AccountID: teamID(10), SeatTier: "team_tier_1"})
	if err := tm.reconcile(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if tm.disk.MovementLimit != nil {
		t.Fatal("verified capacity increase did not permit a new decision")
	}
	if c.Enabled != true {
		t.Fatal("configuration disabled")
	}
}
