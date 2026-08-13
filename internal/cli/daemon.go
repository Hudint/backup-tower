package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hudint/backup-tower/internal/discover"
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

			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				select {
				case <-cmd.Context().Done():
					e.log.Info("daemon stopping")
					return nil
				case <-ticker.C:
					if err := daemonPass(cmd, e, updater, opts); err != nil {
						// A failing pass must not end the daemon: the next one
						// may well succeed, and a silent exit would look exactly
						// like a daemon that is running fine.
						e.log.Error("update pass failed", "error", err)
					}
				}
			}
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

	var updated, failed, upToDate int
	var errs []error
	for _, c := range update.Order(enabled) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res := updater.Update(ctx, c, opts)
		switch res.Outcome {
		case update.OutcomeUpdated:
			updated++
			e.log.Info("container updated", "container", res.Container, "took", res.Duration.Round(time.Second))
		case update.OutcomeUpToDate:
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

	e.log.Info("update pass finished",
		"containers", len(enabled), "updated", updated, "up_to_date", upToDate,
		"failed", failed, "took", time.Since(started).Round(time.Second))
	return errors.Join(errs...)
}

func snapshotID(r *update.Result) string {
	if r.Snapshot == nil {
		return ""
	}
	return r.Snapshot.ID
}
