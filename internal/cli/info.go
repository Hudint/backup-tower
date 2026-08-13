package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Hudint/backup-tower/internal/runtime"
	"github.com/Hudint/backup-tower/internal/version"
)

// newInfoCmd reports what backup-tower sees. It is the first thing to run when
// something is not working: it answers "can I even reach the engine, and which
// one is it".
func newInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show the connected container engine and tool version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			rt, err := runtime.New(ctx)
			if err != nil {
				return err
			}
			defer rt.Close()

			containers, err := rt.List(ctx, true)
			if err != nil {
				return err
			}
			var running int
			for _, c := range containers {
				if c.Running {
					running++
				}
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "backup-tower  %s\n", version.Version)
			fmt.Fprintf(out, "engine        %s %s\n", rt.Flavor(), rt.ServerVersion())
			fmt.Fprintf(out, "api           %s\n", rt.APIVersion())
			fmt.Fprintf(out, "host          %s\n", rt.Host())
			fmt.Fprintf(out, "containers    %d total, %d running\n", len(containers), running)
			return nil
		},
	}
}
