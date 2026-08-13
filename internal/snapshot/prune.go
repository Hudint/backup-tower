package snapshot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/Hudint/backup-tower/internal/runtime"
	"github.com/Hudint/backup-tower/internal/snapshot/store"
)

// Retention decides which snapshots survive.
type Retention struct {
	// Keep is the number of most recent snapshots to keep regardless of age.
	Keep int
	// Days keeps everything younger than this many days regardless of count.
	Days int
	// ProtectManual keeps snapshots taken by hand out of the automatic sweep.
	// Someone who ran a snapshot before making a change by hand did so precisely
	// because they wanted it to be there afterwards; the policy that prunes
	// routine automatic snapshots has no business deciding otherwise.
	ProtectManual bool
}

// DefaultRetention returns a policy from the configured numbers.
func DefaultRetention(keep, days int) Retention {
	return Retention{Keep: keep, Days: days, ProtectManual: true}
}

// Valid rejects a policy that would delete everything.
func (r Retention) Valid() error {
	if r.Keep < 0 || r.Days < 0 {
		return fmt.Errorf("retention numbers must not be negative")
	}
	if r.Keep == 0 && r.Days == 0 {
		return fmt.Errorf("a retention of 0 snapshots and 0 days would delete every backup")
	}
	return nil
}

// PruneDecision records what was decided for one snapshot and why.
type PruneDecision struct {
	ID      string
	Trigger Trigger
	Created time.Time
	Bytes   int64
	Keep    bool
	// Reason explains the decision in the terms of the policy.
	Reason string
	// PinnedTag is the image pin to release when the snapshot goes.
	PinnedTag string
	Err       error
}

// PruneReport summarises a prune run.
type PruneReport struct {
	Container string
	Decisions []PruneDecision
	Removed   int
	Freed     int64
	Released  int
	DryRun    bool
}

// Pruner applies retention.
type Pruner struct {
	store store.Store
	rt    *runtime.Client
	log   *slog.Logger
}

// NewPruner builds a pruner. The engine client may be nil, in which case image
// pins are left alone.
func NewPruner(st store.Store, rt *runtime.Client, log *slog.Logger) *Pruner {
	if log == nil {
		log = slog.Default()
	}
	return &Pruner{store: st, rt: rt, log: log}
}

// Prune applies a retention policy to one container's snapshots.
func (p *Pruner) Prune(ctx context.Context, container string, policy Retention, dryRun bool) (*PruneReport, error) {
	if err := policy.Valid(); err != nil {
		return nil, err
	}

	ids, err := p.store.Snapshots(ctx, container)
	if err != nil {
		return nil, err
	}
	report := &PruneReport{Container: container, DryRun: dryRun}
	if len(ids) == 0 {
		return report, nil
	}

	type entry struct {
		id       string
		manifest *Manifest
		created  time.Time
	}
	entries := make([]entry, 0, len(ids))
	for _, id := range ids {
		m, err := LoadManifest(ctx, p.store, container, id)
		if err != nil {
			// An unreadable manifest is never deleted. It may be the only record
			// of something, and guessing is not worth the disk space.
			report.Decisions = append(report.Decisions, PruneDecision{
				ID: id, Keep: true, Reason: "manifest unreadable, keeping it to be safe", Err: err,
			})
			continue
		}
		created := m.CreatedAt
		if created.IsZero() {
			if t, err := ParseID(id); err == nil {
				created = t
			}
		}
		entries = append(entries, entry{id: id, manifest: m, created: created})
	}

	// Newest first, so "keep the last N" is simply the first N.
	sort.Slice(entries, func(i, j int) bool { return entries[i].created.After(entries[j].created) })

	cutoff := time.Now().AddDate(0, 0, -policy.Days)
	var kept int

	for _, e := range entries {
		d := PruneDecision{
			ID:        e.id,
			Trigger:   e.manifest.Trigger,
			Created:   e.created,
			Bytes:     e.manifest.ArchiveBytes(),
			PinnedTag: e.manifest.Container.PinnedTag,
		}

		switch {
		case policy.ProtectManual && e.manifest.Trigger == TriggerManual:
			d.Keep = true
			d.Reason = "taken by hand"
		case kept < policy.Keep:
			d.Keep = true
			d.Reason = fmt.Sprintf("among the %d most recent", policy.Keep)
			kept++
		case policy.Days > 0 && e.created.After(cutoff):
			d.Keep = true
			d.Reason = fmt.Sprintf("younger than %d days", policy.Days)
			kept++
		default:
			d.Keep = false
			d.Reason = fmt.Sprintf("older than %d days and not among the %d most recent", policy.Days, policy.Keep)
		}

		if !d.Keep && !dryRun {
			if err := p.remove(ctx, container, &d); err != nil {
				d.Err = err
				d.Keep = true
				d.Reason = "could not be removed"
			}
		}
		if !d.Keep {
			report.Removed++
			report.Freed += d.Bytes
			if d.PinnedTag != "" {
				report.Released++
			}
		}
		report.Decisions = append(report.Decisions, d)
	}

	return report, nil
}

// remove deletes a snapshot and releases the image it was holding on to.
//
// Releasing the pin is the half that is easy to forget and expensive to skip:
// every update pins the image it replaced, so without this the image store grows
// by one image per update, forever, and `docker image prune` cannot help because
// every one of them is tagged.
func (p *Pruner) remove(ctx context.Context, container string, d *PruneDecision) error {
	if err := p.store.Remove(ctx, container, d.ID); err != nil {
		return err
	}
	p.log.Info("removed snapshot", "container", container, "snapshot", d.ID, "bytes", d.Bytes)

	if d.PinnedTag == "" || p.rt == nil {
		return nil
	}
	if err := p.rt.UnpinImage(ctx, d.PinnedTag); err != nil {
		// The snapshot is already gone; a lingering pin is untidy, not harmful.
		p.log.Warn("could not release the image pin", "tag", d.PinnedTag, "error", err)
		return nil
	}
	p.log.Info("released image pin", "tag", d.PinnedTag)
	return nil
}

// OrphanedPins finds image pins no snapshot refers to any more.
//
// The pin outlives the snapshot whenever a snapshot directory is removed by
// something other than this tool — a hand-deleted directory, a moved backup
// root, a restore from elsewhere. Those pins would otherwise stay forever and
// defeat the very pruning the mechanism exists to survive.
func (p *Pruner) OrphanedPins(ctx context.Context) ([]string, error) {
	if p.rt == nil {
		return nil, nil
	}

	referenced := map[string]bool{}
	containers, err := p.store.Containers(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		ids, err := p.store.Snapshots(ctx, c)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			m, err := LoadManifest(ctx, p.store, c, id)
			if err != nil {
				// An unreadable manifest may well reference a pin. Refusing to
				// call anything orphaned here is the safe reading.
				return nil, fmt.Errorf("cannot decide which pins are orphaned while %s/%s is unreadable: %w", c, id, err)
			}
			if m.Container.PinnedTag != "" {
				referenced[m.Container.PinnedTag] = true
			}
		}
	}

	tags, err := p.rt.ListPins(ctx)
	if err != nil {
		return nil, err
	}
	var orphans []string
	for _, tag := range tags {
		if !referenced[tag] {
			orphans = append(orphans, tag)
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}

// ReleasePins removes the given pins.
func (p *Pruner) ReleasePins(ctx context.Context, tags []string) (int, error) {
	var released int
	for _, tag := range tags {
		if err := p.rt.UnpinImage(ctx, tag); err != nil {
			p.log.Warn("could not release the image pin", "tag", tag, "error", err)
			continue
		}
		p.log.Info("released orphaned image pin", "tag", tag)
		released++
	}
	return released, nil
}

// PruneAll applies retention to every container in the store.
func (p *Pruner) PruneAll(ctx context.Context, policy Retention, dryRun bool) ([]*PruneReport, error) {
	containers, err := p.store.Containers(ctx)
	if err != nil {
		return nil, err
	}
	reports := make([]*PruneReport, 0, len(containers))
	for _, c := range containers {
		r, err := p.Prune(ctx, c, policy, dryRun)
		if err != nil {
			return reports, err
		}
		reports = append(reports, r)
	}
	return reports, nil
}
