package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Hudint/backup-tower/internal/snapshot"
)

func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <container> [snapshot]",
		Short: "Show what a snapshot contains",
		Long:  "Prints the manifest of a snapshot. With no snapshot given, the most recent one is used.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer e.Close()

			ctx := cmd.Context()
			container := args[0]
			id := ""
			if len(args) == 2 {
				id = args[1]
			}
			id, err = snapshot.Resolve(ctx, e.store, container, id)
			if err != nil {
				return err
			}

			m, err := snapshot.LoadManifest(ctx, e.store, container, id)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "snapshot   %s\n", m.ID)
			fmt.Fprintf(out, "container  %s (%s)\n", m.Container.Name, shortID(m.Container.ID))
			fmt.Fprintf(out, "image      %s\n", m.Container.Image)
			if len(m.Container.ImageDigests) > 0 {
				fmt.Fprintf(out, "digest     %s\n", m.Container.ImageDigests[0])
			} else {
				fmt.Fprintf(out, "digest     none — built locally, cannot be re-pulled\n")
			}
			if m.Container.ComposeProject != "" {
				fmt.Fprintf(out, "compose    %s / %s\n", m.Container.ComposeProject, m.Container.ComposeService)
			}
			fmt.Fprintf(out, "taken      %s (%s)\n", m.CreatedAt.Local().Format("2006-01-02 15:04:05"), agePhrase(m.CreatedAt))
			fmt.Fprintf(out, "trigger    %s\n", m.Trigger)
			fmt.Fprintf(out, "state      %s, container was %s\n", m.Quiesce, runningWord(m.WasRunning))
			fmt.Fprintf(out, "duration   %s\n", m.Duration().Round(1e6))
			fmt.Fprintf(out, "engine     %s %s (api %s)\n", m.Engine.Flavor, m.Engine.Version, m.Engine.APIVersion)
			fmt.Fprintf(out, "location   %s\n", e.store.Location(container, id))

			if len(m.Archives) > 0 {
				fmt.Fprintln(out)
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "KIND\tNAME\tMOUNTED AT\tSIZE\tSOURCE SIZE\tFILES\tMETHOD")
				for _, a := range m.Archives {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
						a.Kind, a.Name, a.Destination,
						humanBytes(a.ArchiveBytes), humanBytes(a.SourceBytes), a.Files, a.Method)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}

			var skipped []string
			for _, a := range m.Archives {
				for _, s := range a.Skipped {
					skipped = append(skipped, a.Name+"/"+s)
				}
			}
			if len(skipped) > 0 {
				fmt.Fprintf(out, "\nskipped entries (%d):\n", len(skipped))
				for _, s := range skipped {
					fmt.Fprintf(out, "  %s\n", s)
				}
			}

			if len(m.Warnings) > 0 {
				fmt.Fprintln(out, "\nwarnings:")
				for _, w := range m.Warnings {
					fmt.Fprintf(out, "  %s\n", w)
				}
			}
			return nil
		},
	}
}

func newVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <container> [snapshot]",
		Short: "Check a snapshot's archives against their recorded checksums",
		Long: "Reads every archive and recomputes its checksum. A backup that is never read\n" +
			"is only a hypothesis; this turns it into something you can rely on before an\n" +
			"incident rather than during one.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer e.Close()

			ctx := cmd.Context()
			container := args[0]
			id := ""
			if len(args) == 2 {
				id = args[1]
			}
			id, err = snapshot.Resolve(ctx, e.store, container, id)
			if err != nil {
				return err
			}

			m, results, err := snapshot.Verify(ctx, e.store, container, id)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s  %s\n", m.Container.Name, m.ID)

			bad := 0
			for _, r := range results {
				switch {
				case r.Err != nil:
					bad++
					fmt.Fprintf(out, "  FAIL  %-30s %v\n", r.Archive.Name, r.Err)
				case !r.OK:
					bad++
					fmt.Fprintf(out, "  FAIL  %-30s checksum mismatch\n", r.Archive.Name)
					fmt.Fprintf(out, "        recorded %s\n        found    %s\n", r.Archive.SHA256, r.Got)
				default:
					fmt.Fprintf(out, "  ok    %-30s %s\n", r.Archive.Name, humanBytes(r.Archive.ArchiveBytes))
				}
			}

			if bad > 0 {
				return fmt.Errorf("%d of %d archives failed verification", bad, len(results))
			}
			fmt.Fprintf(out, "  %d archives verified\n", len(results))
			return nil
		},
	}
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func runningWord(running bool) string {
	if running {
		return "running"
	}
	return "stopped"
}
