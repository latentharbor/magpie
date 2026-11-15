package claudeteam

import (
	"context"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

func TestRuntimeSchedulesPendingRecoveryWithoutAnotherWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var at []time.Time
		r := Runtime{LockPath: filepath.Join(t.TempDir(), "controller.lock"), Waiting: func() {}, Open: func() error { return nil },
			Step: func(context.Context) {
				at = append(at, time.Now())
				if len(at) == 2 {
					cancel()
				}
			},
			NextDelay: func() time.Duration { return 30 * time.Second }, Stopped: func() {}, Failed: func(err error) { t.Error(err) }}
		r.Run(ctx)
		if len(at) != 2 || at[1].Sub(at[0]) != 30*time.Second {
			t.Fatalf("pending recovery waited for the normal five-minute poll: %v", at)
		}
	})
}

func TestRuntimeDoesNotOpenBeforeOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.lock")
	unlock, err := filelock.LockContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan struct{})
	done := make(chan struct{})
	opened := false
	r := Runtime{LockPath: path, Waiting: func() { close(waiting) }, Open: func() error { opened = true; return nil },
		Step: func(context.Context) {}, Stopped: func() {}, Failed: func(error) {}}
	go func() { defer close(done); r.Run(ctx) }()
	<-waiting
	cancel()
	<-done
	if opened {
		t.Fatal("read authoritative state without owning the controller")
	}
}

func TestRuntimeKeepsOwnershipUntilAcceptedStepEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.lock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r := Runtime{LockPath: path, Waiting: func() {}, Open: func() error { return nil },
		Step: func(context.Context) { close(entered); <-finish }, Stopped: func() {}, Failed: func(error) {}}
	go func() { defer close(done); r.Run(ctx) }()
	<-entered
	cancel()
	unlocked, held := filelock.Lock(path, 0)
	if held {
		unlocked()
		close(finish)
		<-done
		t.Fatal("released ownership during a transfer")
	}
	close(finish)
	<-done
	unlocked, held = filelock.Lock(path, 0)
	if !held {
		t.Fatal("ownership survived shutdown")
	}
	unlocked()
}
