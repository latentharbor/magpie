package provider

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

func TestClaudeTeamControllerWaitsForPreviousOwner(t *testing.T) {
	tm, admin, cfg := teamFixture(t)
	old := UsageClaudeVia(func(context.Context, string) (string, error) { return teamUsageFixture(), nil })
	t.Cleanup(func() { UsageClaudeVia(old) })
	unlock, held := filelock.Lock(tm.configPath()+".controller.lock", 0)
	if !held {
		t.Fatal("fixture could not hold controller lock")
	}
	t.Cleanup(func() { unlock() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); tm.keep(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.After(10 * time.Second)
	for {
		tm.mu.Lock()
		waiting, running := tm.err != "", tm.running
		tm.mu.Unlock()
		if running {
			t.Fatal("controller started without ownership")
		}
		if waiting {
			break
		}
		select {
		case <-done:
			t.Fatal("controller abandoned the update handover")
		case <-deadline:
			t.Fatal("controller did not report waiting for ownership")
		case <-time.After(time.Millisecond):
		}
	}
	// The previous owner can finish saving while its replacement waits.
	// Startup must read that final config only after ownership transfers.
	cfg.Name = "saved by previous owner"
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(tm.configPath(), b, 0600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(tm.statePath())
	if err != nil {
		t.Fatal(err)
	}
	var state claudeTeamDisk
	if err := json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	state.History = append(state.History, ClaudeTeamEvent{At: time.Now(), Result: "previous owner finished"})
	b, _ = json.Marshal(state)
	if err := os.WriteFile(tm.statePath(), b, 0600); err != nil {
		t.Fatal(err)
	}
	unlock()
	// Cleanup must not close the same descriptor again after another lock
	// has been acquired; replace its closure with a no-op.
	unlock = func() {}
	deadline = time.After(10 * time.Second)
	for {
		tm.mu.Lock()
		ready := tm.running && tm.cfg.Name == cfg.Name && tm.err == "" && len(tm.disk.History) == len(state.History)
		tm.mu.Unlock()
		if ready {
			break
		}
		select {
		case <-done:
			t.Fatal("controller exited instead of acquiring the released lock")
		case <-deadline:
			t.Fatal("controller did not recover after the old owner released its lock")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	admin.mu.Lock()
	defer admin.mu.Unlock()
	if len(admin.writes) != 0 {
		t.Fatal("ownership handover moved a seat")
	}
	if release, ok := filelock.Lock(tm.configPath()+".controller.lock", 0); !ok {
		t.Fatal("shutdown leaked controller ownership")
	} else {
		release()
	}
}

func TestClaudeTeamControllerReloadRejectsCorruptJournal(t *testing.T) {
	tm, admin, _ := teamFixture(t)
	// The discovery/save phase already loaded a valid journal. An owner's
	// final on-disk state must not be replaced by that earlier cached copy.
	if err := os.WriteFile(tm.statePath(), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); tm.keep(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("controller ignored the owner's unreadable recovery journal")
	}
	tm.mu.Lock()
	running, enabled, message := tm.running, tm.cfg.Enabled, tm.err
	tm.mu.Unlock()
	if running || enabled || message == "" {
		t.Fatal("corrupt recovery state did not quarantine controller startup")
	}
	contents, err := os.ReadFile(tm.statePath())
	if err != nil || string(contents) != "{broken" {
		t.Fatal("controller overwrote the unreadable journal", err)
	}
	admin.mu.Lock()
	defer admin.mu.Unlock()
	if len(admin.writes) != 0 {
		t.Fatal("corrupt journal allowed a seat mutation")
	}
}

func TestClaudeTeamControllerWaitingCanBeCancelled(t *testing.T) {
	tm, _, _ := teamFixture(t)
	unlock, held := filelock.Lock(tm.configPath()+".controller.lock", 0)
	if !held {
		t.Fatal("fixture could not hold controller lock")
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); tm.keep(ctx) }()
	select {
	case <-done:
		t.Fatal("controller exited before cancellation or ownership transfer")
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked waiting for another controller")
	}
}
