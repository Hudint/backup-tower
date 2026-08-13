// Package snapshot captures a container's configuration and storage, and puts
// it back.
package snapshot

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Hudint/backup-tower/internal/snapshot/archive"
)

// SchemaVersion is bumped whenever the on-disk layout changes in a way that a
// reader must know about. It is recorded in every manifest so a future version
// can still read what this one wrote.
const SchemaVersion = 1

// File names inside a snapshot directory.
const (
	ManifestFile = "manifest.json"
	SpecFile     = "spec.json"
	VolumesDir   = "volumes"
	BindsDir     = "binds"
)

// Trigger records why a snapshot was taken. It drives retention: a snapshot
// taken by hand before a manual change must not be swept away by the same
// policy that prunes routine automatic ones.
type Trigger string

const (
	TriggerManual   Trigger = "manual"
	TriggerUpdate   Trigger = "update"
	TriggerSchedule Trigger = "schedule"
)

// Quiesce records whether the container was stopped while its data was read.
// A hot snapshot is only crash-consistent, and a restore needs to know that.
type Quiesce string

const (
	QuiesceStopped Quiesce = "stopped"
	QuiesceHot     Quiesce = "hot"
)

// ArchiveKind separates named volumes from bind-mounted host paths.
type ArchiveKind string

const (
	KindVolume ArchiveKind = "volume"
	KindBind   ArchiveKind = "bind"
)

// Archive describes one stored tar+zstd file.
type Archive struct {
	Kind ArchiveKind `json:"kind"`
	// Name is the volume name, or the mount destination for a bind.
	Name string `json:"name"`
	// Source is where the data was read from on the engine host.
	Source string `json:"source"`
	// Destination is the path inside the container.
	Destination string `json:"destination"`
	// Path is the archive location relative to the snapshot directory.
	Path string `json:"path"`
	// Method records how the data was reached, for diagnosing surprises.
	Method string `json:"method"`

	archive.Stats
}

// ContainerRef identifies what was snapshotted.
type ContainerRef struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Image   string `json:"image"`
	ImageID string `json:"image_id"`
	// ImageDigests are the registry digests of the image at snapshot time.
	// Empty means the image was built locally and cannot be re-pulled.
	ImageDigests   []string `json:"image_digests,omitempty"`
	ComposeProject string   `json:"compose_project,omitempty"`
	ComposeService string   `json:"compose_service,omitempty"`
}

// EngineRef records which engine produced the snapshot.
type EngineRef struct {
	Flavor     string `json:"flavor"`
	Version    string `json:"version,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
}

// Manifest is the index of a snapshot. Everything needed to judge whether a
// snapshot is usable lives here, so that question can be answered without
// unpacking a single archive.
type Manifest struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"created_at"`
	CompletedAt   time.Time `json:"completed_at"`
	Tool          string    `json:"tool"`

	Trigger Trigger `json:"trigger"`
	Quiesce Quiesce `json:"quiesce"`
	// WasRunning records the state the container was in before the snapshot.
	// A restore needs it to decide whether to leave the container running: the
	// captured spec is taken while the container is stopped for the snapshot,
	// so its own state field cannot answer that question.
	WasRunning bool `json:"was_running"`

	Container ContainerRef `json:"container"`
	Engine    EngineRef    `json:"engine"`
	Archives  []Archive    `json:"archives"`

	// Warnings collects everything that went differently than intended but did
	// not abort the run — skipped sockets, unreadable paths, a bind mount left
	// out. A snapshot that quietly omitted data would be worse than none.
	Warnings []string `json:"warnings,omitempty"`
}

// Duration reports how long the snapshot took.
func (m *Manifest) Duration() time.Duration {
	if m.CompletedAt.IsZero() {
		return 0
	}
	return m.CompletedAt.Sub(m.CreatedAt)
}

// ArchiveBytes reports the total compressed size on disk.
func (m *Manifest) ArchiveBytes() int64 {
	var total int64
	for _, a := range m.Archives {
		total += a.ArchiveBytes
	}
	return total
}

// SourceBytes reports the total uncompressed size of the archived contents.
func (m *Manifest) SourceBytes() int64 {
	var total int64
	for _, a := range m.Archives {
		total += a.SourceBytes
	}
	return total
}

// Encode renders the manifest as indented JSON. Snapshots are read by people
// during incidents, so readability beats compactness.
func (m *Manifest) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return append(b, '\n'), nil
}

// DecodeManifest parses a manifest and rejects layouts this build cannot read.
func DecodeManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if m.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("snapshot uses schema version %d, this build understands up to %d", m.SchemaVersion, SchemaVersion)
	}
	return &m, nil
}

// NewID returns the identifier for a snapshot taken at t. It doubles as the
// directory name, so it must sort chronologically and be filesystem-safe on
// every platform — hence the dashes instead of colons.
func NewID(t time.Time) string {
	return t.UTC().Format("2006-01-02T15-04-05Z")
}

// ParseID recovers the timestamp from a snapshot ID.
func ParseID(id string) (time.Time, error) {
	t, err := time.Parse("2006-01-02T15-04-05Z", id)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse snapshot id %q: %w", id, err)
	}
	return t.UTC(), nil
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// ArchivePath builds the in-snapshot path for an archive. Bind mounts are keyed
// by their container destination rather than the host path: the destination is
// what identifies the mount to the application, and it stays stable when the
// host path moves.
func ArchivePath(kind ArchiveKind, name string) string {
	dir := VolumesDir
	if kind == KindBind {
		dir = BindsDir
	}
	return path.Join(dir, sanitizeName(name)+".tar.zst")
}

func sanitizeName(name string) string {
	s := unsafeName.ReplaceAllString(strings.Trim(name, "/"), "_")
	s = strings.Trim(s, "._-")
	if s == "" {
		return "root"
	}
	// Leave room for the directory prefix and suffix within common path limits.
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
