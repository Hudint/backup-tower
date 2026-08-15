package lock

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newLocker(t *testing.T) *Locker {
	t.Helper()
	l, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestASecondHolderIsTurnedAway(t *testing.T) {
	l := newLocker(t)

	release, err := l.TryAcquire("app")
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer release()

	if _, err := l.TryAcquire("app"); !errors.Is(err, ErrBusy) {
		t.Errorf("second acquire returned %v, want ErrBusy", err)
	}
}

func TestDifferentContainersDoNotBlockEachOther(t *testing.T) {
	l := newLocker(t)

	a, err := l.TryAcquire("app")
	if err != nil {
		t.Fatal(err)
	}
	defer a()

	b, err := l.TryAcquire("db")
	if err != nil {
		t.Errorf("locking a different container was refused: %v", err)
		return
	}
	b()
}

func TestReleasingLetsTheNextOneIn(t *testing.T) {
	l := newLocker(t)

	release, err := l.TryAcquire("app")
	if err != nil {
		t.Fatal(err)
	}
	release()

	again, err := l.TryAcquire("app")
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	again()
}

func TestAcquireWaitsRatherThanFailing(t *testing.T) {
	l := newLocker(t)

	release, err := l.TryAcquire("app")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		r, err := l.Acquire(context.Background(), "app")
		if r != nil {
			r()
		}
		done <- err
	}()

	// Still held: the waiter must not have got in.
	select {
	case err := <-done:
		t.Fatalf("Acquire returned %v while the lock was held", err)
	case <-time.After(300 * time.Millisecond):
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Acquire failed after the lock was released: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("Acquire did not return after the lock was released")
	}
}

func TestAcquireGivesUpWhenTheContextDoes(t *testing.T) {
	l := newLocker(t)
	release, err := l.TryAcquire("app")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx, "app"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Acquire returned %v, want the context error", err)
	}
}

// TestTheLockHoldsAcrossProcesses is the one that matters. Two goroutines could
// be serialised with a mutex; the daemon and a command typed at a terminal
// cannot, and that is the collision this package exists to prevent.
func TestTheLockHoldsAcrossProcesses(t *testing.T) {
	if os.Getenv("BT_LOCK_CHILD") != "" {
		// Child half: report whether the lock could be taken.
		l, err := New(os.Getenv("BT_LOCK_DIR"))
		if err != nil {
			os.Exit(3)
		}
		if _, err := l.TryAcquire("app"); errors.Is(err, ErrBusy) {
			os.Exit(0) // correctly turned away
		}
		os.Exit(1)
	}

	root := t.TempDir()
	l, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	release, err := l.TryAcquire("app")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	cmd := exec.Command(os.Args[0], "-test.run=TestTheLockHoldsAcrossProcesses")
	cmd.Env = append(os.Environ(), "BT_LOCK_CHILD=1", "BT_LOCK_DIR="+root)
	err = cmd.Run()
	if err != nil {
		t.Fatalf("a separate process was not turned away by the lock: %v", err)
	}
}

func TestNamesCannotEscapeTheLockDirectory(t *testing.T) {
	l := newLocker(t)
	release, err := l.TryAcquire("../../etc/passwd")
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer release()

	// Assert the property rather than the exact spelling: the file must land in
	// the lock directory itself, with nothing in its name that could walk out
	// of it.
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one lock file, got %d", len(entries))
	}
	name := entries[0].Name()
	if strings.ContainsAny(name, `/\`) {
		t.Errorf("lock file %q contains a path separator", name)
	}
	if strings.HasPrefix(name, ".") {
		t.Errorf("lock file %q starts with a dot and would be skipped by listings", name)
	}
	if _, err := os.Stat(filepath.Join(l.dir, name)); err != nil {
		t.Errorf("the lock file is not where it should be: %v", err)
	}
}
