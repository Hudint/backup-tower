package store

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotIsInvisibleUntilCommitted(t *testing.T) {
	ctx := context.Background()
	s := newTestFS(t)

	tx, err := s.Begin(ctx, "webapp", "2026-08-13T14-22-05Z")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	write(t, ctx, tx, "manifest.json", `{"id":"x"}`)

	// A half-written snapshot must never be offered up as restorable.
	ids, err := s.Snapshots(ctx, "webapp")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("uncommitted snapshot is already listed: %v", ids)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	ids, err = s.Snapshots(ctx, "webapp")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "2026-08-13T14-22-05Z" {
		t.Fatalf("Snapshots after commit = %v", ids)
	}
}

func TestAbortLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	s := newTestFS(t)

	tx, err := s.Begin(ctx, "webapp", "2026-08-13T14-22-05Z")
	if err != nil {
		t.Fatal(err)
	}
	write(t, ctx, tx, "volumes/pgdata.tar.zst", "partial")
	if err := tx.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(s.Root(), "webapp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("abort left %d entries behind: %v", len(entries), entries)
	}
}

func TestSnapshotsAreListedChronologically(t *testing.T) {
	ctx := context.Background()
	s := newTestFS(t)

	// Written out of order on purpose.
	for _, id := range []string{"2026-08-13T14-22-05Z", "2026-08-10T04-00-11Z", "2026-08-12T09-00-00Z"} {
		tx, err := s.Begin(ctx, "webapp", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, ctx, tx, "manifest.json", "{}")
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := s.Snapshots(ctx, "webapp")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-08-10T04-00-11Z", "2026-08-12T09-00-00Z", "2026-08-13T14-22-05Z"}
	for i := range want {
		if i >= len(ids) || ids[i] != want[i] {
			t.Fatalf("Snapshots = %v, want %v", ids, want)
		}
	}
}

func TestRejectsNamesThatEscapeTheRoot(t *testing.T) {
	ctx := context.Background()
	s := newTestFS(t)

	for _, name := range []string{"..", "../evil", "web/app", ".hidden"} {
		if _, err := s.Begin(ctx, name, "2026-08-13T14-22-05Z"); err == nil {
			t.Errorf("Begin accepted container name %q", name)
		}
	}

	tx, err := s.Begin(ctx, "webapp", "2026-08-13T14-22-05Z")
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Abort()
	if _, err := tx.Create(ctx, "../../escape"); err == nil {
		t.Error("Create accepted a file name pointing outside the snapshot")
	}
}

func TestOpenReturnsStoredContent(t *testing.T) {
	ctx := context.Background()
	s := newTestFS(t)

	tx, err := s.Begin(ctx, "webapp", "2026-08-13T14-22-05Z")
	if err != nil {
		t.Fatal(err)
	}
	write(t, ctx, tx, "volumes/pgdata.tar.zst", "archive-bytes")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	r, err := s.Open(ctx, "webapp", "2026-08-13T14-22-05Z", "volumes/pgdata.tar.zst")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "archive-bytes" {
		t.Errorf("content = %q", got)
	}
}

func TestOpenResolverRejectsTraversal(t *testing.T) {
	s := newTestFS(t)
	if _, err := s.resolve("webapp", "2026-08-13T14-22-05Z", "../../../etc/passwd"); err == nil {
		t.Error("resolve accepted a traversing file name")
	}
}

func newTestFS(t *testing.T) *FS {
	t.Helper()
	s, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func write(t *testing.T, ctx context.Context, tx Tx, name, content string) {
	t.Helper()
	w, err := tx.Create(ctx, name)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
