package claudeteam

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExpiryWinsBeforeObservationFreshness(t *testing.T) {
	now := time.Now()
	cfg := Config{OrganizationID: "org", Members: []string{"source", "fresh", "soon"}, Threshold: 100}
	members := []Candidate{{ID: "source", Tier: "team_tier_1", Limits: []Limit{{Kind: "five_hour", Used: 1}}},
		{ID: "fresh", Tier: "unassigned", Reliable: true, Weight: Weight{Known: true, Used: 50, Soon: []time.Time{now.Add(6 * 24 * time.Hour)}}},
		{ID: "soon", Tier: "unassigned", Weight: Weight{Known: true, Used: 0, Soon: []time.Time{now.Add(3 * time.Hour)}}}}
	_, to, err := Choose(cfg, members, "", nil, now)
	if err != nil || to != "soon" {
		t.Fatalf("fresh snapshot hid soon-expiring allowance: %s %v", to, err)
	}
	members[2].Weight.Soon = members[1].Weight.Soon
	_, to, err = Choose(cfg, members, "", nil, now)
	if err != nil || to != "fresh" {
		t.Fatal("freshness did not break a real expiry tie", to, err)
	}
	members[0].Limits = []Limit{{Kind: "five_hour", Used: .2}, {Kind: "seven_day", Used: .2}}
	members[2].Reliable = true
	members[2].Weight.Soon = []time.Time{now.Add(time.Hour)}
	if from, _, _ := Choose(cfg, members, "", nil, now); from != "" {
		t.Fatal("healthy inference ordering consumed a seat move")
	}
}

func TestWeeklySeatMovementLimitIsNotInferenceOrSignInFailure(t *testing.T) {
	message := "This organization has reached its weekly limit on moving this seat type between people. An open seat may still go to someone who had this seat type recently. You can try again after 2026-11-24 (UTC), or contact support. secret-cookie-must-not-escape"
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "permission_error", "message": message}})
	err := ResponseError("PUT", &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))})
	f := safeFailure("assign", err)
	if f.Kind != "seat_movement_limit" || f.RetryDate != "2026-11-24" || f.Status != 403 {
		t.Fatalf("lost server restriction: %+v", f)
	}
	if strings.Contains(f.Message, "secret-cookie") || strings.Contains(f.Message, "authentication") {
		t.Fatal("leaked vendor message or invented sign-out", f.Message)
	}
	if (&TransferFailure{Outcome: "rolled_back", Failure: *f}).TryAnother() {
		t.Fatal("organization cap cycled through candidates")
	}
	if targetFailure(Event{Result: "rolled_back", Failure: f}) {
		t.Fatal("organization limit blamed an individual standby")
	}
	if !(MovementLimit{RetryDate: f.RetryDate}).Active(time.Date(2026, 11, 24, 23, 59, 0, 0, time.UTC)) {
		t.Fatal("a date-only reset invented an earlier retry instant")
	}
}
