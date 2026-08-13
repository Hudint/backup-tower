// Package source reads the data behind a container mount.
//
// There are two ways in, and which one is available depends entirely on where
// backup-tower runs. On the engine host with sufficient rights, a volume's
// mountpoint can simply be read. Inside a container it cannot: mounts cannot be
// added to a running container, so the only way to reach foreign volumes is to
// start a second container that is created with them attached.
package source

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Hudint/backup-tower/internal/runtime"
	"github.com/Hudint/backup-tower/internal/snapshot/archive"
)

// Method records how a mount's data was reached.
type Method string

const (
	// MethodDirect read the path straight off the engine host.
	MethodDirect Method = "direct"
	// MethodHelper went through a short-lived helper container.
	MethodHelper Method = "helper"
)

// HelperMount is where the helper container sees the data it archives.
const HelperMount = "/src"

// HelperRestoreMount is where the helper container writes during a restore. It
// is deliberately a different path from HelperMount so a mistake in the command
// line cannot turn a read-only archive run into a write.
const HelperRestoreMount = "/dst"

// statsPrefix marks the helper's machine-readable report on stderr. stdout
// carries the archive itself and must stay pure.
const statsPrefix = "backup-tower-stats: "

// Options configures how mounts are read.
type Options struct {
	// HelperImage is the image used for helper containers. It must contain this
	// same binary; normally it is backup-tower's own image.
	HelperImage string
	// Force pins the access method instead of detecting it. Useful for testing
	// the helper path on a host where the direct path would work.
	Force Method
}

// Accessor reads container mounts.
type Accessor struct {
	rt   *runtime.Client
	opts Options
}

// New builds an Accessor.
func New(rt *runtime.Client, opts Options) *Accessor {
	return &Accessor{rt: rt, opts: opts}
}

// Archive writes a tar+zstd stream of the mount's contents into w and reports
// what was archived and how it was reached.
func (a *Accessor) Archive(ctx context.Context, m runtime.Mount, w io.Writer, copts archive.CreateOptions) (archive.Stats, Method, error) {
	method := a.chooseMethod(m)
	switch method {
	case MethodDirect:
		stats, err := archive.Create(ctx, m.Source, w, copts)
		return stats, MethodDirect, err
	case MethodHelper:
		stats, err := a.archiveViaHelper(ctx, m, w, copts)
		return stats, MethodHelper, err
	default:
		return archive.Stats{}, method, fmt.Errorf("unknown access method %q", method)
	}
}

// chooseMethod prefers the direct path because it avoids starting a container
// per mount, and falls back to the helper whenever the data is out of reach.
func (a *Accessor) chooseMethod(m runtime.Mount) Method {
	if a.opts.Force != "" {
		return a.opts.Force
	}
	if m.Source == "" {
		return MethodHelper
	}
	f, err := os.Open(m.Source)
	if err != nil {
		return MethodHelper
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && err != io.EOF {
		return MethodHelper
	}
	return MethodDirect
}

func (a *Accessor) archiveViaHelper(ctx context.Context, m runtime.Mount, w io.Writer, copts archive.CreateOptions) (archive.Stats, error) {
	if a.opts.HelperImage == "" {
		return archive.Stats{}, fmt.Errorf("cannot reach %s directly and no helper image is configured", describe(m))
	}

	bind, err := helperBind(m, HelperMount, false)
	if err != nil {
		return archive.Stats{}, err
	}

	cmd := []string{"helper", "archive", "--source", HelperMount}
	if copts.Level != 0 {
		cmd = append(cmd, "--level", strconv.Itoa(copts.Level))
	}
	for _, ex := range copts.Exclude {
		cmd = append(cmd, "--exclude", ex)
	}

	// Measure at this end: the checksum has to cover the bytes that actually
	// arrived, not the ones the helper believes it sent.
	meter := archive.NewMeter(w)

	stderr, err := a.rt.RunHelper(ctx, runtime.HelperSpec{
		Image:   a.opts.HelperImage,
		Cmd:     cmd,
		Binds:   []string{bind},
		Purpose: "archiving " + describe(m),
	}, meter)
	if err != nil {
		return archive.Stats{}, err
	}

	stats, err := parseStats(stderr)
	if err != nil {
		return archive.Stats{}, fmt.Errorf("helper archived %s but did not report what it did: %w", describe(m), err)
	}
	stats.ArchiveBytes = meter.Bytes()
	stats.SHA256 = meter.SHA256()
	return stats, nil
}

// helperBind builds the mount spec that gives the helper sight of the data.
// While archiving it is read-only, and that is not a formality: the helper must
// be incapable of altering the very data it is supposed to preserve.
func helperBind(m runtime.Mount, mountPoint string, writable bool) (string, error) {
	mode := ":ro"
	if writable {
		mode = ":rw"
	}
	switch m.Type {
	case runtime.MountVolume:
		if m.Name == "" {
			return "", fmt.Errorf("volume mount at %s has no name", m.Destination)
		}
		return m.Name + ":" + mountPoint + mode, nil
	case runtime.MountBind:
		if m.Source == "" {
			return "", fmt.Errorf("bind mount at %s has no host path", m.Destination)
		}
		return m.Source + ":" + mountPoint + mode, nil
	default:
		return "", fmt.Errorf("mount type %q at %s cannot be archived", m.Type, m.Destination)
	}
}

// EncodeStats renders the helper's report for stderr.
func EncodeStats(s archive.Stats) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode helper stats: %w", err)
	}
	return statsPrefix + string(b), nil
}

// parseStats picks the report line out of the helper's stderr, ignoring any
// other diagnostics it may have printed.
func parseStats(stderr []byte) (archive.Stats, error) {
	scanner := bufio.NewScanner(bytes.NewReader(stderr))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var line string
	for scanner.Scan() {
		if s := strings.TrimSpace(scanner.Text()); strings.HasPrefix(s, statsPrefix) {
			line = strings.TrimPrefix(s, statsPrefix)
		}
	}
	if line == "" {
		return archive.Stats{}, fmt.Errorf("no stats line in helper output")
	}

	var stats archive.Stats
	if err := json.Unmarshal([]byte(line), &stats); err != nil {
		return archive.Stats{}, fmt.Errorf("decode helper stats: %w", err)
	}
	return stats, nil
}

func describe(m runtime.Mount) string {
	if m.Type == runtime.MountVolume {
		return "volume " + m.Name
	}
	return "bind mount " + m.Destination
}
