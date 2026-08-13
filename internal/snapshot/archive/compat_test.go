package archive

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestArchiveReadableByStandardTar guards the promise the whole storage format
// rests on: if backup-tower is unavailable, broken or incompatible, the data
// must still come out with ordinary system tools. A regression here would only
// surface during a real recovery, which is the worst possible moment.
func TestArchiveReadableByStandardTar(t *testing.T) {
	tarBin, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("system tar not available")
	}

	ctx := context.Background()
	src := buildTree(t)

	var buf bytes.Buffer
	if _, err := Create(ctx, src, &buf, CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	work := t.TempDir()
	archivePath := filepath.Join(work, "volume.tar.zst")
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(work, "extracted")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(ctx, tarBin, "--zstd", "-xf", archivePath, "-C", dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "zstd") && strings.Contains(string(out), "not") {
			t.Skipf("system tar lacks zstd support: %s", out)
		}
		t.Fatalf("tar --zstd -xf failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(filepath.Join(dst, "config.yaml"))
	if err != nil {
		t.Fatalf("extracted tree is missing config.yaml: %v", err)
	}
	if string(got) != "listen: 0.0.0.0\n" {
		t.Errorf("content after system-tar extraction = %q", got)
	}

	target, err := os.Readlink(filepath.Join(dst, "data", "link.yaml"))
	if err != nil || target != "../config.yaml" {
		t.Errorf("symlink not restored by system tar: target=%q err=%v", target, err)
	}

	fi, err := os.Stat(filepath.Join(dst, "data", "nested", "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode after system-tar extraction = %o, want 600", fi.Mode().Perm())
	}
}
