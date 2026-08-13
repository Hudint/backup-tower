package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hudint/backup-tower/internal/snapshot"
	"github.com/hudint/backup-tower/internal/snapshot/source"
)

func newSnapshotCmd() *cobra.Command {
	var (
		includeBinds bool
		stopPolicy   string
		level        int
		forceHelper  bool
	)

	cmd := &cobra.Command{
		Use:   "snapshot <container>",
		Short: "Snapshot a container's configuration and storage",
		Long: "Captures the container's configuration, its named volumes and — when asked —\n" +
			"its bind mounts. Use this before making a change by hand.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stop, err := parseStopPolicy(stopPolicy)
			if err != nil {
				return err
			}

			e, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer e.Close()

			opts := source.Options{HelperImage: e.cfg.HelperImage}
			if forceHelper {
				opts.Force = source.MethodHelper
			}

			if level == 0 {
				level = e.cfg.ZstdLevel
			}

			taker := snapshot.NewTaker(e.rt, e.store, source.New(e.rt, opts), toolName(), e.log)
			m, err := taker.Take(cmd.Context(), args[0], snapshot.Options{
				Trigger:      snapshot.TriggerManual,
				Stop:         stop,
				IncludeBinds: includeBinds,
				Level:        level,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s  %s\n", m.Container.Name, m.ID)
			fmt.Fprintf(out, "  %s at %s\n", m.Quiesce, e.store.Location(m.Container.Name, m.ID))
			for _, a := range m.Archives {
				fmt.Fprintf(out, "  %-8s %-30s %10s  (%s, %d files)\n",
					a.Kind, a.Name, humanBytes(a.ArchiveBytes), a.Method, a.Files)
			}
			fmt.Fprintf(out, "  total %s in %s\n", humanBytes(m.ArchiveBytes()), m.Duration().Round(1e6))
			printWarnings(cmd, m.Warnings)
			return nil
		},
	}

	cmd.Flags().BoolVar(&includeBinds, "binds", false, "archive bind-mounted host paths as well")
	cmd.Flags().StringVar(&stopPolicy, "stop", "never", "stop the container while reading: auto, always or never")
	cmd.Flags().IntVar(&level, "level", 0, "zstd compression level (default from configuration)")
	cmd.Flags().BoolVar(&forceHelper, "helper", false, "always read volumes through a helper container")
	return cmd
}

func parseStopPolicy(s string) (snapshot.StopPolicy, error) {
	switch snapshot.StopPolicy(s) {
	case snapshot.StopAuto:
		return snapshot.StopAuto, nil
	case snapshot.StopAlways:
		return snapshot.StopAlways, nil
	case snapshot.StopNever:
		return snapshot.StopNever, nil
	default:
		return "", fmt.Errorf("--stop must be auto, always or never, got %q", s)
	}
}

func printWarnings(cmd *cobra.Command, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	out := cmd.ErrOrStderr()
	for _, w := range warnings {
		fmt.Fprintf(out, "  warning: %s\n", w)
	}
}
