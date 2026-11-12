package claudelogin

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/filelock"
)

func TestListenerOwnershipIsVisibleAcrossStatusReaders(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	if busy, err := ListenerBusy(); err != nil || busy {
		t.Fatalf("missing listener: %v %v", busy, err)
	}
	unlock, err := filelock.LockContext(context.Background(), Path()+".proxy.lock")
	if err != nil {
		t.Fatal(err)
	}
	if busy, err := ListenerBusy(); err != nil || !busy {
		unlock()
		t.Fatalf("live listener was missed: %v %v", busy, err)
	}
	unlock()
	if busy, err := ListenerBusy(); err != nil || busy {
		t.Fatalf("stale lock file blocked reconfiguration: %v %v", busy, err)
	}
}
