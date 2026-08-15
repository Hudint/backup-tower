package cli

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hudint/backup-tower/internal/discover"
	"github.com/Hudint/backup-tower/internal/schedule"
	"github.com/Hudint/backup-tower/internal/snapshot"
	"github.com/Hudint/backup-tower/internal/snapshot/source"
	"github.com/Hudint/backup-tower/internal/update"
)

func newDaemonCmd() *cobra.Command {
	var (
		interval    time.Duration
		once        bool
		runNow      bool
		noHealth    bool
		forceHelper bool
	)

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Keep checking for updates on an interval",
		Long: "Runs update passes on a schedule. Only containers that opted in are touched,\n" +
			"so this is safe to start before deciding which ones those are — check with\n" +
			"'backup-tower plan' first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer e.Close()

			if interval == 0 {
				interval = e.cfg.Interval
			}

			updater, err := newUpdater(cmd.Context(), e, forceHelper)
			if err != nil {
				return err
			}

			e.log.Info("daemon started",
				"interval", interval,
				"backup_dir", e.cfg.BackupDir,
				"compose", updater.ComposeAvailable())

			opts := update.Options{
				ZstdLevel: e.cfg.ZstdLevel,
				Health:    update.HealthOptions{Disabled: noHealth},
			}

			// The first pass waits a full interval by default. Starting the
			// daemon should not immediately change anything on the host —
			// that decision belongs to whoever is watching.
			if runNow || once {
				if err := daemonPass(cmd, e, updater, opts); err != nil {
					e.log.Error("update pass failed", "error", err)
				}
				if once {
					return nil
				}
			}

			// Two independent loops. They used to share one select, which meant
			// an update pass — minutes, when it actually replaces something —
			// blocked the minute tick that scheduled backups run on, and those
			// drifted behind their cron times for as long as the pass took.
			//
			// Running them separately is only safe because both take a
			// per-container lock first, so the one thing that must never happen
			// concurrently — two runs touching the same container — still cannot.
			var wg sync.WaitGroup
			wg.Add(2)

			go func() {
				defer wg.Done()
				ticker := time.NewTicker(interval)
				defer ticker.Stop()
				for {
					select {
					case <-cmd.Context().Done():
						return
					case <-ticker.C:
						if err := daemonPass(cmd, e, updater, opts); err != nil {
							// A failing pass must not end the daemon: the next
							// one may well succeed, and a silent exit would look
							// exactly like a daemon that is running fine.
							e.log.Error("update pass failed", "error", err)
						}
					}
				}
			}()

			go func() {
				defer wg.Done()
				// Checked every minute, which is the finest resolution a cron
				// expression can ask for.
				ticker := time.NewTicker(time.Minute)
				defer ticker.Stop()
				scheduler := schedule.NewChecker(e.store, time.Now())
				for {
					select {
					case <-cmd.Context().Done():
						return
					case <-ticker.C:
						if err := scheduledBackups(cmd, e, scheduler, forceHelper); err != nil {
							e.log.Error("scheduled backup pass failed", "error", err)
						}
					}
				}
			}()

			wg.Wait()
			e.log.Info("daemon stopping")
			return nil
		},
	}

	cmd.Flags().DurationVar(&interval, "interval", 0, "how often to check (default from configuration)")
	cmd.Flags().BoolVar(&once, "once", false, "run a single pass and exit")
	cmd.Flags().BoolVar(&runNow, "run-now", false, "run a pass immediately instead of waiting for the first interval")
	cmd.Flags().BoolVar(&noHealth, "no-health-check", false, "skip the health gate after updating")
	cmd.Flags().BoolVar(&forceHelper, "helper", false, "always read volumes through a helper container")
	return cmd
}

func daemonPass(cmd *cobra.Command, e *env, updater *update.Updater, opts update.Options) error {
	ctx := cmd.Context()
	started := time.Now()

	sel, sourceNotes, err := buildSelector(ctx, e.cfg, e.rt, true)
	if err != nil {
		return err
	}
	for _, n := range sourceNotes {
		e.log.Info("selection source", "detail", n)
	}

	all, err := sel.Select(ctx, false)
	if err != nil {
		return err
	}
	var enabled []*discover.Candidate
	for _, c := range all {
		if c.Decision.Policy.Enabled {
			enabled = append(enabled, c)
		}
	}
	if len(enabled) == 0 {
		e.log.Info("update pass finished", "containers", 0, "note", "nothing is enabled")
		return nil
	}

	// Ask every registry at once before touching anything. This is pure waiting,
	// so overlapping it turns a pass that scaled with the number of containers
	// into one that barely does.
	opts.Concurrency = e.cfg.Concurrency
	opts.Checks = updater.Checker().CheckAll(ctx, enabled, e.cfg.Concurrency)

	var updated, failed, upToDate, busy int
	var errs []error
	for _, c := range update.Order(enabled) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Skip rather than wait. Whatever holds the lock is already doing this
		// container's work, and blocking here would stall every container behind
		// it for the sake of one that is being handled anyway.
		release, err := e.locks.TryAcquire(c.Container.Name)
		if err != nil {
			busy++
			e.log.Info("skipping container, another run has it", "container", c.Container.Name)
			continue
		}
		res := updater.Update(ctx, c, opts)
		release()
		switch res.Outcome {
		case update.OutcomeUpdated:
			updated++
			e.log.Info("container updated", "container", res.Container, "took", res.Duration.Round(time.Second))
		case update.OutcomeUpToDate, update.OutcomeNoChange:
			upToDate++
		case update.OutcomeFailed, update.OutcomeRolledBack:
			failed++
			e.log.Error("update failed", "container", res.Container, "outcome", res.Outcome, "error", res.Err)
			errs = append(errs, fmt.Errorf("%s: %w", res.Container, res.Err))
		}
		if res.RollbackErr != nil {
			e.log.Error("the rollback failed as well; this needs attention",
				"container", res.Container, "snapshot", snapshotID(res), "error", res.RollbackErr)
		}
	}

	// Updates create snapshots too, so retention has to run here as well —
	// otherwise a container that is updated often but never on a schedule would
	// accumulate snapshots without limit.
	if updated > 0 {
		pruner := snapshot.NewPruner(e.store, e.rt, e.log)
		for _, c := range enabled {
			p := c.Decision.Policy
			policy := snapshot.Retention{Keep: p.RetentionKeep, Days: p.RetentionDays, ProtectManual: true}
			if policy.Valid() != nil {
				continue
			}
			if _, err := pruner.Prune(ctx, c.Container.Name, policy, false); err != nil {
				e.log.Warn("retention failed", "container", c.Container.Name, "error", err)
			}
		}
	}

	e.log.Info("update pass finished",
		"containers", len(enabled), "updated", updated, "up_to_date", upToDate,
		"failed", failed, "busy", busy, "took", time.Since(started).Round(time.Second))
	return errors.Join(errs...)
}

func snapshotID(r *update.Result) string {
	if r.Snapshot == nil {
		return ""
	}
	return r.Snapshot.ID
}

// scheduledBackups takes the backups that are due right now.
//
// These are separate from updates on purpose: a container can be worth backing
// up nightly without ever being updated automatically, and the two decisions
// belong to different people often enough that tying them together would be
// wrong.
func scheduledBackups(cmd *cobra.Command, e *env, scheduler *schedule.Checker, forceHelper bool) error {
	ctx := cmd.Context()

	sel, _, err := buildSelector(ctx, e.cfg, e.rt, true)
	if err != nil {
		return err
	}
	candidates, err := sel.Select(ctx, false)
	if err != nil {
		return err
	}

	srcOpts := source.Options{HelperImage: e.cfg.HelperImage}
	if forceHelper {
		srcOpts.Force = source.MethodHelper
	}
	taker := snapshot.NewTaker(e.rt, e.store, source.New(e.rt, srcOpts), toolName(), e.log)
	pruner := snapshot.NewPruner(e.store, e.rt, e.log)

	var errs []error
	for _, c := range candidates {
		policy := c.Decision.Policy
		if policy.Schedule == "" {
			continue
		}
		due, err := scheduler.Check(ctx, c.Container.Name, policy.Schedule, time.Now())
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Container.Name, err))
			continue
		}
		if due == nil {
			continue
		}

		e.log.Info("scheduled backup due",
			"container", c.Container.Name, "schedule", policy.Schedule,
			"scheduled_for", due.ScheduledFor.Format(time.RFC3339))

		// An update pass may be replacing this very container. Snapshotting it
		// halfway through would capture a state that never existed, and the
		// backup is due again on the next tick anyway.
		release, err := e.locks.TryAcquire(c.Container.Name)
		if err != nil {
			e.log.Info("scheduled backup deferred, another run has this container",
				"container", c.Container.Name)
			continue
		}

		// A scheduled backup defaults to reading hot. Creating downtime on a
		// timer is not a decision to make on the operator's behalf; stopping
		// first is available, but has to be asked for.
		stop := policy.Stop
		if stop == snapshot.StopAuto {
			stop = snapshot.StopNever
		}
		m, err := taker.Take(ctx, c.Container.ID, snapshot.Options{
			Trigger:      snapshot.TriggerSchedule,
			Stop:         stop,
			IncludeBinds: policy.IncludeBinds,
			Level:        e.cfg.ZstdLevel,
		})
		release()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Container.Name, err))
			continue
		}
		e.log.Info("scheduled backup complete",
			"container", c.Container.Name, "snapshot", m.ID,
			"quiesce", m.Quiesce, "bytes", m.ArchiveBytes())

		// Prune straight afterwards, so the backup directory cannot grow past
		// the policy between two housekeeping runs.
		policyRetention := snapshot.Retention{
			Keep:          policy.RetentionKeep,
			Days:          policy.RetentionDays,
			ProtectManual: true,
		}
		if err := policyRetention.Valid(); err != nil {
			e.log.Warn("retention not applied", "container", c.Container.Name, "error", err)
			continue
		}
		if _, err := pruner.Prune(ctx, c.Container.Name, policyRetention, false); err != nil {
			e.log.Warn("retention failed", "container", c.Container.Name, "error", err)
		}
	}
	return errors.Join(errs...)
}
