package claudelogin

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

type Status struct {
	Running bool   `json:"running"`
	Listen  string `json:"listen,omitempty"`
	Error   string `json:"error,omitempty"`
}

var state struct {
	sync.Mutex
	value Status
}

func ReadStatus() Status { state.Lock(); defer state.Unlock(); return state.value }
func setStatus(s Status) { state.Lock(); state.value = s; state.Unlock() }

// ListenerBusy also works from a short-lived CLI process, whose in-memory
// status cannot describe the proxy owned by the desktop gateway.
func ListenerBusy() (bool, error) {
	f, err := os.OpenFile(Path()+".proxy.lock", os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	got, err := filelock.TryLock(f)
	if got {
		filelock.Unlock(f)
	}
	return !got, err
}

// Keep runs only in the gateway owner. Update handover waits for the old
// proxy owner; a failed read never turns selected inference into passthrough.
// Once started, disabling rules keeps the proxy reachable until Magpie exits,
// so clients still configured to use it can reach the official service.
func Keep(ctx context.Context, gateway string, official http.RoundTripper) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		c, err := Load()
		if err != nil {
			setStatus(Status{Error: err.Error()})
		} else if c.Enabled {
			err = runOwned(ctx, c, gateway, official)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				setStatus(Status{Error: err.Error()})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func runOwned(ctx context.Context, c Config, gateway string, official http.RoundTripper) error {
	unlock, err := filelock.LockContext(ctx, Path()+".proxy.lock")
	if err != nil {
		return err
	}
	defer unlock()
	// The previous process may have changed configuration during handover.
	c, err = Load()
	if err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	u, err := GatewayURL(gateway)
	if err != nil {
		return err
	}
	a, err := EnsureAuthority(ctx, CertDir())
	if err != nil {
		return err
	}
	p := &Proxy{Authority: a, Config: Load, Gateway: u, Official: official, Local: &http.Transport{}}
	if c.GatewayUsage && c.UsageOrganization != "" {
		if !ReadWebCertificate().Trusted {
			return fmt.Errorf("trust the separately prepared claude.ai quota CA before enabling organization usage")
		}
		p.WebAuthority, err = EnsureWebAuthority(ctx, WebCertDir())
		if err != nil {
			return err
		}
	}
	defer p.Close()
	setStatus(Status{Listen: c.Listen})
	// Bind first, then publish Running; Serve's public variant is also used
	// by tests and owns the same bounded cancellation path.
	err = serveReady(ctx, c, p, func() { setStatus(Status{Running: true, Listen: c.Listen}) })
	setStatus(Status{})
	if err != nil {
		return fmt.Errorf("Claude login proxy: %w", err)
	}
	return nil
}
