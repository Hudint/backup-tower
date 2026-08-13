package snapshot

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Hudint/backup-tower/internal/snapshot/store"
)

// writeSnapshot puts a snapshot into the store with a chosen age and trigger.
func writeSnapshot(t *testing.T, st store.Store, container string, age time.Duration, trigger Trigger, pin string) string {
	t.Helper()
	ctx := context.Background()
	created := time.Now().Add(-age)
	id := NewID(created)

	m := &Manifest{
		SchemaVersion: SchemaVersion,
		ID:            id,
		CreatedAt:     created.UTC(),
		CompletedAt:   created.UTC(),
		Trigger:       trigger,
		Container:     ContainerRef{Name: container, PinnedTag: pin},
	}
	b, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}

	tx, err := st.Begin(ctx, container, id)
	if err != nil {
		t.Fatal(err)
	}
	w, err := tx.Create(ctx, ManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

func newStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func kept(t *testing.T, st store.Store, container string) []string {
	t.Helper()
	ids, err := st.Snapshots(context.Background(), container)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestRetentionKeepsTheMostRecentRegardlessOfAge(t *testing.T) {
	st := newStore(t)
	for _, age := range []time.Duration{100 * 24 * time.Hour, 90 * 24 * time.Hour, 80 * 24 * time.Hour} {
		writeSnapshot(t, st, "app", age, TriggerUpdate, "")
	}

	p := NewPruner(st, nil, testLogger())
	report, err := p.Prune(context.Background(), "app", Retention{Keep: 2, Days: 1, ProtectManual: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Removed != 1 {
		t.Errorf("removed %d snapshots, want 1", report.Removed)
	}
	if got := kept(t, st, "app"); len(got) != 2 {
		t.Errorf("%d snapshots left, want 2: %v", len(got), got)
	}
}

func TestRetentionKeepsYoungSnapshotsBeyondTheCount(t *testing.T) {
	st := newStore(t)
	// Five snapshots, all from today. Keep: 2 alone would delete three of them,
	// but the age rule has to save them.
	for i := range 5 {
		writeSnapshot(t, st, "app", time.Duration(i)*time.Hour, TriggerUpdate, "")
	}

	p := NewPruner(st, nil, testLogger())
	report, err := p.Prune(context.Background(), "app", Retention{Keep: 2, Days: 14, ProtectManual: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Removed != 0 {
		t.Errorf("removed %d snapshots although all are younger than the cutoff", report.Removed)
	}
}

func TestManualSnapshotsAreProtected(t *testing.T) {
	st := newStore(t)
	// Someone took this by hand before making a change; the policy that sweeps
	// routine automatic snapshots must not decide it was expendable.
	manual := writeSnapshot(t, st, "app", 365*24*time.Hour, TriggerManual, "")
	writeSnapshot(t, st, "app", 300*24*time.Hour, TriggerUpdate, "")
	writeSnapshot(t, st, "app", 1*time.Hour, TriggerUpdate, "")

	p := NewPruner(st, nil, testLogger())
	if _, err := p.Prune(context.Background(), "app", Retention{Keep: 1, Days: 1, ProtectManual: true}, false); err != nil {
		t.Fatal(err)
	}

	got := kept(t, st, "app")
	found := false
	for _, id := range got {
		if id == manual {
			found = true
		}
	}
	if !found {
		t.Errorf("the hand-taken snapshot was pruned: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("kept %v, want the manual one plus the most recent", got)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	st := newStore(t)
	for i := range 4 {
		writeSnapshot(t, st, "app", time.Duration(100+i)*24*time.Hour, TriggerUpdate, "")
	}

	p := NewPruner(st, nil, testLogger())
	report, err := p.Prune(context.Background(), "app", Retention{Keep: 1, Days: 1, ProtectManual: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Removed != 3 {
		t.Errorf("the dry run reported %d removals, want 3", report.Removed)
	}
	if got := kept(t, st, "app"); len(got) != 4 {
		t.Errorf("the dry run deleted something: %d snapshots left, want 4", len(got))
	}
}

func TestPruneReportsTheImagePinsItWouldRelease(t *testing.T) {
	// Every update pins the image it replaced. Without releasing them the image
	// store grows by one tagged image per update, forever, and `docker image
	// prune` cannot help because every one of them is tagged.
	st := newStore(t)
	writeSnapshot(t, st, "app", 200*24*time.Hour, TriggerUpdate, "backup-tower/keep:app-old")
	writeSnapshot(t, st, "app", 1*time.Hour, TriggerUpdate, "backup-tower/keep:app-new")

	p := NewPruner(st, nil, testLogger())
	report, err := p.Prune(context.Background(), "app", Retention{Keep: 1, Days: 1, ProtectManual: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Released != 1 {
		t.Errorf("report.Released = %d, want 1", report.Released)
	}
}

func TestRetentionThatWouldDeleteEverythingIsRejected(t *testing.T) {
	if err := (Retention{Keep: 0, Days: 0}).Valid(); err == nil {
		t.Error("a retention of zero snapshots and zero days was accepted")
	}
	if err := (Retention{Keep: -1, Days: 5}).Valid(); err == nil {
		t.Error("a negative count was accepted")
	}
	if err := (Retention{Keep: 3, Days: 14}).Valid(); err != nil {
		t.Errorf("a sensible policy was rejected: %v", err)
	}
}

func TestUnreadableSnapshotsAreNeverDeleted(t *testing.T) {
	st := newStore(t)
	writeSnapshot(t, st, "app", 1*time.Hour, TriggerUpdate, "")

	// A directory with no manifest at all: something is wrong with it, and
	// guessing is not worth the disk space.
	ctx := context.Background()
	tx, err := st.Begin(ctx, "app", "2020-01-01T00-00-00Z")
	if err != nil {
		t.Fatal(err)
	}
	w, _ := tx.Create(ctx, "volumes/data.tar.zst")
	io.WriteString(w, "orphan")
	w.Close()
	tx.Commit(ctx)

	p := NewPruner(st, nil, testLogger())
	if _, err := p.Prune(ctx, "app", Retention{Keep: 1, Days: 1, ProtectManual: true}, false); err != nil {
		t.Fatal(err)
	}
	got := kept(t, st, "app")
	if len(got) != 2 {
		t.Errorf("the snapshot with an unreadable manifest was deleted: %v", got)
	}
}

// testLogger keeps prune output out of the test log; failures are asserted on
// the report, not on what was printed.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
