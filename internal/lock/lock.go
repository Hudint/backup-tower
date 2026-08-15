// Package lock serialises work on a single container.
//
// The locks are advisory file locks, which is the point: the daemon's update
// pass and a `backup-tower snapshot` typed at the same moment are different
// processes, and nothing inside either of them can see the other. Only the
// filesystem can arbitrate. Two goroutines in one process are covered by the
// same mechanism, so there is one rule rather than two.
//
// What this prevents is not theoretical. An update stops a container, archives
// its volumes and replaces it; a snapshot running against the same container
// halfway through that would capture a state that never existed, and a restore
// from it would look successful.
package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ErrBusy is returned by TryAcquire when someone else holds the lock.
var ErrBusy = errors.New("another backup-tower run is working on this container")

// Locker hands out per-container locks rooted at one directory.
type Locker struct {
	dir string
}

// New creates a locker. The directory is created if it does not exist; a
// dot-prefixed name keeps it out of the store's own listings.
func New(root string) (*Locker, error) {
	dir := filepath.Join(root, ".locks")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create lock directory %q: %w", dir, err)
	}
	return &Locker{dir: dir}, nil
}

// Release gives the lock back. It is safe to call more than once.
type Release func()

// TryAcquire takes the lock if it is free and returns ErrBusy if it is not.
//
// This is what the daemon uses. Waiting would stall every other container
// behind the busy one, and the work being skipped is due again on the next
// tick — a late backup is a much smaller problem than a stalled pass.
func (l *Locker) TryAcquire(name string) (Release, error) {
	return l.acquire(name, false, nil)
}

// Acquire waits until the lock is free, or until ctx is done.
//
// This is what a person at a terminal gets: they asked for this container
// specifically, so waiting for the daemon to finish with it is what they want,
// not being turned away.
func (l *Locker) Acquire(ctx context.Context, name string) (Release, error) {
	return l.acquire(name, true, ctx)
}

func (l *Locker) acquire(name string, wait bool, ctx context.Context) (Release, error) {
	path := filepath.Join(l.dir, safeName(name)+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", path, err)
	}

	release := func() {
		// Closing the descriptor drops the flock. Doing it in this order means
		// a crash releases the lock too, which a lock file holding a PID would
		// not: the file outlives the process that wrote it.
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}

	const retry = 200 * time.Millisecond
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock %q: %w", name, err)
		}
		if !wait {
			f.Close()
			return nil, fmt.Errorf("%s: %w", name, ErrBusy)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(retry):
		}
	}
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// safeName keeps a container name from escaping the lock directory or colliding
// with the filesystem's own rules.
func safeName(name string) string {
	s := unsafeName.ReplaceAllString(name, "_")
	s = strings.TrimLeft(s, ".")
	if s == "" {
		s = "unnamed"
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
