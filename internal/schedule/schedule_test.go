package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/Hudint/backup-tower/internal/snapshot"
	"github.com/Hudint/backup-tower/internal/snapshot/store"
)

func newStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// record writes a snapshot with a given creation time and trigger.
func record(t *testing.T, st store.Store, container string, at time.Time, trigger snapshot.Trigger) {
	t.Helper()
	ctx := context.Background()
	m := &snapshot.Manifest{
		SchemaVersion: snapshot.SchemaVersion,
		ID:            snapshot.NewID(at),
		CreatedAt:     at.UTC(),
		Trigger:       trigger,
		Container:     snapshot.ContainerRef{Name: container},
	}
	b, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.Begin(ctx, container, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	w, err := tx.Create(ctx, snapshot.ManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(b)
	w.Close()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNothingIsOwedRetroactively(t *testing.T) {
	// A container that has never had a scheduled backup must not look overdue
	// for every fire time since the epoch.
	st := newStore(t)
	start := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	c := NewChecker(st, start)

	due, err := c.Check(context.Background(), "app", "0 4 * * *", start.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if due != nil {
		t.Errorf("a container with no history was reported due immediately: %+v", due)
	}
}

func TestDueOnceTheFireTimeHasPassed(t *testing.T) {
	st := newStore(t)
	start := time.Date(2026, 8, 13, 3, 0, 0, 0, time.UTC)
	c := NewChecker(st, start)

	// 04:00 has not arrived yet.
	if due, _ := c.Check(context.Background(), "app", "0 4 * * *", start.Add(30*time.Minute)); due != nil {
		t.Error("reported due before the scheduled time")
	}
	// 04:01 — it has.
	due, err := c.Check(context.Background(), "app", "0 4 * * *", start.Add(61*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if due == nil {
		t.Fatal("not reported due after the scheduled time passed")
	}
	if !due.ScheduledFor.Equal(time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("ScheduledFor = %s, want 04:00", due.ScheduledFor)
	}
}

func TestTheLastRunComesFromTheStore(t *testing.T) {
	// Reading it back from the snapshots means a restarted daemon neither
	// repeats a backup it already took nor skips one it owes.
	st := newStore(t)
	yesterday4am := time.Date(2026, 8, 12, 4, 0, 0, 0, time.UTC)
	record(t, st, "app", yesterday4am, snapshot.TriggerSchedule)

	// A daemon that started only minutes ago still sees the older run.
	start := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	c := NewChecker(st, start)

	due, err := c.Check(context.Background(), "app", "0 4 * * *", start)
	if err != nil {
		t.Fatal(err)
	}
	if due == nil {
		t.Fatal("today's backup was not reported due although the last one was yesterday")
	}
	if !due.LastRun.Equal(yesterday4am) {
		t.Errorf("LastRun = %s, want yesterday 04:00", due.LastRun)
	}
	if !due.ScheduledFor.Equal(time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("ScheduledFor = %s, want today 04:00", due.ScheduledFor)
	}
}

func TestOnlyScheduledSnapshotsCount(t *testing.T) {
	// An update or a hand-taken snapshot does not satisfy a schedule; treating
	// it as one would silently skip the backup that was actually asked for.
	st := newStore(t)
	start := time.Date(2026, 8, 13, 3, 0, 0, 0, time.UTC)
	record(t, st, "app", start.Add(30*time.Minute), snapshot.TriggerUpdate)
	record(t, st, "app", start.Add(40*time.Minute), snapshot.TriggerManual)

	c := NewChecker(st, start)
	due, err := c.Check(context.Background(), "app", "0 4 * * *", start.Add(61*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if due == nil {
		t.Fatal("the scheduled backup was skipped because of an unrelated snapshot")
	}
	if !due.LastRun.IsZero() {
		t.Errorf("LastRun = %s, want zero — no scheduled backup has ever run", due.LastRun)
	}
}

func TestAlreadyRunThisCycleIsNotDueAgain(t *testing.T) {
	st := newStore(t)
	start := time.Date(2026, 8, 13, 3, 0, 0, 0, time.UTC)
	record(t, st, "app", time.Date(2026, 8, 13, 4, 0, 5, 0, time.UTC), snapshot.TriggerSchedule)

	c := NewChecker(st, start)
	due, err := c.Check(context.Background(), "app", "0 4 * * *", time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if due != nil {
		t.Errorf("today's backup was reported due a second time: %+v", due)
	}
}

func TestInvalidExpressionsAreRejected(t *testing.T) {
	if _, err := Parse("not a cron expression"); err == nil {
		t.Error("nonsense was accepted as a cron expression")
	}
	if _, err := Parse("0 4 * * *"); err != nil {
		t.Errorf("a standard five-field expression was rejected: %v", err)
	}
	if _, err := Parse("@daily"); err != nil {
		t.Errorf("a descriptor was rejected: %v", err)
	}
}
