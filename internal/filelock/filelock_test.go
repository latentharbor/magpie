package filelock

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLockContextCancellationAndErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "controller.lock")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if unlock, err := LockContext(ctx, path); !errors.Is(err, context.Canceled) || unlock != nil {
		t.Fatalf("cancelled acquisition: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("cancelled acquisition touched the filesystem")
	}
	unlock, err := LockContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if release, err := LockContext(ctx, path); !errors.Is(err, context.DeadlineExceeded) || release != nil {
		t.Fatalf("contended acquisition: %v", err)
	}
	unlock()
	// An I/O error is reported rather than treated as contention forever.
	if release, err := LockContext(context.Background(), filepath.Dir(path)); err == nil || release != nil {
		t.Fatal("directory accepted as a lock file")
	}
}

// TestLockHolder is the other magpie of TestLockAcrossProcesses: it takes
// the lock, says so, and holds it until it is killed.
func TestLockHolder(t *testing.T) {
	path := os.Getenv("MAGPIE_FILELOCK_HOLD")
	if path == "" {
		t.Skip("run by TestLockAcrossProcesses")
	}
	if _, held := Lock(path, time.Second); !held {
		t.Fatal("not held")
	}
	os.Stdout.WriteString("locked\n")
	time.Sleep(time.Minute)
}

// Another magpie holding the lock is waited for, up to the wait and no
// longer; once it ends, however it ends, the lock is free again; and a
// Mutex holds it while it is held itself.
func TestLockAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json.lock")
	holder := exec.Command(os.Args[0], "-test.run=^TestLockHolder$", "-test.count=1")
	holder.Env = append(os.Environ(), "MAGPIE_FILELOCK_HOLD="+path)
	out, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "locked\n" {
		t.Fatalf("the other magpie: %q", line)
	}

	start := time.Now()
	unlock, held := Lock(path, 300*time.Millisecond)
	unlock()
	if held || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("held %v after %v, while another magpie held it", held, time.Since(start))
	}

	holder.Process.Kill()
	holder.Wait()
	unlock, held = Lock(path, 5*time.Second)
	if !held {
		t.Fatal("not held after the other magpie ended")
	}
	unlock()

	m := Mutex{Path: func() string { return path }}
	m.Lock()
	if unlock, held := Lock(path, 50*time.Millisecond); held {
		unlock()
		t.Fatal("taken while a Mutex held it")
	}
	m.Unlock()
	unlock, held = Lock(path, time.Second)
	if !held {
		t.Fatal("not free after the Mutex let go")
	}
	unlock()
}

// #1665: each take asked the system to make the lock file's folder before
// it opened the file, ~190 times in a Windows start's first 25s, each one
// work an antivirus filter driver adds its own to. The folder is made only
// when it is missing.
func TestLockMakesFolderOnlyWhenMissing(t *testing.T) {
	made := 0
	was := mkdirAll
	mkdirAll = func(p string, m os.FileMode) error { made++; return was(p, m) }
	t.Cleanup(func() { mkdirAll = was })

	path := filepath.Join(t.TempDir(), "new", "f.json.lock")
	unlock, held := Lock(path, time.Second)
	if !held {
		t.Fatal("not held in a folder not there yet")
	}
	unlock()
	if made != 1 {
		t.Fatalf("missing folder made %d times, want 1", made)
	}
	m := Mutex{Path: func() string { return path }}
	for range 20 {
		m.Lock()
		m.Unlock()
	}
	if made != 1 {
		t.Fatalf("20 takes in a folder that is there made it %d more times", made-1)
	}
}
