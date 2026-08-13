package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// buildTree creates a source tree exercising the properties a container volume
// actually has: modes that matter, nested directories, symlinks and hardlinks.
func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// A restrictive mode on the root, as PostgreSQL and similar require.
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "config.yaml"), "listen: 0.0.0.0\n", 0o640)
	if err := os.MkdirAll(filepath.Join(root, "data", "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "data", "nested", "payload.bin"), "binary-ish\x00\x01\x02", 0o600)
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../config.yaml", filepath.Join(root, "data", "link.yaml")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "linked-a"), "shared inode", 0o644)
	if err := os.Link(filepath.Join(root, "linked-a"), filepath.Join(root, "linked-b")); err != nil {
		t.Fatal(err)
	}
	return root
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile honours umask, so set the mode explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestRoundTripPreservesTree(t *testing.T) {
	ctx := context.Background()
	src := buildTree(t)

	var buf bytes.Buffer
	stats, err := Create(ctx, src, &buf, CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if stats.SHA256 == "" {
		t.Error("checksum is empty; a restore could not verify the archive")
	}
	if stats.ArchiveBytes != int64(buf.Len()) {
		t.Errorf("ArchiveBytes = %d, want %d", stats.ArchiveBytes, buf.Len())
	}
	if stats.Symlinks != 1 {
		t.Errorf("Symlinks = %d, want 1", stats.Symlinks)
	}

	dst := t.TempDir()
	if err := Extract(ctx, bytes.NewReader(buf.Bytes()), dst, ExtractOptions{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Content and mode of a regular file.
	got, err := os.ReadFile(filepath.Join(dst, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "listen: 0.0.0.0\n" {
		t.Errorf("config.yaml content = %q", got)
	}
	assertMode(t, filepath.Join(dst, "config.yaml"), 0o640)
	assertMode(t, filepath.Join(dst, "data", "nested"), 0o750)
	assertMode(t, filepath.Join(dst, "data", "nested", "payload.bin"), 0o600)

	// An empty directory must survive; losing it changes application behaviour.
	if fi, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("empty directory missing after restore: %v", err)
	}

	// The mode of the root itself must round-trip. PostgreSQL refuses to start
	// when its data directory is group-readable, so losing this would produce a
	// restore that looks complete and does not run.
	assertMode(t, dst, 0o700)

	// Symlinks must stay symlinks pointing at the same target.
	target, err := os.Readlink(filepath.Join(dst, "data", "link.yaml"))
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != "../config.yaml" {
		t.Errorf("symlink target = %q, want ../config.yaml", target)
	}

	// Hardlinks must remain a single inode, not two independent copies.
	if inode(t, filepath.Join(dst, "linked-a")) != inode(t, filepath.Join(dst, "linked-b")) {
		t.Error("hardlinked files were restored as separate inodes")
	}
}

func TestExtractRejectsPathEscape(t *testing.T) {
	for _, name := range []string{"../escape", "/etc/passwd", "data/../../escape"} {
		if _, err := safeJoin("/backups/target", name); err == nil {
			t.Errorf("safeJoin(%q) accepted an entry that escapes the destination", name)
		}
	}
	if _, err := safeJoin("/backups/target", "data/nested/file"); err != nil {
		t.Errorf("safeJoin rejected a legitimate entry: %v", err)
	}
}

func TestCreateHonoursExclude(t *testing.T) {
	ctx := context.Background()
	src := buildTree(t)

	var buf bytes.Buffer
	if _, err := Create(ctx, src, &buf, CreateOptions{Exclude: []string{"data"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	dst := t.TempDir()
	if err := Extract(ctx, bytes.NewReader(buf.Bytes()), dst, ExtractOptions{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "data")); !os.IsNotExist(err) {
		t.Errorf("excluded directory was archived anyway (err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "config.yaml")); err != nil {
		t.Errorf("exclude removed more than it should: %v", err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", filepath.Base(path), got, want)
	}
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("inode information unavailable on this platform")
	}
	return st.Ino
}
