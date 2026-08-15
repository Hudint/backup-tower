package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Hudint/backup-tower/internal/config"
	"github.com/Hudint/backup-tower/internal/discover"
	"github.com/Hudint/backup-tower/internal/runtime"
	"github.com/Hudint/backup-tower/internal/update"
)

func newPlanCmd() *cobra.Command {
	var (
		showAll        bool
		includeStopped bool
		explain        string
		noNotes        bool
		check          bool
	)

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show which containers backup-tower would act on, and why",
		Long: "Evaluates every container against its labels and the rule file, and\n" +
			"prints the result without touching anything.\n\n" +
			"Run this before enabling automatic updates. Automatic updates are opt-in, so\n" +
			"a host with no labels set must show nothing selected — and if it does not,\n" +
			"this is where you find out.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer e.Close()

			ctx := cmd.Context()
			sel, sourceNotes, err := buildSelector(ctx, e.cfg, e.rt, noNotes)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			for _, n := range sourceNotes {
				fmt.Fprintf(out, "%s\n", n)
			}
			if len(sourceNotes) > 0 {
				fmt.Fprintln(out)
			}

			if explain != "" {
				cand, err := sel.SelectOne(ctx, explain)
				if err != nil {
					return err
				}
				printExplain(out, cand)
				return nil
			}

			candidates, err := sel.Select(ctx, includeStopped)
			if err != nil {
				return err
			}

			var checks map[string]*update.Check
			if check {
				checks, err = runChecks(ctx, e, candidates, showAll)
				if err != nil {
					return err
				}
			}

			printPlanTable(out, candidates, checks, showAll)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&showAll, "all", "a", false, "list every container, not just the selected ones")
	cmd.Flags().BoolVar(&includeStopped, "include-stopped", false, "consider stopped containers too")
	cmd.Flags().StringVar(&explain, "explain", "", "explain the decision for one container in detail")
	cmd.Flags().BoolVar(&noNotes, "no-notes", false, "omit the advisory notes column")
	cmd.Flags().BoolVar(&check, "check", false, "ask each registry whether a newer image is available")
	return cmd
}

// buildSelector assembles the selection sources and reports what it managed to
// use.
//
// Both sources are local — labels come off the containers, rules off a file on
// disk — so this cannot fail for reasons outside the host. That is deliberate.
// Selection used to depend on Komodo as well, which meant an unreachable Komodo
// stopped every scheduled backup on the machine, including for containers that
// were configured entirely by label and needed nothing from it.
func buildSelector(_ context.Context, cfg config.Config, rt *runtime.Client, noNotes bool) (*discover.Selector, []string, error) {
	var notes []string

	rules := &discover.RuleFile{}
	if cfg.RulesFile != "" {
		loaded, err := discover.LoadRuleFile(cfg.RulesFile)
		if err != nil {
			return nil, nil, err
		}
		rules = loaded
		notes = append(notes, fmt.Sprintf("rules:  %s (%d rules)", cfg.RulesFile, len(rules.Rules)))
	}

	return discover.NewSelector(rt, discover.SelectorOptions{
		Rules:         rules,
		RetentionKeep: cfg.RetentionKeep,
		RetentionDays: cfg.RetentionDays,
		NoNotes:       noNotes,
	}), notes, nil
}

// runChecks asks the registries about the selected containers. Failures are
// carried in the result rather than aborting: one unreachable registry must not
// hide the state of every other container.
func runChecks(ctx context.Context, e *env, candidates []*discover.Candidate, all bool) (map[string]*update.Check, error) {
	auth, notes, err := buildRegistryAuth(ctx, e)
	if err != nil {
		return nil, err
	}
	for _, n := range notes {
		e.log.Info("registry credentials", "detail", n)
	}
	checker := update.NewChecker(e.rt, auth)

	wanted := make([]*discover.Candidate, 0, len(candidates))
	for _, c := range candidates {
		if !all && !c.Decision.Policy.Enabled {
			continue
		}
		wanted = append(wanted, c)
	}
	return checker.CheckAll(ctx, wanted, e.cfg.Concurrency), nil
}

func printPlanTable(out io.Writer, candidates []*discover.Candidate, checks map[string]*update.Check, showAll bool) {
	shown := candidates
	if !showAll {
		shown = nil
		for _, c := range candidates {
			if c.Decision.Policy.Enabled || c.Decision.Policy.Schedule != "" || len(c.Decision.Problems) > 0 {
				shown = append(shown, c)
			}
		}
	}

	summary := discover.Summarise(candidates)

	if len(shown) == 0 {
		fmt.Fprintf(out, "Nothing selected. %d containers were evaluated and none opted in.\n", summary.Total)
		fmt.Fprintf(out, "Set the label %s=true on a container, or use a rule file, to enable it.\n", discover.LabelEnable)
		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := "CONTAINER\tACTION\tSTRATEGY\tSNAPSHOT\tSCHEDULE\tENABLED BY\tNOTE"
	if checks != nil {
		header = "CONTAINER\tACTION\tREGISTRY\tSTRATEGY\tSNAPSHOT\tENABLED BY\tNOTE"
	}
	fmt.Fprintln(tw, header)
	for _, c := range shown {
		if checks != nil {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				c.Container.Name, action(c), checkCell(checks[c.Container.Name]),
				strategyCell(c), snapshotCell(c), enabledBy(c), firstNote(c))
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Container.Name,
			action(c),
			strategyCell(c),
			snapshotCell(c),
			dash(c.Decision.Policy.Schedule),
			enabledBy(c),
			firstNote(c),
		)
	}
	if err := tw.Flush(); err != nil {
		return
	}

	fmt.Fprintf(out, "\n%s\n", summary)
	if summary.NotUpdatable > 0 {
		fmt.Fprintf(out, "%d enabled containers cannot be updated from a registry; run with --all to see why.\n", summary.NotUpdatable)
	}
	if summary.Problems > 0 {
		fmt.Fprintf(out, "\n%d configuration problems found:\n", summary.Problems)
		for _, c := range candidates {
			for _, p := range c.Decision.Problems {
				fmt.Fprintf(out, "  %s: %s\n", c.Container.Name, p)
			}
		}
	}
	if !showAll {
		fmt.Fprintf(out, "\nShowing only selected containers; --all lists all %d.\n", summary.Total)
	}
}

func action(c *discover.Candidate) string {
	switch {
	case c.WillUpdate():
		return "update"
	case c.Decision.Policy.Enabled && c.Decision.Policy.MonitorOnly:
		// Not a skip: reporting available updates without applying them is a
		// deliberate mode, and calling it "skipped" would misrepresent it.
		return "monitor"
	case c.Decision.Policy.Enabled:
		return "skip: " + c.SkipReason()
	case c.Decision.Policy.Schedule != "":
		return "backup only"
	default:
		return "-"
	}
}

func checkCell(c *update.Check) string {
	if c == nil {
		return "-"
	}
	return c.Describe()
}

func strategyCell(c *discover.Candidate) string {
	if !c.WillUpdate() {
		return "-"
	}
	if c.Strategy == discover.StrategyCompose {
		return "compose"
	}
	return "recreate"
}

func snapshotCell(c *discover.Candidate) string {
	if !c.Decision.Policy.Snapshot {
		return "no"
	}
	parts := []string{string(c.Decision.Policy.Stop)}
	if c.Decision.Policy.IncludeBinds {
		parts = append(parts, "binds")
	}
	return strings.Join(parts, "+")
}

func enabledBy(c *discover.Candidate) string {
	if c.Decision.EnabledBy == nil {
		return "-"
	}
	return c.Decision.EnabledBy.String()
}

func firstNote(c *discover.Candidate) string {
	if len(c.Decision.Notes) == 0 {
		return ""
	}
	return c.Decision.Notes[0]
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// printExplain answers "why is this container treated the way it is" in full.
func printExplain(out io.Writer, c *discover.Candidate) {
	p := c.Decision.Policy
	fmt.Fprintf(out, "container   %s\n", c.Container.Name)
	fmt.Fprintf(out, "image       %s (%s)\n", c.Container.Image, c.Updatability)
	if proj := c.Container.ComposeProject(); proj != "" {
		fmt.Fprintf(out, "compose     %s / %s\n", proj, c.Container.ComposeService())
	}
	if c.ComposeFile != "" {
		fmt.Fprintf(out, "compose file %s\n", c.ComposeFile)
	}

	fmt.Fprintln(out)
	if c.WillUpdate() {
		fmt.Fprintf(out, "would be updated using the %s strategy\n", c.Strategy)
	} else {
		fmt.Fprintf(out, "would not be updated: %s\n", c.SkipReason())
	}

	fmt.Fprintln(out, "\neffective settings:")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  enabled\t%v\n", p.Enabled)
	fmt.Fprintf(tw, "  monitor only\t%v\n", p.MonitorOnly)
	fmt.Fprintf(tw, "  snapshot\t%v (stop: %s, binds: %v)\n", p.Snapshot, p.Stop, p.IncludeBinds)
	fmt.Fprintf(tw, "  schedule\t%s\n", dash(p.Schedule))
	fmt.Fprintf(tw, "  retention\t%d snapshots, %d days\n", p.RetentionKeep, p.RetentionDays)
	fmt.Fprintf(tw, "  auto rollback\t%v\n", p.Rollback)
	fmt.Fprintf(tw, "  strategy\t%s (resolved: %s)\n", p.Strategy, c.Strategy)
	if p.Hooks.PreSnapshot != "" || p.Hooks.PreUpdate != "" || p.Hooks.PostUpdate != "" {
		fmt.Fprintf(tw, "  hooks\tpre-snapshot: %s\n", dash(p.Hooks.PreSnapshot))
		fmt.Fprintf(tw, "  \tpre-update: %s\n", dash(p.Hooks.PreUpdate))
		fmt.Fprintf(tw, "  \tpost-update: %s\n", dash(p.Hooks.PostUpdate))
	}
	tw.Flush()

	fmt.Fprintln(out, "\nsettings came from, in order:")
	for _, o := range c.Decision.Origins {
		fmt.Fprintf(out, "  %s\n", o)
	}
	if c.Decision.EnabledBy != nil {
		fmt.Fprintf(out, "\nenabled by %s\n", c.Decision.EnabledBy)
	}

	if labels := towerLabels(c.Container.Labels); len(labels) > 0 {
		fmt.Fprintln(out, "\nlabels on the container:")
		for _, l := range labels {
			fmt.Fprintf(out, "  %s\n", l)
		}
	}

	for _, p := range c.Decision.Problems {
		fmt.Fprintf(out, "\nproblem: %s\n", p)
	}
	for _, n := range c.Decision.Notes {
		fmt.Fprintf(out, "\nnote: %s\n", n)
	}
}

func towerLabels(labels map[string]string) []string {
	var out []string
	for k, v := range labels {
		if strings.HasPrefix(k, discover.LabelPrefix) {
			out = append(out, k+"="+v)
		}
	}
	sort.Strings(out)
	return out
}
