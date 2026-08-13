// Package cli assembles the command tree. The same binary serves as daemon,
// operator CLI and helper container, so every entry point lives here.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hudint/backup-tower/internal/version"
)

// Run executes the command tree and returns a process exit code.
func Run(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := newRootCmd()
	root.SetArgs(args)

	if err := root.ExecuteContext(ctx); err != nil {
		// Cobra already reports usage errors; anything else we print ourselves.
		if !errors.Is(err, errSilent) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		return 1
	}
	return 0
}

// errSilent marks errors that have already been reported to the user.
var errSilent = errors.New("silent")

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup-tower",
		Short: "Snapshot and update containers without losing the way back",
		Long: "backup-tower snapshots container volumes and configuration, then updates\n" +
			"containers with a verified rollback path. Snapshots run on update, on a\n" +
			"schedule, or on demand.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}

	cmd.PersistentFlags().String("backup-dir", "", "backup destination (default $TOWER_BACKUP_DIR or /backups)")
	cmd.PersistentFlags().BoolP("verbose", "v", false, "log at debug level")

	cmd.AddCommand(
		newInfoCmd(),
		newSnapshotCmd(),
		newListCmd(),
		newShowCmd(),
		newVerifyCmd(),
		newHelperCmd(),
	)
	return cmd
}
