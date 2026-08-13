package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hudint/backup-tower/internal/snapshot"
	"github.com/hudint/backup-tower/internal/version"
)

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [container]",
		Short: "List stored snapshots",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Reading the backup directory must not depend on a reachable
			// engine: when things have gone wrong, this is the first question.
			e, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer e.Close()

			ctx := cmd.Context()
			containers := args
			if len(containers) == 0 {
				containers, err = e.store.Containers(ctx)
				if err != nil {
					return err
				}
			}
			if len(containers) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no snapshots in %s\n", e.cfg.BackupDir)
				return nil
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CONTAINER\tSNAPSHOT\tAGE\tTRIGGER\tSTATE\tARCHIVES\tSIZE")

			var rows int
			for _, name := range containers {
				ids, err := e.store.Snapshots(ctx, name)
				if err != nil {
					return err
				}
				for _, id := range ids {
					m, err := snapshot.LoadManifest(ctx, e.store, name, id)
					if err != nil {
						// A snapshot whose manifest cannot be read is still worth
						// showing — it is precisely the one worth looking into.
						fmt.Fprintf(tw, "%s\t%s\t\t\tunreadable\t\t\n", name, id)
						rows++
						continue
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
						name, id, age(m.CreatedAt), m.Trigger, m.Quiesce,
						len(m.Archives), humanBytes(m.ArchiveBytes()))
					rows++
				}
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if rows == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no snapshots in %s\n", e.cfg.BackupDir)
			}
			return nil
		},
	}
}

func age(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// agePhrase renders an age as prose, where "just now ago" would read wrong.
func agePhrase(t time.Time) string {
	a := age(t)
	if a == "just now" || a == "?" {
		return a
	}
	return a + " ago"
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func toolName() string {
	return "backup-tower/" + version.Version
}
