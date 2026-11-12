package claudeteam

import (
	"context"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

// Runtime owns the process lease and background schedule. A host restores
// its local observations in Open only after the old controller has released
// the lease. Step never runs on the request path. Shutdown waits for an
// accepted step to finish before relinquishing ownership.
type Runtime struct {
	LockPath string
	Wake <-chan struct{}
	Waiting func()
	Open func() error
	Step func(context.Context)
	Stopped func()
	Failed func(error)
}

func (r Runtime) Run(ctx context.Context) {
	r.Waiting()
	unlock, err := filelock.LockContext(ctx, r.LockPath)
	if err != nil { r.Failed(err); return }
	defer unlock()
	if ctx.Err() != nil { return }
	if err := r.Open(); err != nil { r.Failed(err); return }
	defer r.Stopped()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-timer.C:
		case <-r.Wake:
		}
		if ctx.Err() != nil { return }
		r.Step(ctx)
		timer.Reset(5*time.Minute)
	}
}
