package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Hudint/backup-tower/internal/discover"
	"github.com/Hudint/backup-tower/internal/runtime"
	"github.com/Hudint/backup-tower/internal/snapshot"
	"github.com/Hudint/backup-tower/internal/snapshot/source"
	"github.com/Hudint/backup-tower/internal/snapshot/store"
)

// Outcome says what happened to one container.
type Outcome string

const (
	OutcomeUpToDate    Outcome = "up-to-date"
	OutcomeSkipped     Outcome = "skipped"
	OutcomeUpdated     Outcome = "updated"
	OutcomeRolledBack  Outcome = "rolled-back"
	OutcomeFailed      Outcome = "failed"
	OutcomeWouldUpdate Outcome = "would-update"
	OutcomeReported    Outcome = "update-available"
	// OutcomeNoChange means the strategy ran and decided nothing needed doing.
	// Compose reaches this conclusion whenever the image and the configuration
	// it computes are both unchanged.
	OutcomeNoChange Outcome = "no-change"
)

// Result is the record of one container's update attempt.
type Result struct {
	Container string
	Outcome   Outcome
	Check     *Check
	// Snapshot is the pre-update snapshot, nil when none was taken.
	Snapshot *snapshot.Manifest
	Strategy discover.Strategy
	// StrategyNote explains a downgrade from the requested strategy.
	StrategyNote string
	Health       *HealthResult
	Hooks        []*HookResult
	// RollbackErr is set when the update failed and the rollback failed too —
	// the one situation that needs a human immediately.
	RollbackErr error
	Warnings    []string
	Err         error
	Duration    time.Duration
}

// Options configures an update run.
type Options struct {
	// DryRun goes as far as the registry check and stops there.
	DryRun bool
	// Force applies the update even when the registry says nothing changed.
	Force bool
	// Health configures the gate.
	Health HealthOptions
	// HookTimeout bounds each lifecycle hook.
	HookTimeout time.Duration
	// ComposeTimeout bounds a compose invocation.
	ComposeTimeout time.Duration
	// ZstdLevel is passed to snapshots.
	ZstdLevel int
	// Concurrency bounds how many registry checks run at once. Applying an
	// update is never parallel; only the asking is.
	Concurrency int
	// Checks holds results from an earlier batch check, keyed by container
	// name. A container found here is not asked about again — the point of
	// checking in a batch is to overlap the waiting, which is lost if the
	// sequential pass repeats the work.
	Checks map[string]*Check
}

// Updater applies updates.
type Updater struct {
	rt      *runtime.Client
	store   store.Store
	src     *source.Accessor
	checker *Checker
	taker   *snapshot.Taker
	restore *snapshot.Restorer
	log     *slog.Logger

	composeBinary  string
	composeTimeout time.Duration
	hookTimeout    time.Duration
}

// NewUpdater wires up an updater.
func NewUpdater(rt *runtime.Client, st store.Store, src *source.Accessor, auth *runtime.RegistryAuth, tool string, log *slog.Logger) *Updater {
	if log == nil {
		log = slog.Default()
	}
	return &Updater{
		rt:            rt,
		store:         st,
		src:           src,
		checker:       NewChecker(rt, auth),
		taker:         snapshot.NewTaker(rt, st, src, tool, log),
		restore:       snapshot.NewRestorer(rt, st, src, log),
		log:           log,
		composeBinary: findComposeBinary(),
	}
}

// ComposeAvailable reports whether the compose plugin was found.
func (u *Updater) ComposeAvailable() bool { return u.composeBinary != "" }

// Checker exposes the registry checker so callers can batch their lookups
// before working through the containers one at a time.
func (u *Updater) Checker() *Checker { return u.checker }

// Update processes one candidate.
//
// The sequence is deliberate. The image is pulled while the container is still
// serving, so the download is not part of the downtime. The container is then
// stopped once — for the snapshot and the replacement together — because during
// an update it has to go down anyway, and that is exactly what makes a
// consistent snapshot free.
func (u *Updater) Update(ctx context.Context, cand *discover.Candidate, opts Options) *Result {
	started := time.Now()
	policy := cand.Decision.Policy
	res := &Result{Container: cand.Container.Name, Strategy: cand.Strategy}
	defer func() { res.Duration = time.Since(started) }()

	res.Check = opts.Checks[cand.Container.Name]
	if res.Check == nil {
		res.Check = u.checker.Check(ctx, cand)
	}
	switch {
	case res.Check.State == StateFailed:
		res.Outcome = OutcomeFailed
		res.Err = fmt.Errorf("%s: %w", res.Check.Reason, res.Check.Err)
		return res
	case res.Check.State == StateSkipped:
		res.Outcome = OutcomeSkipped
		return res
	case res.Check.State == StateUpToDate && !opts.Force:
		res.Outcome = OutcomeUpToDate
		return res
	}

	if policy.MonitorOnly {
		res.Outcome = OutcomeReported
		return res
	}
	if opts.DryRun {
		res.Outcome = OutcomeWouldUpdate
		return res
	}

	applier, note := u.pickApplier(cand)
	res.Strategy = applier.name()
	res.StrategyNote = note
	if note != "" {
		u.log.Warn("update strategy downgraded", "container", cand.Container.Name, "reason", note)
	}

	// Pull first. The container keeps serving while the bytes come down, so the
	// download never counts as downtime.
	reference := res.Check.Reference
	u.log.Info("pulling image", "container", cand.Container.Name, "image", reference)
	if err := u.pull(ctx, reference); err != nil {
		res.Outcome = OutcomeFailed
		res.Err = err
		return res
	}

	// Hooks that need the application alive have to run before anything stops
	// it, so both pre-hooks happen here: pre-update first, as the coarse "a
	// change is coming" signal, then pre-snapshot so a dump is taken with the
	// application already quiesced.
	for _, h := range []struct {
		phase   HookPhase
		command string
	}{
		{HookPreUpdate, policy.Hooks.PreUpdate},
		{HookPreSnapshot, policy.Hooks.PreSnapshot},
	} {
		if cand.Container.Running {
			if hr := runHook(ctx, u.rt, cand.Container.ID, h.phase, h.command, u.hookTimeoutOr(opts)); hr != nil {
				res.Hooks = append(res.Hooks, hr)
				if hr.Err != nil {
					// A failed pre-hook aborts before the container is touched.
					// Continuing would mean updating on top of preparation that
					// did not happen.
					res.Outcome = OutcomeFailed
					res.Err = hr.Err
					return res
				}
			}
		}
	}

	if policy.Snapshot {
		stop := policy.Stop
		if stop == snapshot.StopAuto {
			stop = snapshot.StopAlways
		}
		m, err := u.taker.Take(ctx, cand.Container.ID, snapshot.Options{
			Trigger:      snapshot.TriggerUpdate,
			Stop:         stop,
			IncludeBinds: policy.IncludeBinds,
			Level:        opts.ZstdLevel,
			// The replacement is about to come up; restarting the outgoing
			// container first would only double the downtime.
			LeaveStopped: true,
		})
		if err != nil {
			res.Outcome = OutcomeFailed
			res.Err = fmt.Errorf("snapshot before update: %w", err)
			// The container was possibly left stopped; put it back so a failed
			// snapshot does not take the service down with it.
			u.restart(ctx, cand.Container.ID)
			return res
		}
		res.Snapshot = m
		res.Warnings = append(res.Warnings, m.Warnings...)
	}

	newID, warnings, err := applier.apply(ctx, &applyRequest{
		Container: cand.Container,
		Spec:      u.specFor(ctx, res),
		Image:     reference,
		Suffix:    suffixFor(res),
	})
	res.Warnings = append(res.Warnings, warnings...)
	if err != nil {
		res.Outcome = OutcomeFailed
		res.Err = err
		u.rollbackIfPossible(ctx, cand, res, policy, "the update could not be applied")
		return res
	}

	// A strategy that hands back the same container did not replace anything.
	// Reporting that as an update would be a false statement, and this tool is
	// worth exactly as much as its reports are.
	replaced := newID != cand.Container.ID

	res.Health = checkHealth(ctx, u.rt, newID, opts.Health)
	if !res.Health.Passed() {
		res.Outcome = OutcomeFailed
		res.Err = fmt.Errorf("the updated container did not come up: %s", res.Health.Reason)
		u.rollbackIfPossible(ctx, cand, res, policy, res.Health.Reason)
		return res
	}

	if hr := runHook(ctx, u.rt, newID, HookPostUpdate, policy.Hooks.PostUpdate, u.hookTimeoutOr(opts)); hr != nil {
		res.Hooks = append(res.Hooks, hr)
		if hr.Err != nil {
			// The update itself worked; a failed post-hook is worth reporting
			// but not worth undoing a healthy deployment for.
			res.Warnings = append(res.Warnings, hr.Err.Error())
		}
	}

	res.Outcome = OutcomeUpdated
	if !replaced {
		res.Outcome = OutcomeNoChange
	}
	u.log.Info("update finished",
		"outcome", res.Outcome,
		"container", cand.Container.Name, "image", reference,
		"strategy", res.Strategy, "health", res.Health.Method,
		"took", time.Since(started).Round(time.Millisecond))
	return res
}

// pull fetches the image, trying each set of credentials the same way the check
// did. Doing otherwise would let a check succeed and the pull that follows it
// fail for want of the very credentials that just worked.
func (u *Updater) pull(ctx context.Context, reference string) error {
	var lastErr error
	for _, auth := range u.checker.Auth().Attempts(reference) {
		err := u.rt.PullImage(ctx, reference, auth)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// rollbackIfPossible undoes a failed update when the policy asks for it.
//
// Rollback is opt-in. Undoing an update automatically means restoring data and
// discarding whatever the new version wrote, which is the right call often
// enough to offer and not often enough to impose.
func (u *Updater) rollbackIfPossible(ctx context.Context, cand *discover.Candidate, res *Result, policy discover.Policy, why string) {
	if res.Snapshot == nil {
		res.Warnings = append(res.Warnings, "no snapshot was taken, so there is nothing to roll back to")
		u.leaveRunning(ctx, cand, res)
		return
	}
	if !policy.Rollback {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"automatic rollback is off; roll back by hand with: backup-tower rollback %s %s",
			cand.Container.Name, res.Snapshot.ID))
		u.leaveRunning(ctx, cand, res)
		return
	}

	u.log.Warn("rolling back failed update", "container", cand.Container.Name,
		"snapshot", res.Snapshot.ID, "reason", why)

	// Detach from the caller's context: an interrupted run must not abandon a
	// container halfway between two versions.
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
	defer cancel()

	start := true
	if _, err := u.restore.Restore(rollbackCtx, cand.Container.Name, res.Snapshot.ID, snapshot.RestoreOptions{
		Data:   true,
		Binds:  policy.IncludeBinds,
		Config: true,
		Image:  true,
		Chown:  true,
		Start:  &start,
	}); err != nil {
		res.RollbackErr = err
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"the rollback failed as well; the snapshot is intact at %s and can be restored by hand",
			res.Snapshot.ID))
		return
	}
	res.Outcome = OutcomeRolledBack
}

// leaveRunning makes sure a failed update does not leave the service down when
// no rollback happened.
func (u *Updater) leaveRunning(ctx context.Context, cand *discover.Candidate, res *Result) {
	if !cand.Container.Running {
		return
	}
	c, err := u.rt.Inspect(context.WithoutCancel(ctx), cand.Container.Name)
	if err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			res.Warnings = append(res.Warnings, "the container no longer exists after the failed update")
		}
		return
	}
	if c.Running {
		return
	}
	u.restart(ctx, c.ID)
}

func (u *Updater) restart(ctx context.Context, id string) {
	restartCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := u.rt.Start(restartCtx, id); err != nil {
		u.log.Error("could not start the container again after a failed update", "container", id, "error", err)
	}
}

// specFor loads the configuration captured by the pre-update snapshot.
func (u *Updater) specFor(ctx context.Context, res *Result) *snapshot.Spec {
	if res.Snapshot == nil {
		return nil
	}
	spec, err := snapshot.LoadSpec(ctx, u.store, res.Snapshot.Container.Name, res.Snapshot.ID)
	if err != nil {
		u.log.Warn("could not read the captured spec; falling back to the live configuration",
			"container", res.Snapshot.Container.Name, "error", err)
		return nil
	}
	return spec
}

func suffixFor(res *Result) string {
	if res.Snapshot != nil {
		return res.Snapshot.ID
	}
	return "update"
}

func (u *Updater) hookTimeoutOr(opts Options) time.Duration {
	if opts.HookTimeout > 0 {
		return opts.HookTimeout
	}
	if u.hookTimeout > 0 {
		return u.hookTimeout
	}
	return 5 * time.Minute
}

// Describe renders a result as one line.
func (r *Result) Describe() string {
	switch r.Outcome {
	case OutcomeUpToDate:
		return "up to date"
	case OutcomeSkipped:
		return "skipped: " + r.Check.Reason
	case OutcomeReported:
		return "update available, not applied (monitor only)"
	case OutcomeWouldUpdate:
		return "would update: " + r.Check.Describe()
	case OutcomeUpdated:
		if r.Health != nil && r.Health.Method == "runtime" {
			return "updated (health verified only by staying up)"
		}
		return "updated"
	case OutcomeNoChange:
		return "no change was needed: " + string(r.Strategy) + " considered the container current"
	case OutcomeRolledBack:
		return "update failed and was rolled back: " + errText(r.Err)
	case OutcomeFailed:
		if r.RollbackErr != nil {
			return "update failed AND the rollback failed: " + errText(r.Err)
		}
		return "failed: " + errText(r.Err)
	default:
		return string(r.Outcome)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
