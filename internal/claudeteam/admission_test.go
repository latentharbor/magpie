package claudeteam

import (
	"context"
	"testing"
)

func TestAdmissionClosesOnlyTheDrainingAccount(t *testing.T) {
	a := NewAdmissions()
	release, ok := a.Enter("a")
	if !ok {
		t.Fatal("initial admission")
	}
	a.SetClosed("a", true)
	if _, ok := a.Enter("a"); ok {
		t.Fatal("stale candidate admitted")
	}
	other, ok := a.Enter("b")
	if !ok {
		t.Fatal("other Premium account was blocked")
	}
	other()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if a.Drain(ctx, "a") == nil {
		t.Fatal("live request ignored")
	}
	release()
	release()
	if a.Active("a") != 0 {
		t.Fatal("release counted more than once")
	}
	if err := a.Drain(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	a.SetClosed("a", false)
	done, ok := a.Enter("a")
	if !ok {
		t.Fatal("restored account stayed closed")
	}
	done()
}
