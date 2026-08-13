package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hudint/backup-tower/internal/snapshot/archive"
	"github.com/hudint/backup-tower/internal/snapshot/source"
)

// newHelperCmd is the entry point used inside short-lived helper containers.
// backup-tower runs its own image as the helper so the archiving code is the
// same on both sides — no shell pipes, no external tar or zstd binaries, and no
// second implementation that could drift from this one.
//
// It is hidden because it is not meant to be run by hand.
func newHelperCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "helper",
		Short:  "Internal commands executed inside helper containers",
		Hidden: true,
	}
	cmd.AddCommand(newHelperArchiveCmd())
	return cmd
}

func newHelperArchiveCmd() *cobra.Command {
	var (
		src     string
		level   int
		exclude []string
	)

	cmd := &cobra.Command{
		Use:   "archive",
		Short: "Write a tar+zstd archive of a directory to stdout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// stdout carries the archive and nothing else; the report goes to
			// stderr, where the parent process reads it back.
			stats, err := archive.Create(cmd.Context(), src, os.Stdout, archive.CreateOptions{
				Level:   level,
				Exclude: exclude,
			})
			if err != nil {
				return err
			}
			line, err := source.EncodeStats(stats)
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, line)
			return nil
		},
	}

	cmd.Flags().StringVar(&src, "source", source.HelperMount, "directory to archive")
	cmd.Flags().IntVar(&level, "level", archive.DefaultLevel, "zstd compression level")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "paths to skip, relative to the source")
	return cmd
}
