package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Hudint/backup-tower/internal/runtime"
	"github.com/Hudint/backup-tower/internal/snapshot/source"
	"github.com/Hudint/backup-tower/internal/snapshot/store"
)

// RestoreOptions selects what is put back.
type RestoreOptions struct {
	// Data restores the archived volume contents.
	Data bool
	// Binds includes bind-mounted host paths in the data restore. Kept separate
	// because those paths belong to the host, not to the container, and
	// overwriting them can reach well beyond the application being restored.
	Binds bool
	// Config recreates the container from the captured spec.
	Config bool
	// Image runs the recreated container on the image recorded in the snapshot
	// instead of whatever the container uses now. This is what makes a rollback
	// a rollback.
	Image bool
	// Chown restores recorded ownership.
	Chown bool
	// Start overrides whether the container is left running; nil follows what
	// the manifest recorded.
	Start *bool
	// SkipVerify skips the checksum check. Only for a desperate restore from a
	// partially damaged backup, where something is better than nothing.
	SkipVerify bool
}

// RestoredMount reports one restored archive.
type RestoredMount struct {
	Name        string
	Kind        ArchiveKind
	Destination string
	Method      source.Method
	Bytes       int64
}

// RestoreReport describes what a restore did.
type RestoreReport struct {
	Snapshot     string
	Container    string
	Mounts       []RestoredMount
	Recreated    bool
	Image        string
	Started      bool
	Warnings     []string
	Duration     time.Duration
	SkippedBinds []string
}

// Restorer puts snapshots back.
type Restorer struct {
	rt    *runtime.Client
	store store.Store
	src   *source.Accessor
	log   *slog.Logger
}

// NewRestorer wires up a restorer.
func NewRestorer(rt *runtime.Client, st store.Store, src *source.Accessor, log *slog.Logger) *Restorer {
	if log == nil {
		log = slog.Default()
	}
	return &Restorer{rt: rt, store: st, src: src, log: log}
}

// Plan describes what a restore would do, without doing it.
type Plan struct {
	Manifest *Manifest
	// Archives that would be written back.
	Archives []Archive
	// SkippedBinds lists bind archives left out because they were not requested.
	SkippedBinds []string
	// Container is the current container, nil when it no longer exists.
	Container *runtime.Container
	// Image is the reference the recreated container would run.
	Image string
	// Recreate reports whether the container would be replaced.
	Recreate bool
	// WillStart reports whether it would be running afterwards.
	WillStart bool
}

// PlanRestore works out what a restore would do. Every destructive command shows
// this first: replacing live data is not something to discover after the fact.
func (r *Restorer) PlanRestore(ctx context.Context, container, id string, opts RestoreOptions) (*Plan, error) {
	m, err := LoadManifest(ctx, r.store, container, id)
	if err != nil {
		return nil, err
	}

	p := &Plan{Manifest: m, Recreate: opts.Config || opts.Image}

	if opts.Data {
		for _, a := range m.Archives {
			if a.Kind == KindBind && !opts.Binds {
				p.SkippedBinds = append(p.SkippedBinds, a.Destination)
				continue
			}
			p.Archives = append(p.Archives, a)
		}
	}

	current, err := r.rt.Inspect(ctx, container)
	switch {
	case err == nil:
		p.Container = current
	case errors.Is(err, runtime.ErrNotFound):
		// A missing container is a normal starting point for a restore; it is
		// exactly the situation the captured spec exists for.
		if !p.Recreate {
			return nil, fmt.Errorf("container %q no longer exists; restore its configuration as well to bring it back", container)
		}
	default:
		return nil, err
	}

	if p.Recreate {
		p.Image, err = r.resolveImage(ctx, m, current, opts)
		if err != nil {
			return nil, err
		}
	}

	p.WillStart = m.WasRunning
	if opts.Start != nil {
		p.WillStart = *opts.Start
	}
	return p, nil
}

// resolveImage decides which image the recreated container will run.
func (r *Restorer) resolveImage(ctx context.Context, m *Manifest, current *runtime.Container, opts RestoreOptions) (string, error) {
	if !opts.Image {
		// Keep whatever the container runs now; only its configuration changes.
		if current != nil {
			return current.Image, nil
		}
		return "", fmt.Errorf("container no longer exists, so its current image is unknown; restore the recorded image as well")
	}

	// A registry digest comes first because it is both exact and named: the
	// container stays identifiable afterwards, and a later update check can say
	// "pinned to a digest" rather than "this image has no name".
	//
	// The plain tag is deliberately absent. It is the reference we are rolling
	// back *from* — resolving it now would hand back the very image that just
	// failed, which is the opposite of a rollback.
	var candidates []string
	candidates = append(candidates, m.Container.ImageDigests...)
	candidates = append(candidates, m.Container.PinnedTag)
	candidates = append(candidates, runtime.KeepTag(m.Container.Name, m.ID))
	candidates = append(candidates, m.Container.ImageID)

	ref, err := r.rt.ResolveImage(ctx, candidates...)
	if err != nil {
		return "", fmt.Errorf("cannot roll back to the image recorded in this snapshot: %w."+
			" The tag %q is not used as a fallback because it now points at the image being rolled back from",
			err, m.Container.Image)
	}
	return ref, nil
}

// Restore executes a plan.
func (r *Restorer) Restore(ctx context.Context, container, id string, opts RestoreOptions) (*RestoreReport, error) {
	started := time.Now()

	p, err := r.PlanRestore(ctx, container, id, opts)
	if err != nil {
		return nil, err
	}

	if !opts.SkipVerify && len(p.Archives) > 0 {
		if err := r.verifyArchives(ctx, container, id, p.Archives); err != nil {
			return nil, err
		}
	}

	report := &RestoreReport{
		Snapshot:     id,
		Container:    container,
		SkippedBinds: p.SkippedBinds,
		Image:        p.Image,
	}

	// Stop first. Writing into a volume underneath a running process produces a
	// state neither the old nor the new data ever had.
	if p.Container != nil && p.Container.Running {
		r.log.Info("stopping container before restore", "container", container)
		if err := r.rt.Stop(ctx, p.Container.ID, nil); err != nil {
			return nil, err
		}
	}

	for _, a := range p.Archives {
		restored, err := r.restoreArchive(ctx, container, id, a, opts)
		if err != nil {
			return report, err
		}
		report.Mounts = append(report.Mounts, restored)
		r.log.Info("restored archive",
			"container", container, "mount", a.Name, "kind", a.Kind, "method", restored.Method)
	}

	if p.Recreate {
		warnings, err := r.recreate(ctx, container, id, p)
		report.Warnings = append(report.Warnings, warnings...)
		if err != nil {
			return report, err
		}
		report.Recreated = true
	}

	if p.WillStart {
		target, err := r.rt.Inspect(ctx, container)
		if err != nil {
			return report, err
		}
		if !target.Running {
			r.log.Info("starting container after restore", "container", container)
			if err := r.rt.Start(ctx, target.ID); err != nil {
				return report, err
			}
		}
		report.Started = true
	}

	report.Duration = time.Since(started)
	return report, nil
}

func (r *Restorer) verifyArchives(ctx context.Context, container, id string, archives []Archive) error {
	_, results, err := Verify(ctx, r.store, container, id)
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(archives))
	for _, a := range archives {
		wanted[a.Path] = true
	}
	for _, res := range results {
		if !wanted[res.Archive.Path] {
			continue
		}
		if res.Err != nil {
			return fmt.Errorf("archive %s cannot be read: %w", res.Archive.Name, res.Err)
		}
		if !res.OK {
			return fmt.Errorf("archive %s does not match its recorded checksum; refusing to restore corrupted data", res.Archive.Name)
		}
	}
	return nil
}

func (r *Restorer) restoreArchive(ctx context.Context, container, id string, a Archive, opts RestoreOptions) (RestoredMount, error) {
	target, err := r.resolveTarget(ctx, a)
	if err != nil {
		return RestoredMount{}, err
	}

	rc, err := r.store.Open(ctx, container, id, a.Path)
	if err != nil {
		return RestoredMount{}, err
	}
	defer rc.Close()

	method, err := r.src.Restore(ctx, target, rc, source.RestoreOptions{
		Clean: true,
		Chown: opts.Chown,
	})
	if err != nil {
		return RestoredMount{}, fmt.Errorf("restore %s: %w", a.Name, err)
	}

	return RestoredMount{
		Name:        a.Name,
		Kind:        a.Kind,
		Destination: a.Destination,
		Method:      method,
		Bytes:       a.ArchiveBytes,
	}, nil
}

// resolveTarget works out where an archive's contents belong now. Volume
// mountpoints are looked up fresh rather than taken from the manifest: the
// recorded path was true when the snapshot was taken, not necessarily today.
func (r *Restorer) resolveTarget(ctx context.Context, a Archive) (runtime.Mount, error) {
	switch a.Kind {
	case KindVolume:
		v, err := r.rt.EnsureVolume(ctx, a.Name)
		if err != nil {
			return runtime.Mount{}, err
		}
		return runtime.Mount{
			Type:        runtime.MountVolume,
			Name:        v.Name,
			Source:      v.Mountpoint,
			Destination: a.Destination,
			ReadWrite:   true,
		}, nil
	case KindBind:
		if a.Source == "" {
			return runtime.Mount{}, fmt.Errorf("bind archive %s has no recorded host path", a.Name)
		}
		return runtime.Mount{
			Type:        runtime.MountBind,
			Source:      a.Source,
			Destination: a.Destination,
			ReadWrite:   true,
		}, nil
	default:
		return runtime.Mount{}, fmt.Errorf("archive %s has unknown kind %q", a.Name, a.Kind)
	}
}

// recreate replaces the container from the captured spec.
//
// The old container is moved aside rather than deleted, and only removed once
// its replacement exists. If anything fails in between, the original is put back
// under its own name — a rollback that leaves you with no container at all would
// be a worse failure than the one it was trying to fix.
func (r *Restorer) recreate(ctx context.Context, container, id string, p *Plan) ([]string, error) {
	spec, err := LoadSpec(ctx, r.store, container, id)
	if err != nil {
		return nil, err
	}
	in, err := spec.Container()
	if err != nil {
		return nil, err
	}

	newID, warnings, err := r.rt.Replace(ctx, p.Container, in, runtime.ReplaceOptions{
		Name:       container,
		Image:      p.Image,
		ParkSuffix: id,
		Log:        r.log,
	})
	if err != nil {
		return warnings, err
	}
	r.log.Info("recreated container", "container", container, "id", newID[:12], "image", p.Image)
	return warnings, nil
}
