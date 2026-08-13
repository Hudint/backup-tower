package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pendingPrefix marks a snapshot directory that is still being written. Listing
// skips these, so an interrupted run never presents itself as restorable.
const pendingPrefix = ".pending-"

// FS stores snapshots in a directory tree: <root>/<container>/<snapshot-id>/.
type FS struct {
	root string
}

// NewFS opens a filesystem-backed store rooted at dir, creating it if needed.
func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, errors.New("backup directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve backup directory %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("create backup directory %q: %w", abs, err)
	}
	return &FS{root: abs}, nil
}

// Root reports the directory the store writes to.
func (f *FS) Root() string { return f.root }

func (f *FS) Close() error { return nil }

func (f *FS) Location(container, id string) string {
	return filepath.Join(f.root, container, id)
}

func (f *FS) Begin(ctx context.Context, container, id string) (Tx, error) {
	if err := validName(container); err != nil {
		return nil, fmt.Errorf("container name: %w", err)
	}
	if err := validName(id); err != nil {
		return nil, fmt.Errorf("snapshot id: %w", err)
	}

	final := filepath.Join(f.root, container, id)
	if _, err := os.Stat(final); err == nil {
		return nil, fmt.Errorf("snapshot %s/%s already exists", container, id)
	}

	pending := filepath.Join(f.root, container, pendingPrefix+id)
	if err := os.RemoveAll(pending); err != nil {
		return nil, fmt.Errorf("clear stale pending snapshot %q: %w", pending, err)
	}
	if err := os.MkdirAll(pending, 0o750); err != nil {
		return nil, fmt.Errorf("create snapshot directory %q: %w", pending, err)
	}
	return &fsTx{pending: pending, final: final}, nil
}

func (f *FS) Containers(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(f.root)
	if err != nil {
		return nil, fmt.Errorf("read backup directory %q: %w", f.root, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *FS) Snapshots(ctx context.Context, container string) ([]string, error) {
	dir := filepath.Join(f.root, container)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshots of %q: %w", container, err)
	}
	var out []string
	for _, e := range entries {
		// Skip pending and any other dot-prefixed bookkeeping.
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	// Snapshot IDs are timestamps in a sortable layout, so lexical order is
	// chronological order.
	sort.Strings(out)
	return out, nil
}

func (f *FS) Open(ctx context.Context, container, id, name string) (io.ReadCloser, error) {
	path, err := f.resolve(container, id, name)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s not found in snapshot %s/%s", name, container, id)
		}
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	return file, nil
}

func (f *FS) Remove(ctx context.Context, container, id string) error {
	if err := validName(container); err != nil {
		return fmt.Errorf("container name: %w", err)
	}
	if err := validName(id); err != nil {
		return fmt.Errorf("snapshot id: %w", err)
	}
	dir := filepath.Join(f.root, container, id)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove snapshot %q: %w", dir, err)
	}
	// Drop the container directory once its last snapshot is gone, so listings
	// do not accumulate empty entries.
	parent := filepath.Join(f.root, container)
	if entries, err := os.ReadDir(parent); err == nil && len(entries) == 0 {
		_ = os.Remove(parent)
	}
	return nil
}

func (f *FS) resolve(container, id, name string) (string, error) {
	if err := validName(container); err != nil {
		return "", fmt.Errorf("container name: %w", err)
	}
	if err := validName(id); err != nil {
		return "", fmt.Errorf("snapshot id: %w", err)
	}
	rel, err := validRelPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(f.root, container, id, rel), nil
}

type fsTx struct {
	pending   string
	final     string
	committed bool
}

func (t *fsTx) Create(ctx context.Context, name string) (io.WriteCloser, error) {
	rel, err := validRelPath(name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(t.pending, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create directory for %q: %w", name, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("create %q: %w", path, err)
	}
	return file, nil
}

func (t *fsTx) Commit(ctx context.Context) error {
	if t.committed {
		return nil
	}
	// Flush the directory so the rename cannot outrun the data on a crash.
	if err := syncDir(t.pending); err != nil {
		return err
	}
	if err := os.Rename(t.pending, t.final); err != nil {
		return fmt.Errorf("publish snapshot %q: %w", t.final, err)
	}
	t.committed = true
	_ = syncDir(filepath.Dir(t.final))
	return nil
}

func (t *fsTx) Abort() error {
	if t.committed {
		return nil
	}
	if err := os.RemoveAll(t.pending); err != nil {
		return fmt.Errorf("discard partial snapshot %q: %w", t.pending, err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %q for sync: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %q: %w", dir, err)
	}
	return nil
}

// validName rejects path components that would let a container or snapshot name
// escape the backup root.
func validName(name string) error {
	switch {
	case name == "":
		return errors.New("must not be empty")
	case name == "." || name == "..":
		return fmt.Errorf("%q is not a usable name", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("%q must not contain path separators", name)
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("%q must not start with a dot", name)
	}
	return nil
}

// validRelPath accepts forward-slash paths within a snapshot and rejects
// anything that points outside it.
func validRelPath(name string) (string, error) {
	if name == "" {
		return "", errors.New("file name must not be empty")
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q points outside the snapshot", name)
	}
	return clean, nil
}
