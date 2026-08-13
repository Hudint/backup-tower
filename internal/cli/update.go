package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hudint/backup-tower/internal/discover"
	"github.com/Hudint/backup-tower/internal/runtime"
	"github.com/Hudint/backup-tower/internal/snapshot/source"
	"github.com/Hudint/backup-tower/internal/update"
)

func newUpdateCmd() *cobra.Command {
	var (
		dryRun         bool
		force          bool
		noHealth       bool
		healthTimeout  time.Duration
		settle         time.Duration
		includeStopped bool
		forceHelper    bool
	)

	cmd := &cobra.Command{
		Use:   "update [container...]",
		Short: "Update containers, snapshotting each one first",
		Long: "Checks the registry, snapshots, replaces the container and verifies it came\n" +
			"up. With no arguments it processes every container that opted in; naming\n" +
			"containers explicitly updates exactly those, regardless of whether they are\n" +
			"enabled.\n\n" +
			"Start with --dry-run.",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer e.Close()

			ctx := cmd.Context()
			sel, sourceNotes, err := buildSelector(ctx, e.cfg, e.rt, false)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			for _, n := range sourceNotes {
				fmt.Fprintln(out, n)
			}

			var candidates []*discover.Candidate
			if len(args) > 0 {
				for _, ref := range args {
					c, err := sel.SelectOne(ctx, ref)
					if err != nil {
						return err
					}
					// Naming a container is itself the instruction to act on it.
					c.Decision.Policy.Enabled = true
					c.Decision.Policy.MonitorOnly = false
					candidates = append(candidates, c)
				}
			} else {
				all, err := sel.Select(ctx, includeStopped)
				if err != nil {
					return err
				}
				for _, c := range all {
					if c.Decision.Policy.Enabled {
						candidates = append(candidates, c)
					}
				}
			}

			if len(candidates) == 0 {
				fmt.Fprintln(out, "Nothing is enabled for updates. Run 'backup-tower plan' to see why.")
				return nil
			}

			updater, err := newUpdater(ctx, e, forceHelper)
			if err != nil {
				return err
			}
			if !updater.ComposeAvailable() {
				fmt.Fprintln(out, "note: the docker compose plugin was not found; compose-managed containers will be recreated through the API instead")
			}

			opts := update.Options{
				DryRun:    dryRun,
				Force:     force,
				ZstdLevel: e.cfg.ZstdLevel,
				Health: update.HealthOptions{
					Timeout:  healthTimeout,
					Settle:   settle,
					Disabled: noHealth,
				},
			}

			results := runUpdates(ctx, updater, candidates, opts, out)
			return summariseUpdates(out, results, dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check the registries and report, without changing anything")
	cmd.Flags().BoolVar(&force, "force", false, "update even when the registry reports no change")
	cmd.Flags().BoolVar(&noHealth, "no-health-check", false, "skip the health gate after updating")
	cmd.Flags().DurationVar(&healthTimeout, "health-timeout", update.DefaultHealthOptions().Timeout, "how long to wait for a healthcheck to turn healthy")
	cmd.Flags().DurationVar(&settle, "settle", update.DefaultHealthOptions().Settle, "how long a container without a healthcheck must keep running")
	cmd.Flags().BoolVar(&includeStopped, "include-stopped", false, "consider stopped containers too")
	cmd.Flags().BoolVar(&forceHelper, "helper", false, "always read volumes through a helper container")
	return cmd
}

func newUpdater(ctx context.Context, e *env, forceHelper bool) (*update.Updater, error) {
	auth, err := runtime.LoadRegistryAuth("")
	if err != nil {
		return nil, err
	}
	srcOpts := source.Options{HelperImage: e.cfg.HelperImage}
	if forceHelper {
		srcOpts.Force = source.MethodHelper
	}
	return update.NewUpdater(e.rt, e.store, source.New(e.rt, srcOpts), auth, toolName(), e.log), nil
}

// runUpdates processes candidates in dependency order, reporting as it goes so
// a long run is legible while it happens rather than only at the end.
func runUpdates(ctx context.Context, u *update.Updater, candidates []*discover.Candidate, opts update.Options, out io.Writer) []*update.Result {
	ordered := update.Order(candidates)
	results := make([]*update.Result, 0, len(ordered))

	for _, c := range ordered {
		if ctx.Err() != nil {
			break
		}
		res := u.Update(ctx, c, opts)
		results = append(results, res)
		fmt.Fprintf(out, "  %-40s %s\n", res.Container, res.Describe())
		for _, w := range res.Warnings {
			fmt.Fprintf(out, "  %-40s   warning: %s\n", "", w)
		}
		if res.StrategyNote != "" {
			fmt.Fprintf(out, "  %-40s   note: %s\n", "", res.StrategyNote)
		}
	}
	return results
}

func summariseUpdates(out io.Writer, results []*update.Result, dryRun bool) error {
	var updated, rolledBack, failed, available, upToDate, skipped int
	for _, r := range results {
		switch r.Outcome {
		case update.OutcomeUpdated:
			updated++
		case update.OutcomeRolledBack:
			rolledBack++
		case update.OutcomeFailed:
			failed++
		case update.OutcomeReported, update.OutcomeWouldUpdate:
			available++
		case update.OutcomeUpToDate:
			upToDate++
		case update.OutcomeSkipped:
			skipped++
		}
	}

	fmt.Fprintf(out, "\n%d checked", len(results))
	if dryRun {
		fmt.Fprintf(out, ", %d would be updated, %d up to date, %d skipped\n", available, upToDate, skipped)
		return nil
	}
	fmt.Fprintf(out, ", %d updated, %d up to date, %d skipped\n", updated, upToDate, skipped)
	if available > 0 {
		fmt.Fprintf(out, "%d have updates available but were not applied\n", available)
	}
	if rolledBack > 0 {
		fmt.Fprintf(out, "%d failed and were rolled back\n", rolledBack)
	}

	// A rollback that failed too is the one case that needs someone now, so it
	// is stated separately rather than folded into a count.
	for _, r := range results {
		if r.RollbackErr != nil {
			fmt.Fprintf(out, "\n%s: the update failed and so did the rollback: %v\n", r.Container, r.RollbackErr)
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d containers failed to update", failed)
	}
	return nil
}
