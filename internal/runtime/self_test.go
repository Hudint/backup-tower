package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func writeMountinfo(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestContainerIDIsFoundInTheMountTable(t *testing.T) {
	const id = "3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"
	path := writeMountinfo(t, `
25 30 0:24 / /sys/fs/cgroup ro,nosuid shared:9 - cgroup2 cgroup2 rw
30 20 0:26 /docker/containers/`+id+`/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/vda3 rw
`)
	if got := idFromMountinfo(path); got != id {
		t.Errorf("idFromMountinfo = %q, want the container id", got)
	}
}

func TestHexStringsOutsideEnginePathsAreIgnored(t *testing.T) {
	// A bind-mounted directory can easily contain a 64-character hex name — a
	// git object store, a content-addressed cache. Picking one of those up would
	// send us inspecting a container that does not exist.
	path := writeMountinfo(t, `
30 20 0:26 / /data/objects/aaaaaaaabbbbbbbbccccccccddddddddeeeeeeeeffffffff0000000011111111 rw - ext4 /dev/vda3 rw
`)
	if got := idFromMountinfo(path); got != "" {
		t.Errorf("idFromMountinfo = %q, want empty for a path outside the engine's storage", got)
	}
}

func TestMissingMountinfoIsNotAnError(t *testing.T) {
	// Running as a plain binary on the host is the normal case; there is simply
	// nothing to find.
	if got := idFromMountinfo(filepath.Join(t.TempDir(), "absent")); got != "" {
		t.Errorf("idFromMountinfo = %q, want empty", got)
	}
}
