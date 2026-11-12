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
	mu sync.Mutex
	closed map[string]bool
	active map[string]int
	changed chan struct{}
}

func NewAdmissions() *Admissions {
	return &Admissions{closed:map[string]bool{},active:map[string]int{},changed:make(chan struct{})}
}
func(a *Admissions) signal(){close(a.changed);a.changed=make(chan struct{})}
func(a *Admissions) SetClosed(id string,closed bool){a.mu.Lock();a.closed[id]=closed;a.signal();a.mu.Unlock()}
func(a *Admissions) Closed(id string)bool{a.mu.Lock();defer a.mu.Unlock();return a.closed[id]}
func(a *Admissions) Forget(id string){a.mu.Lock();delete(a.closed,id);a.signal();a.mu.Unlock()}
func(a *Admissions) ResetClosed(){a.mu.Lock();a.closed=map[string]bool{};a.signal();a.mu.Unlock()}
func(a *Admissions) Active(id string)int{a.mu.Lock();defer a.mu.Unlock();return a.active[id]}
func(a *Admissions) Counts()map[string]int{a.mu.Lock();defer a.mu.Unlock();return maps.Clone(a.active)}

func(a *Admissions) Enter(id string)(func(),bool){
	a.mu.Lock()
	if a.closed[id]{a.mu.Unlock();return func(){},false}
	a.active[id]++;a.mu.Unlock()
	var once sync.Once
	return func(){once.Do(func(){a.mu.Lock();a.active[id]--;a.signal();a.mu.Unlock()})},true
}

func(a *Admissions) Drain(ctx context.Context,id string)error{
	for {
		a.mu.Lock();n,ch:=a.active[id],a.changed;a.mu.Unlock()
		if n==0{return nil}
		select{
		case <-ctx.Done():return errors.New("Claude Team drain timed out; no assignment was changed")
		case <-ch:
		}
	}
}
