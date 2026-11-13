package claudeteam

import (
	"context"
	"errors"
	"maps"
	"sync"
)

// Admissions closes an account locally before draining it. It performs no
// file reads or network work and is shared by all inference transports.
type Admissions struct {
	mu      sync.Mutex
	closed  map[string]bool
	active  map[string]int
	changed chan struct{}
}

func NewAdmissions() *Admissions {
	return &Admissions{closed: map[string]bool{}, active: map[string]int{}}
}
func (a *Admissions) signal() {
	if a.changed != nil {
		close(a.changed)
		a.changed = nil
	}
}
func (a *Admissions) SetClosed(id string, closed bool) {
	a.mu.Lock()
	a.closed[id] = closed
	a.signal()
	a.mu.Unlock()
}
func (a *Admissions) Closed(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed[id]
}
func (a *Admissions) Forget(id string) {
	a.mu.Lock()
	delete(a.closed, id)
	a.signal()
	a.mu.Unlock()
}
func (a *Admissions) Active(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active[id]
}
func (a *Admissions) Counts() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.active)
}

func (a *Admissions) Enter(id string) (func(), bool) {
	a.mu.Lock()
	if a.closed[id] {
		a.mu.Unlock()
		return func() {}, false
	}
	a.active[id]++
	a.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { a.mu.Lock(); a.active[id]--; a.signal(); a.mu.Unlock() }) }, true
}

func (a *Admissions) Drain(ctx context.Context, id string) error {
	for {
		a.mu.Lock()
		if a.active[id] == 0 {
			a.mu.Unlock()
			return nil
		}
		if a.changed == nil {
			a.changed = make(chan struct{})
		}
		ch := a.changed
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return errors.New("Claude Team drain timed out; no assignment was changed")
		case <-ch:
		}
	}
}
