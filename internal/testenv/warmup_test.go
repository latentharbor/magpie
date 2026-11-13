package testenv

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"
)

func warmupProcess(t *testing.T, script string) *os.Process {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix stand-in process")
	}
	p, err := os.StartProcess("/bin/sh", []string{"sh", "-c", script}, &os.ProcAttr{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Kill(); p.Wait() })
	return p
}

func TestWarmupWaitKillsAndReapsStalledChild(t *testing.T) {
	// Builtins only: killing this shell leaves no child such as sleep behind.
	p := warmupProcess(t, "while :; do :; done")
	done := make(chan error, 1)
	go func() {
		_, err := waitWarmup(p, 50*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stalled warm-up returned %v, want deadline", err)
		}
	case <-time.After(10 * time.Second):
		p.Kill()
		<-done
		t.Fatal("warm-up did not return after its deadline")
	}
	if err := p.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("warm-up was not reaped before return: %v", err)
	}
}

func TestWarmupWaitKeepsCompletedExitStatus(t *testing.T) {
	for _, script := range []string{"exit 0", "exit 1"} {
		p := warmupProcess(t, script)
		state, err := waitWarmup(p, 10*time.Second)
		if err != nil || state == nil || state.Success() != (script == "exit 0") {
			t.Fatalf("%s: state=%v error=%v", script, state, err)
		}
	}
}
