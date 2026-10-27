package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type refusingTeamRead struct {
	claudeTeamAdmin
	refuse func()
}

func (f refusingTeamRead) Members(context.Context) ([]claudeTeamSeat, error) {
	f.refuse()
	return nil, errors.New("fixture member read failed")
}

func TestClaudeTeamRefusalDuringReconcileKeepsHandoffPending(t *testing.T) {
	tm, admin, _ := teamFixture(t)
	old := UsageClaudeVia(func(context.Context, string) (string, error) {
		return "", errors.New("fixture quota unavailable")
	})
	t.Cleanup(func() { UsageClaudeVia(old) })
	user := "u2@example.com · Team"
	tm.running, tm.requested = true, true
	newClaudeTeamAdmin = func(ClaudeTeamConfig) claudeTeamAdmin {
		return refusingTeamRead{admin, func() {
			NoteClaudeSignInFailure(user, ClaudeLoginVersion(user), "OAuth token revoked")
		}}
	}
	if err := tm.reconcile(context.Background(), ""); err == nil {
		t.Fatal("fixture member failure was lost")
	}
	// The in-flight pass failed to observe or transfer the revoked seat.
	// Its completion must not release the request waiting for that transfer.
	if !tm.requested || tm.rotating || !tm.status().Rotating {
		t.Fatal("reconciliation discarded the concurrent sign-out handoff")
	}
	select {
	case <-tm.wake:
	default:
		t.Fatal("concurrent sign-out did not schedule another pass")
	}
	newClaudeTeamAdmin = func(ClaudeTeamConfig) claudeTeamAdmin { return admin }
	if err := tm.reconcile(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if tm.rotating || tm.seats[teamID(3)] != "team_tier_1" || len(admin.writes) != 2 {
		t.Fatalf("next pass did not complete the pending handoff: requested=%v rotating=%v seats=%v writes=%v enabled=%v lapse=%q",
			tm.requested, tm.rotating, tm.seats, admin.writes, tm.routeEnabled(tm.cfg), claudeLapseOf(user))
	}
	// Reading the refused source's native store can wake the controller
	// again during that transfer. Consume it without making another transfer.
	if err := tm.reconcile(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if tm.requested || tm.rotating || len(admin.writes) != 2 {
		t.Fatal("the duplicate refusal did not settle after one handoff")
	}
}

func TestClaudeTeamSignedOutPremiumHandsOffWithoutExhaustion(t *testing.T) {
	for _, source := range []string{"refusal", "cleared-store", "capture-cleared-store", "already-refused"} {
		t.Run(source, func(t *testing.T) {
			tm, admin, cfg := teamFixture(t)
			user := "u2@example.com · Team"
			release, ok := tm.track(user)
			if !ok {
				t.Fatal("healthy request not admitted")
			}
			defer release()
			// Another Premium seat remains healthy; only the broken one
			// hands off, even with no exhausted quota in the journal.
			switch source {
			case "cleared-store", "capture-cleared-store":
				dir := claudeAccountDir(user)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				clear := func(string) error {
					return writePrivate(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`))
				}
				if source == "capture-cleared-store" {
					if err := inSavedClaudeDir(user, clear); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := clear(dir); err != nil {
						t.Fatal(err)
					}
					if _, err := claudeSavedDir(user); err == nil {
						t.Fatal("cleared credential admitted")
					}
				}
			default:
				NoteClaudeSignInFailure(user, ClaudeLoginVersion(user), "OAuth token revoked")
			}
			tm.mu.Lock()
			closed, pending := tm.closed[teamID(2)], tm.requested
			tm.mu.Unlock()
			if !closed || !pending {
				t.Fatal("confirmed sign-out did not close admissions and request handoff")
			}
			select {
			case <-tm.wake:
			default:
				t.Fatal("sign-out did not wake controller")
			}
			if again, admitted := tm.track(user); admitted {
				again()
				t.Fatal("stale plan admitted signed-out account")
			}
			if source == "already-refused" {
				// Startup and periodic reconciliation must also recover
				// a refusal persisted before this controller was running.
				tm.mu.Lock()
				tm.requested = false
				tm.mu.Unlock()
			}
			from, to, err := tm.choose(cfg, admin.seats, "")
			if err != nil || from != teamID(2) || to != teamID(3) {
				t.Fatalf("signed-out Premium was stranded: %q -> %q: %v", from, to, err)
			}
			// A cancelled drain must leave every cloud seat as it was.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := tm.transfer(ctx, cfg, admin, admin.seats, from, to); err == nil {
				t.Fatal("transfer ignored active request")
			}
			if len(admin.writes) != 0 {
				t.Fatal("seat changed before active request finished")
			}
			release()
			if err := tm.transfer(context.Background(), cfg, admin, admin.seats, from, to); err != nil {
				t.Fatal(err)
			}
			if len(admin.writes) != 2 || claudeLapseOf(user) == "" {
				t.Fatal("handoff did not preserve refusal or seat count")
			}
			if release, ok := tm.track("u3@example.com · Team"); !ok {
				t.Fatal("replacement not admitted")
			} else {
				release()
			}
		})
	}
}

func TestClaudeTeamTemporaryAndStaleAuthErrorsDoNotRotate(t *testing.T) {
	for _, message := range []string{
		"Failed to refresh OAuth token: another Claude Code process is refreshing it",
		"OAuth token refresh failed (HTTP 503)",
		"network connection timed out",
		"OAuth token revoked",
	} {
		t.Run(message, func(t *testing.T) {
			tm, admin, cfg := teamFixture(t)
			user := "u2@example.com · Team"
			version := ClaudeLoginVersion(user)
			if message == "OAuth token revoked" {
				version = "a-previous-credential"
			}
			NoteClaudeSignInFailure(user, version, message)
			from, _, err := tm.choose(cfg, admin.seats, "")
			if err != nil || from != "" || claudeLapseOf(user) != "" {
				t.Fatalf("temporary/stale failure moved healthy seat: %q %v", from, err)
			}
			tm.mu.Lock()
			closed, requested := tm.closed[teamID(2)], tm.requested
			tm.mu.Unlock()
			if closed || requested {
				t.Fatal("temporary/stale failure closed healthy account")
			}
		})
	}
}

func TestClaudeTeamSignedOutHasNoResetToOffer(t *testing.T) {
	tm, admin, cfg := teamFixture(t)
	for _, n := range []int{2, 3, 4} {
		user := fmt.Sprintf("u%d@example.com · Team", n)
		NoteClaudeSignInFailure(user, ClaudeLoginVersion(user), "OAuth token revoked")
	}
	from, to, err := tm.choose(cfg, admin.seats, "")
	if from == "" || to != "" || err == nil {
		t.Fatalf("no-alternative sign-out: %q -> %q: %v", from, to, err)
	}
	if at, ok := tm.reset(); ok || !at.Equal(time.Time{}) {
		t.Fatal("authentication failure invented quota reset")
	}
}
