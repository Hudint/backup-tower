package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Hudint/backup-tower/internal/snapshot"
)

func newPruneCmd() *cobra.Command {
	var (
		keep          int
		days          int
		dryRun        bool
		includeManual bool
		assumeYes     bool
	)

	cmd := &cobra.Command{
		Use:   "prune [container...]",
		Short: "Apply the retention policy to stored snapshots",
		Long: "Deletes snapshots that fall outside the retention policy and releases the\n" +
			"image pins they were holding.\n\n" +
			"A snapshot is kept if it is among the most recent N or younger than D days.\n" +
			"Snapshots taken by hand are left alone unless you ask otherwise: someone who\n" +
			"took one before making a change did so because they wanted it afterwards.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// The engine is only needed to release image pins; without it the
			// snapshots still go, which is the more important half.
			e, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer e.Close()

			if keep < 0 {
				keep = e.cfg.RetentionKeep
			}
			if days < 0 {
				days = e.cfg.RetentionDays
			}
			policy := snapshot.Retention{Keep: keep, Days: days, ProtectManual: !includeManual}
			if err := policy.Valid(); err != nil {
				return err
			}

			ctx := cmd.Context()
			pruner := snapshot.NewPruner(e.store, e.rt, e.log)

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Retention: keep the last %d, and everything younger than %d days", policy.Keep, policy.Days)
			if policy.ProtectManual {
				fmt.Fprint(out, "; hand-taken snapshots are protected")
			}
			fmt.Fprintln(out, ".")

			// Always show what would go before anything goes.
			preview, err := prune(ctx, pruner, e, args, policy, true)
			if err != nil {
				return err
			}
			printPruneReports(out, preview)

			total := 0
			var freed int64
			for _, r := range preview {
				total += r.Removed
				freed += r.Freed
			}
			if total == 0 {
				fmt.Fprintln(out, "\nNothing to remove.")
				return releaseOrphanedPins(ctx, pruner, out, dryRun)
			}
			fmt.Fprintf(out, "\n%d snapshots would be removed, freeing %s.\n", total, humanBytes(freed))
			if dryRun {
				return nil
			}

			if !assumeYes {
				ok, err := confirm(cmd, "Remove them?")
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(out, "aborted, nothing was removed")
					return nil
				}
			}

			reports, err := prune(ctx, pruner, e, args, policy, false)
			if err != nil {
				return err
			}
			var removed, released int
			freed = 0
			for _, r := range reports {
				removed += r.Removed
				released += r.Released
				freed += r.Freed
			}
			fmt.Fprintf(out, "\nRemoved %d snapshots, freed %s, released %d image pins.\n",
				removed, humanBytes(freed), released)

			// Pins whose snapshot went away by other means would otherwise stay
			// forever and defeat the pruning they exist to survive.
			return releaseOrphanedPins(ctx, pruner, out, false)
		},
	}

	cmd.Flags().IntVar(&keep, "keep", -1, "how many recent snapshots to keep (default from configuration)")
	cmd.Flags().IntVar(&days, "days", -1, "keep everything younger than this many days (default from configuration)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only report what would be removed")
	cmd.Flags().BoolVar(&includeManual, "include-manual", false, "also prune snapshots taken by hand")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func prune(ctx context.Context, p *snapshot.Pruner, e *env, containers []string, policy snapshot.Retention, dryRun bool) ([]*snapshot.PruneReport, error) {
	if len(containers) == 0 {
		return p.PruneAll(ctx, policy, dryRun)
	}
	reports := make([]*snapshot.PruneReport, 0, len(containers))
	for _, c := range containers {
		r, err := p.Prune(ctx, c, policy, dryRun)
		if err != nil {
			return reports, err
		}
		reports = append(reports, r)
	}
	return reports, nil
}

// releaseOrphanedPins clears image pins no snapshot refers to any more.
func releaseOrphanedPins(ctx context.Context, p *snapshot.Pruner, out io.Writer, dryRun bool) error {
	orphans, err := p.OrphanedPins(ctx)
	if err != nil {
		// Not fatal: the snapshots were pruned, which is the important half.
		fmt.Fprintf(out, "could not check for orphaned image pins: %v\n", err)
		return nil
	}
	if len(orphans) == 0 {
		return nil
	}
	if dryRun {
		fmt.Fprintf(out, "\n%d image pins refer to snapshots that no longer exist and would be released:\n", len(orphans))
		for _, tag := range orphans {
			fmt.Fprintf(out, "  %s\n", tag)
		}
		return nil
	}
	released, err := p.ReleasePins(ctx, orphans)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Released %d image pins whose snapshots no longer exist.\n", released)
	return nil
}

func printPruneReports(out io.Writer, reports []*snapshot.PruneReport) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	var rows int
	for _, r := range reports {
		for _, d := range r.Decisions {
			if d.Keep {
				continue
			}
			if rows == 0 {
				fmt.Fprintln(tw, "\nCONTAINER\tSNAPSHOT\tAGE\tTRIGGER\tSIZE\tREASON")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Container, d.ID, age(d.Created), d.Trigger, humanBytes(d.Bytes), d.Reason)
			rows++
		}
	}
	if rows > 0 {
		tw.Flush()
	}
}
