package snapshot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/Hudint/backup-tower/internal/runtime"
	"github.com/Hudint/backup-tower/internal/snapshot/archive"
	"github.com/Hudint/backup-tower/internal/snapshot/source"
	"github.com/Hudint/backup-tower/internal/snapshot/store"
)

// StopPolicy decides whether the container is stopped while its data is read.
type StopPolicy string

const (
	// StopAuto stops for update snapshots and reads hot otherwise. During an
	// update the container is going down anyway, so a consistent cold copy is
	// free; a scheduled backup would have to create downtime to get one, which
	// is not a decision to make on the operator's behalf.
	StopAuto StopPolicy = "auto"
	// StopAlways always stops first.
	StopAlways StopPolicy = "always"
	// StopNever never stops, accepting a crash-consistent copy.
	StopNever StopPolicy = "never"
)

// Options configures a single snapshot run.
type Options struct {
	Trigger Trigger
	Stop    StopPolicy
	// IncludeBinds archives bind-mounted host paths too. Off by default: bind
	// mounts are frequently large media directories that have no business in a
	// per-update snapshot.
	IncludeBinds bool
	// Level is the zstd compression level; zero uses the default.
	Level int
	// StopTimeout is passed to the engine when stopping; nil uses the
	// container's own configuration.
	StopTimeout *int
	// NoPin disables tagging the image so it survives pruning. Only useful when
	// the snapshot is explicitly not meant to be a rollback point.
	NoPin bool
	// LeaveStopped keeps a container that was stopped for the snapshot down
	// afterwards. An update is about to replace it anyway, and starting it back
	// up only to stop it again would double the downtime for nothing.
	LeaveStopped bool
}

// Taker produces snapshots.
type Taker struct {
	rt    *runtime.Client
	store store.Store
	src   *source.Accessor
	tool  string
	log   *slog.Logger
}

// NewTaker wires up a snapshot producer.
func NewTaker(rt *runtime.Client, st store.Store, src *source.Accessor, tool string, log *slog.Logger) *Taker {
	if log == nil {
		log = slog.Default()
	}
	return &Taker{rt: rt, store: st, src: src, tool: tool, log: log}
}

// Take snapshots a container. It returns the manifest of the published
// snapshot; nothing becomes visible in the store unless the run completed.
func (t *Taker) Take(ctx context.Context, ref string, opts Options) (*Manifest, error) {
	started := time.Now()

	c, err := t.rt.Inspect(ctx, ref)
	if err != nil {
		return nil, err
	}

	// Capture the configuration before anything is stopped, so the recorded
	// spec describes the container as it normally runs.
	spec := NewSpec(c, t.tool, started)

	m := &Manifest{
		SchemaVersion: SchemaVersion,
		ID:            NewID(started),
		CreatedAt:     started.UTC(),
		Tool:          t.tool,
		Trigger:       opts.Trigger,
		WasRunning:    c.Running,
		Container: ContainerRef{
			Name:           c.Name,
			ID:             c.ID,
			Image:          c.Image,
			ImageID:        c.ImageID,
			ComposeProject: c.ComposeProject(),
			ComposeService: c.ComposeService(),
		},
		Engine: EngineRef{
			Flavor:     string(t.rt.Flavor()),
			Version:    t.rt.ServerVersion(),
			APIVersion: t.rt.APIVersion(),
		},
	}

	if img, err := t.rt.InspectImage(ctx, c.ImageID); err == nil {
		m.Container.ImageDigests = img.RepoDigests
		if !img.FromRegistry() {
			m.Warnings = append(m.Warnings, "image has no registry digest; it was built locally and cannot be re-pulled")
		}
	} else {
		m.Warnings = append(m.Warnings, fmt.Sprintf("could not inspect image %s: %v", c.ImageID, err))
	}

	mounts, warnings := t.selectMounts(c, opts)
	m.Warnings = append(m.Warnings, warnings...)
	// Only say a container has nothing to archive when that is actually true.
	// A container whose storage was deliberately left out has already been
	// reported above, and repeating it as "no storage" would be misleading.
	if len(mounts) == 0 && !hasDurableStorage(c) {
		m.Warnings = append(m.Warnings, "container has no storage to archive; only its configuration was captured")
	}

	// Decide and apply quiescing.
	stopWanted := shouldStop(opts, c.Running)
	m.Quiesce = QuiesceHot
	if stopWanted {
		m.Quiesce = QuiesceStopped
	}

	restart, err := t.quiesce(ctx, c, stopWanted && !opts.LeaveStopped)
	if stopWanted && opts.LeaveStopped {
		// Stop without arranging a restart: the caller is about to replace this
		// container and will bring the replacement up itself.
		t.log.Info("stopping container for a consistent snapshot", "container", c.Name)
		if err := t.rt.Stop(ctx, c.ID, opts.StopTimeout); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	// The container must come back up whatever happens below. A failed snapshot
	// that leaves a service down is a worse outcome than no snapshot at all.
	defer func() {
		if restart == nil {
			return
		}
		if err := restart(); err != nil {
			t.log.Error("could not restart container after snapshot",
				"container", c.Name, "error", err)
		}
	}()

	tx, err := t.store.Begin(ctx, c.Name, m.ID)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			if err := tx.Abort(); err != nil {
				t.log.Error("could not discard partial snapshot",
					"container", c.Name, "snapshot", m.ID, "error", err)
			}
		}
	}()

	if err := writeFile(ctx, tx, SpecFile, func(w io.Writer) error {
		b, err := spec.Encode()
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}); err != nil {
		return nil, err
	}

	for _, mnt := range mounts {
		entry, err := t.archiveMount(ctx, tx, mnt, opts)
		if err != nil {
			return nil, err
		}
		m.Archives = append(m.Archives, entry)
		t.log.Info("archived mount",
			"container", c.Name, "mount", entry.Name, "kind", entry.Kind,
			"method", entry.Method, "bytes", entry.ArchiveBytes)
	}

	// Pin the image before the snapshot is published. `docker image prune`
	// removes untagged images, and the image a container runs becomes untagged
	// the moment an update replaces it — so without this the rollback path works
	// right up until someone tidies up.
	if !opts.NoPin && c.ImageID != "" {
		tag := runtime.KeepTag(c.Name, m.ID)
		if err := t.rt.PinImage(ctx, c.ImageID, tag); err != nil {
			m.Warnings = append(m.Warnings, fmt.Sprintf("could not pin the image against pruning: %v", err))
		} else {
			m.Container.PinnedTag = tag
		}
	}

	m.CompletedAt = time.Now().UTC()
	if err := writeFile(ctx, tx, ManifestFile, func(w io.Writer) error {
		b, err := m.Encode()
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	committed = true

	t.log.Info("snapshot complete",
		"container", c.Name, "snapshot", m.ID, "trigger", m.Trigger,
		"quiesce", m.Quiesce, "archives", len(m.Archives),
		"bytes", m.ArchiveBytes(), "took", m.Duration().Round(time.Millisecond))
	return m, nil
}

// selectMounts works out what is worth archiving and explains what it left out.
func (t *Taker) selectMounts(c *runtime.Container, opts Options) ([]runtime.Mount, []string) {
	var mounts []runtime.Mount
	var warnings []string

	mounts = append(mounts, c.VolumeMounts()...)

	binds := c.BindMounts()
	switch {
	case opts.IncludeBinds:
		mounts = append(mounts, binds...)
	case len(binds) > 0:
		for _, b := range binds {
			warnings = append(warnings, fmt.Sprintf("bind mount %s (%s) was not archived; enable binds to include it", b.Destination, b.Source))
		}
	}

	// tmpfs is memory-backed and has nothing durable to save, so it is skipped
	// without comment. Anything else is unexpected enough to say out loud.
	for _, m := range c.Mounts {
		if m.Type == runtime.MountOther {
			warnings = append(warnings, fmt.Sprintf("mount at %s has an unsupported type and was not archived", m.Destination))
		}
	}
	return mounts, warnings
}

// hasDurableStorage reports whether the container has anything worth archiving
// at all, regardless of whether it was selected.
func hasDurableStorage(c *runtime.Container) bool {
	for _, m := range c.Mounts {
		if m.Type == runtime.MountVolume || m.Type == runtime.MountBind {
			return true
		}
	}
	return false
}

// shouldStop resolves the stop policy against the trigger.
func shouldStop(opts Options, running bool) bool {
	if !running {
		return false // nothing to stop
	}
	switch opts.Stop {
	case StopAlways:
		return true
	case StopNever:
		return false
	default:
		return opts.Trigger == TriggerUpdate
	}
}

// quiesce stops the container when required and returns the function that puts
// it back, or nil when nothing was stopped.
func (t *Taker) quiesce(ctx context.Context, c *runtime.Container, stop bool) (func() error, error) {
	if !stop {
		return nil, nil
	}
	t.log.Info("stopping container for a consistent snapshot", "container", c.Name)
	if err := t.rt.Stop(ctx, c.ID, nil); err != nil {
		return nil, err
	}
	return func() error {
		// Use a context detached from the caller's: cancelling a snapshot must
		// still bring the service back up.
		restartCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		t.log.Info("restarting container after snapshot", "container", c.Name)
		return t.rt.Start(restartCtx, c.ID)
	}, nil
}

func (t *Taker) archiveMount(ctx context.Context, tx store.Tx, m runtime.Mount, opts Options) (Archive, error) {
	kind := KindVolume
	name := m.Name
	if m.Type == runtime.MountBind {
		kind = KindBind
		name = m.Destination
	}

	path := ArchivePath(kind, name)
	w, err := tx.Create(ctx, path)
	if err != nil {
		return Archive{}, err
	}

	stats, method, archiveErr := t.src.Archive(ctx, m, w, archive.CreateOptions{Level: opts.Level})
	closeErr := w.Close()
	if archiveErr != nil {
		return Archive{}, fmt.Errorf("archive %s: %w", name, archiveErr)
	}
	if closeErr != nil {
		return Archive{}, fmt.Errorf("finish archive %s: %w", name, closeErr)
	}

	return Archive{
		Kind:        kind,
		Name:        name,
		Source:      m.Source,
		Destination: m.Destination,
		Path:        path,
		Method:      string(method),
		Stats:       stats,
	}, nil
}

func writeFile(ctx context.Context, tx store.Tx, name string, fn func(io.Writer) error) error {
	w, err := tx.Create(ctx, name)
	if err != nil {
		return err
	}
	if err := fn(w); err != nil {
		w.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish %s: %w", name, err)
	}
	return nil
}
