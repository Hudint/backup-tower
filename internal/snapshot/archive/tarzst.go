// Package archive turns a directory into a tar+zstd stream and back again.
//
// The output is a plain tar stream compressed with zstd, deliberately nothing
// more: if backup-tower is gone, broken or incompatible, `tar --zstd -xf` still
// gets the data out. That property is worth more than any clever container
// format, so nothing here may depend on tool-specific metadata.
package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/klauspost/compress/zstd"
)

// DefaultLevel is a deliberate compromise. During an update the container is
// stopped while this runs, so compression time is downtime — level 3 buys most
// of the ratio at a fraction of the cost of the higher levels.
const DefaultLevel = 3

// Stats describes what an archive run produced.
type Stats struct {
	Files    int64 `json:"files"`
	Dirs     int64 `json:"dirs"`
	Symlinks int64 `json:"symlinks"`
	// Skipped counts entries that cannot be represented in a tar archive,
	// such as sockets. They are reported rather than silently dropped.
	Skipped []string `json:"skipped,omitempty"`
	// SourceBytes is the summed size of the archived file contents.
	SourceBytes int64 `json:"source_bytes"`
	// ArchiveBytes is the size of the compressed output.
	ArchiveBytes int64 `json:"archive_bytes"`
	// SHA256 is the checksum of the compressed output, so a restore can prove
	// the archive is intact before trusting it.
	SHA256 string `json:"sha256"`
}

// CreateOptions configures an archive run.
type CreateOptions struct {
	// Level is the zstd compression level; zero means DefaultLevel.
	Level int
	// Exclude holds paths relative to the source root that are skipped.
	Exclude []string
}

// Create walks src and writes a tar+zstd stream to dst, returning what it saw.
// Paths in the archive are relative to src, so extraction into any directory
// reproduces the original tree.
func Create(ctx context.Context, src string, dst io.Writer, opts CreateOptions) (Stats, error) {
	level := opts.Level
	if level == 0 {
		level = DefaultLevel
	}

	meter := NewMeter(dst)

	enc, err := zstd.NewWriter(meter, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)))
	if err != nil {
		return Stats{}, fmt.Errorf("init zstd encoder: %w", err)
	}
	tw := tar.NewWriter(enc)

	stats, walkErr := writeTree(ctx, src, tw, opts.Exclude)

	// Close in order and report the first failure: a swallowed close error
	// means a truncated archive that looks successful.
	if err := tw.Close(); err != nil && walkErr == nil {
		walkErr = fmt.Errorf("close tar stream: %w", err)
	}
	if err := enc.Close(); err != nil && walkErr == nil {
		walkErr = fmt.Errorf("close zstd stream: %w", err)
	}
	if walkErr != nil {
		return stats, walkErr
	}

	stats.ArchiveBytes = meter.Bytes()
	stats.SHA256 = meter.SHA256()
	return stats, nil
}

func writeTree(ctx context.Context, src string, tw *tar.Writer, exclude []string) (Stats, error) {
	var stats Stats

	root, err := filepath.Abs(src)
	if err != nil {
		return stats, fmt.Errorf("resolve source %q: %w", src, err)
	}
	if _, err := os.Stat(root); err != nil {
		return stats, fmt.Errorf("read source %q: %w", src, err)
	}

	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excluded[filepath.Clean(e)] = true
	}

	// Hardlinked files must be stored once and referenced afterwards, otherwise
	// a restore silently multiplies disk usage and breaks link identity.
	seenInodes := make(map[inodeKey]string)

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %q: %w", path, err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativise %q: %w", path, err)
		}
		if excluded[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			// A file that vanished mid-walk is normal on a hot snapshot.
			if errors.Is(err, os.ErrNotExist) {
				stats.Skipped = append(stats.Skipped, rel+" (vanished)")
				return nil
			}
			return fmt.Errorf("stat %q: %w", path, err)
		}

		switch {
		case info.Mode()&os.ModeSocket != 0:
			// Sockets have no on-disk content and cannot be represented in tar.
			stats.Skipped = append(stats.Skipped, rel+" (socket)")
			return nil
		case info.Mode()&os.ModeIrregular != 0:
			stats.Skipped = append(stats.Skipped, rel+" (irregular)")
			return nil
		}

		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return fmt.Errorf("read symlink %q: %w", path, err)
			}
		}

		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return fmt.Errorf("build tar header for %q: %w", path, err)
		}
		// The root itself is stored as "./", the same convention `tar -C dir .`
		// uses. Without it the directory's own mode and ownership are lost, and
		// some applications refuse to start when they come back wrong —
		// PostgreSQL, for one, insists its data directory is not group- or
		// world-readable.
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		applyOwnership(hdr, info)

		// Detect a second reference to an already-archived inode.
		if info.Mode().IsRegular() {
			if key, ok := inodeOf(info); ok && key.links > 1 {
				if first, seen := seenInodes[key]; seen {
					hdr.Typeflag = tar.TypeLink
					hdr.Linkname = first
					hdr.Size = 0
					if err := tw.WriteHeader(hdr); err != nil {
						return fmt.Errorf("write hardlink header for %q: %w", path, err)
					}
					stats.Files++
					return nil
				}
				seenInodes[key] = hdr.Name
			}
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header for %q: %w", path, err)
		}

		switch {
		case d.IsDir():
			stats.Dirs++
		case info.Mode()&os.ModeSymlink != 0:
			stats.Symlinks++
		case info.Mode().IsRegular():
			n, err := copyExactly(tw, path, hdr.Size)
			if err != nil {
				return err
			}
			stats.Files++
			stats.SourceBytes += n
		default:
			// Devices and FIFOs carry no payload; the header is the whole entry.
			stats.Files++
		}
		return nil
	})

	sort.Strings(stats.Skipped)
	return stats, err
}

// copyExactly writes exactly size bytes from path into w. On a hot snapshot the
// file may grow or shrink between stat and read; tar headers are fixed-size, so
// a mismatch would corrupt the whole stream. Short files are zero-padded and
// long ones truncated, which keeps the archive valid.
func copyExactly(w io.Writer, path string, size int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Vanished after the header was written: pad the reserved space.
			return 0, padZeros(w, size)
		}
		return 0, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()

	n, err := io.CopyN(w, f, size)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("read %q: %w", path, err)
	}
	if n < size {
		return n, padZeros(w, size-n)
	}
	return n, nil
}

func padZeros(w io.Writer, n int64) error {
	if n <= 0 {
		return nil
	}
	if _, err := io.CopyN(w, zeroReader{}, n); err != nil {
		return fmt.Errorf("pad archive entry: %w", err)
	}
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// ExtractOptions configures extraction.
type ExtractOptions struct {
	// Chown restores the recorded uid/gid. Requires privileges; when false the
	// extracting user owns everything.
	Chown bool
}

// CleanDir removes everything inside dir without removing dir itself.
//
// A restore has to replace, not merge. Files written after the snapshot would
// otherwise survive it and leave the application looking at a state that never
// existed. The directory itself must stay because it is the volume's mount
// point — removing it would detach the volume from the container.
func CleanDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %q: %w", dir, err)
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %q: %w", path, err)
		}
	}
	return nil
}

// Extract unpacks a tar+zstd stream into dst, which must already exist.
func Extract(ctx context.Context, r io.Reader, dst string, opts ExtractOptions) error {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return fmt.Errorf("init zstd decoder: %w", err)
	}
	defer dec.Close()

	root, err := filepath.Abs(dst)
	if err != nil {
		return fmt.Errorf("resolve destination %q: %w", dst, err)
	}

	tr := tar.NewReader(dec)
	// Directory times must be applied after their contents are written, or
	// writing a child resets the parent's mtime.
	type dirTime struct {
		path string
		hdr  *tar.Header
	}
	var dirs []dirTime

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar stream: %w", err)
		}

		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode).Perm()); err != nil {
				return fmt.Errorf("create directory %q: %w", target, err)
			}
			dirs = append(dirs, dirTime{path: target, hdr: hdr})
		case tar.TypeReg:
			if err := writeFile(tr, target, hdr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create parent of %q: %w", target, err)
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("create symlink %q: %w", target, err)
			}
		case tar.TypeLink:
			source, err := safeJoin(root, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create parent of %q: %w", target, err)
			}
			_ = os.Remove(target)
			if err := os.Link(source, target); err != nil {
				return fmt.Errorf("create hardlink %q: %w", target, err)
			}
		default:
			// Devices and FIFOs need privileges we may not have; skipping them
			// is preferable to failing the whole restore.
			continue
		}

		if opts.Chown && hdr.Typeflag != tar.TypeSymlink {
			if err := os.Chown(target, hdr.Uid, hdr.Gid); err != nil {
				return fmt.Errorf("chown %q: %w", target, err)
			}
		}
		if hdr.Typeflag == tar.TypeReg {
			if err := restoreTimes(target, hdr); err != nil {
				return err
			}
		}
	}

	// Apply directory metadata from the deepest path upwards.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		if err := os.Chmod(d.path, os.FileMode(d.hdr.Mode).Perm()); err != nil {
			return fmt.Errorf("chmod %q: %w", d.path, err)
		}
		if err := restoreTimes(d.path, d.hdr); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(r io.Reader, target string, hdr *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create parent of %q: %w", target, err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode).Perm())
	if err != nil {
		return fmt.Errorf("create %q: %w", target, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("write %q: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %q: %w", target, err)
	}
	return nil
}

// safeJoin rejects archive entries that would escape the destination, which is
// how a tampered or malformed archive would otherwise overwrite host files.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}
	target := filepath.Join(root, clean)
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}
	return target, nil
}

type inodeKey struct {
	dev   uint64
	ino   uint64
	links uint64
}

func inodeOf(info os.FileInfo) (inodeKey, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return inodeKey{}, false
	}
	return inodeKey{dev: uint64(st.Dev), ino: st.Ino, links: uint64(st.Nlink)}, true
}

func applyOwnership(hdr *tar.Header, info os.FileInfo) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	hdr.Uid = int(st.Uid)
	hdr.Gid = int(st.Gid)
}

// restoreTimes puts back the recorded modification time. Access times are
// restored alongside it because tar carries both, but only mtime is relied on.
func restoreTimes(path string, hdr *tar.Header) error {
	mtime := hdr.ModTime
	if mtime.IsZero() {
		return nil
	}
	atime := hdr.AccessTime
	if atime.IsZero() {
		atime = mtime
	}
	if err := os.Chtimes(path, atime, mtime); err != nil {
		return fmt.Errorf("restore timestamps on %q: %w", path, err)
	}
	return nil
}
