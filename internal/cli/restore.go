package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/Hudint/backup-tower/internal/snapshot"
	"github.com/Hudint/backup-tower/internal/snapshot/source"
)

func newRestoreCmd() *cobra.Command {
	opts := snapshot.RestoreOptions{Data: true}
	var (
		assumeYes   bool
		forceHelper bool
		noStart     bool
		start       bool
	)

	cmd := &cobra.Command{
		Use:   "restore <container> [snapshot]",
		Short: "Put a snapshot's data back",
		Long: "Restores the archived volume contents into the container's volumes. With no\n" +
			"snapshot given, the most recent one is used.\n\n" +
			"This replaces the current contents. Anything written since the snapshot is\n" +
			"gone afterwards — that is the point, but it is worth being sure first.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("start") {
				opts.Start = &start
			}
			if noStart {
				v := false
				opts.Start = &v
			}
			return runRestore(cmd, args, opts, assumeYes, forceHelper)
		},
	}

	cmd.Flags().BoolVar(&opts.Data, "data", true, "restore archived volume contents")
	cmd.Flags().BoolVar(&opts.Binds, "binds", false, "also restore bind-mounted host paths")
	cmd.Flags().BoolVar(&opts.Config, "config", false, "recreate the container from the captured configuration")
	cmd.Flags().BoolVar(&opts.Image, "image", false, "recreate it on the image recorded in the snapshot")
	cmd.Flags().BoolVar(&opts.Chown, "chown", true, "restore recorded file ownership")
	cmd.Flags().BoolVar(&opts.SkipVerify, "skip-verify", false, "restore even if checksums do not match")
	cmd.Flags().BoolVar(&start, "start", false, "leave the container running afterwards")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "leave the container stopped afterwards")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&forceHelper, "helper", false, "always write volumes through a helper container")
	return cmd
}

func newRollbackCmd() *cobra.Command {
	var (
		assumeYes   bool
		forceHelper bool
		binds       bool
	)

	cmd := &cobra.Command{
		Use:   "rollback <container> [snapshot]",
		Short: "Undo an update: data, configuration and image together",
		Long: "The full way back after an update went wrong. Restores the archived data,\n" +
			"recreates the container from its captured configuration, and puts it back on\n" +
			"the image it was running when the snapshot was taken.\n\n" +
			"Equivalent to restore with --data --config --image.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := snapshot.RestoreOptions{
				Data:   true,
				Binds:  binds,
				Config: true,
				Image:  true,
				Chown:  true,
			}
			return runRestore(cmd, args, opts, assumeYes, forceHelper)
		},
	}

	cmd.Flags().BoolVar(&binds, "binds", false, "also restore bind-mounted host paths")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&forceHelper, "helper", false, "always write volumes through a helper container")
	return cmd
}

func runRestore(cmd *cobra.Command, args []string, opts snapshot.RestoreOptions, assumeYes, forceHelper bool) error {
	e, err := openEnv(cmd, true)
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

	srcOpts := source.Options{HelperImage: e.cfg.HelperImage}
	if forceHelper {
		srcOpts.Force = source.MethodHelper
	}
	restorer := snapshot.NewRestorer(e.rt, e.store, source.New(e.rt, srcOpts), e.log)

	plan, err := restorer.PlanRestore(ctx, container, id, opts)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	printPlan(out, container, id, plan, opts)

	if !assumeYes {
		ok, err := confirm(cmd, "Proceed?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "aborted, nothing was changed")
			return nil
		}
	}

	// A restore rewrites volume contents and may replace the container. Nothing
	// else may be working on it while that happens.
	release, err := e.locks.Acquire(ctx, container)
	if err != nil {
		return err
	}
	defer release()

	report, err := restorer.Restore(ctx, container, id, opts)
	if report != nil {
		printReport(out, report)
	}
	if err != nil {
		return err
	}
	return nil
}

// printPlan spells out what is about to happen. Restores destroy the current
// state by design, so the destructive parts are named explicitly rather than
// summarised.
func printPlan(out io.Writer, container, id string, p *snapshot.Plan, opts snapshot.RestoreOptions) {
	m := p.Manifest
	fmt.Fprintf(out, "Restoring %s from snapshot %s (%s, taken %s)\n",
		container, id, m.Quiesce, agePhrase(m.CreatedAt))

	if m.Quiesce == snapshot.QuiesceHot {
		fmt.Fprintln(out, "  note: this snapshot was taken while the container was running, so it is")
		fmt.Fprintln(out, "        only crash-consistent — a database may need to recover on start")
	}

	if len(p.Archives) > 0 {
		fmt.Fprintln(out, "\nThe current contents of these will be replaced:")
		for _, a := range p.Archives {
			where := a.Name
			if a.Kind == snapshot.KindBind {
				where = a.Source + " (host path)"
			}
			fmt.Fprintf(out, "  %-8s %-40s %s\n", a.Kind, where, humanBytes(a.ArchiveBytes))
		}
	} else if opts.Data {
		fmt.Fprintln(out, "\nNo data archives to restore.")
	}

	for _, b := range p.SkippedBinds {
		fmt.Fprintf(out, "  skipping bind mount %s (pass --binds to include it)\n", b)
	}

	if p.Recreate {
		fmt.Fprintln(out, "\nThe container will be replaced:")
		if p.Container != nil {
			fmt.Fprintf(out, "  current  %s running %s\n", shortID(p.Container.ID), p.Container.Image)
		} else {
			fmt.Fprintf(out, "  current  does not exist\n")
		}
		fmt.Fprintf(out, "  new      recreated from the captured spec on %s\n", p.Image)
	}

	state := "stopped"
	if p.WillStart {
		state = "running"
	}
	fmt.Fprintf(out, "\nAfterwards the container will be %s.\n", state)
}

func printReport(out io.Writer, r *snapshot.RestoreReport) {
	fmt.Fprintln(out)
	for _, m := range r.Mounts {
		fmt.Fprintf(out, "  restored %-8s %-40s (%s)\n", m.Kind, m.Name, m.Method)
	}
	if r.Recreated {
		fmt.Fprintf(out, "  recreated container on %s\n", r.Image)
	}
	if r.Started {
		fmt.Fprintln(out, "  container is running")
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(out, "  warning: %s\n", w)
	}
	if r.Duration > 0 {
		fmt.Fprintf(out, "  done in %s\n", r.Duration.Round(1e6))
	}
}

// confirm asks before doing something that cannot be undone. Without a terminal
// it refuses rather than assuming yes: a restore triggered by a script that
// never meant to run it is precisely the accident worth preventing.
func confirm(cmd *cobra.Command, question string) (bool, error) {
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !isTerminal(in) {
		return false, fmt.Errorf("this replaces live data and needs confirmation, but there is no terminal to ask on; pass --yes to proceed anyway")
	}

	fmt.Fprintf(cmd.OutOrStdout(), "\n%s [y/N] ", question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// isTerminal asks the kernel rather than guessing from the file mode. The
// common shortcut of checking for a character device also accepts /dev/null,
// which would leave an unattended run waiting at a prompt nobody can answer.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}
