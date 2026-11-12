package gateway

import (
	"sync"
	"testing"
	"time"
)

type blockedPruneHub struct {
	fakeHub
	entered, release, stopped chan struct{}
	once, stopOnce           sync.Once
}

func (h *blockedPruneHub) Prune() {
	h.once.Do(func() { close(h.entered) })
	<-h.release
}

func (h *blockedPruneHub) Stop() {
	h.stopOnce.Do(func() { close(h.stopped) })
}

// Inject a prune that cannot finish until released. The HTTP shutdown's Stop
// callback proves shutdown has advanced before checking that the gateway still
// waits. The short negative observation cannot fail a correct implementation
// under load: the prune is blocked on a channel, not on a duration.
func TestGatewayWaitsForHubPruner(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:0")
	h := &blockedPruneHub{entered: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{})}
	oldHub, oldEvery := MCPHub, hubPruneEvery
	MCPHub, hubPruneEvery = h, time.Millisecond
	t.Cleanup(func() { MCPHub, hubPruneEvery = oldHub, oldEvery })
	stop, done := listening(t, New())
	ended := make(chan struct{})
	go func() { <-done; close(ended) }()
	var release sync.Once
	t.Cleanup(func() { stop(); release.Do(func() { close(h.release) }); <-ended })
	select {
	case <-h.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("pruner never started")
	}
	stop()
	select {
	case <-h.stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not reach hub shutdown")
	}
	select {
	case <-ended:
		t.Error("gateway returned while its hub pruner was still running")
	case <-time.After(100 * time.Millisecond):
	}
	release.Do(func() { close(h.release) })
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not finish after its pruner ended")
	}
}
