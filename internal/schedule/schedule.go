// Package schedule decides when a container is due for a scheduled backup.
//
// The last run is not tracked in a state file. It is read back from the backup
// store, which already records exactly when each snapshot was taken and why.
// Deriving it that way means a restarted daemon neither repeats a backup it
// already took nor silently skips one it owes, and there is no second source of
// truth to drift out of step with the first.
package schedule

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Hudint/backup-tower/internal/snapshot"
	"github.com/Hudint/backup-tower/internal/snapshot/store"
)

// Parser accepts standard five-field cron expressions plus the usual
// descriptors, which is what people expect from a crontab line.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Parse validates a cron expression.
func Parse(expr string) (cron.Schedule, error) {
	s, err := parser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("cron expression %q: %w", expr, err)
	}
	return s, nil
}

// Due reports whether a container's scheduled backup is owed at now.
type Due struct {
	Container string
	// LastRun is when the last scheduled backup for this container happened,
	// zero if there has never been one.
	LastRun time.Time
	// ScheduledFor is the fire time that is now due.
	ScheduledFor time.Time
}

// Checker decides what is due.
type Checker struct {
	store store.Store
	// since bounds how far back a first run is computed from. Without it, a
	// container that has never had a scheduled backup would look overdue for
	// every fire time since the epoch.
	since time.Time
}

// NewChecker builds a checker. The since time is normally when the daemon
// started: a container with no history is not retroactively owed anything.
func NewChecker(st store.Store, since time.Time) *Checker {
	return &Checker{store: st, since: since}
}

// Check reports whether a container with the given expression is due now.
func (c *Checker) Check(ctx context.Context, container, expr string, now time.Time) (*Due, error) {
	sched, err := Parse(expr)
	if err != nil {
		return nil, err
	}

	last, err := c.lastScheduled(ctx, container)
	if err != nil {
		return nil, err
	}

	from := last
	if from.IsZero() || from.Before(c.since) {
		// Never run, or last run before this daemon's window: start counting
		// from the daemon's start so nothing fires retroactively.
		if from.IsZero() {
			from = c.since
		}
	}

	next := sched.Next(from)
	if next.After(now) {
		return nil, nil
	}
	return &Due{Container: container, LastRun: last, ScheduledFor: next}, nil
}

// lastScheduled finds when the most recent scheduled backup of a container ran.
func (c *Checker) lastScheduled(ctx context.Context, container string) (time.Time, error) {
	ids, err := c.store.Snapshots(ctx, container)
	if err != nil {
		return time.Time{}, err
	}
	// Newest last, so walk backwards and stop at the first scheduled one.
	for i := len(ids) - 1; i >= 0; i-- {
		m, err := snapshot.LoadManifest(ctx, c.store, container, ids[i])
		if err != nil {
			continue // an unreadable manifest tells us nothing either way
		}
		if m.Trigger != snapshot.TriggerSchedule {
			continue
		}
		if !m.CreatedAt.IsZero() {
			return m.CreatedAt, nil
		}
		if t, err := snapshot.ParseID(ids[i]); err == nil {
			return t, nil
		}
	}
	return time.Time{}, nil
}

// NextRun reports when a container would next be backed up, for display.
func NextRun(expr string, after time.Time) (time.Time, error) {
	sched, err := Parse(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after), nil
}
