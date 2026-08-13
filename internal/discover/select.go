package discover

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Hudint/backup-tower/internal/runtime"
)

// Updatability says whether an image can be checked against a registry at all.
type Updatability string

const (
	// Updatable means the image has a registry reference we can compare against.
	Updatable Updatability = "updatable"
	// LocalImage means the image was built here and never pushed, so there is
	// no registry to ask. Snapshots still work; updates cannot.
	LocalImage Updatability = "local-image"
	// UnnamedImage means the container references an image only by ID, with no
	// repository name left to resolve.
	UnnamedImage Updatability = "unnamed-image"
)

// Candidate is one container together with the decision made about it.
type Candidate struct {
	Container *runtime.Container
	Decision  Decision

	// Updatability reports whether an update could even be attempted.
	Updatability Updatability
	// Strategy is the resolved update strategy, with auto already decided.
	Strategy Strategy
	// ComposeFile is the compose file backing this container, when there is a
	// readable one.
	ComposeFile string
}

// WillUpdate reports whether an update run would act on this container.
func (c *Candidate) WillUpdate() bool {
	return c.Decision.Policy.Enabled &&
		!c.Decision.Policy.MonitorOnly &&
		c.Updatability == Updatable
}

// SkipReason explains in one phrase why a container is not being updated, empty
// when it is. The plan output is only useful if it answers "why not" as
// reliably as "why".
func (c *Candidate) SkipReason() string {
	switch {
	case !c.Decision.Policy.Enabled:
		return "not enabled"
	case c.Decision.Policy.MonitorOnly:
		return "monitor only"
	case c.Updatability == LocalImage:
		return "image built locally, no registry to check"
	case c.Updatability == UnnamedImage:
		return "image has no name, only an ID"
	default:
		return ""
	}
}

// Selector produces candidates.
type Selector struct {
	rt       *runtime.Client
	rules    *RuleFile
	komodo   *KomodoSelection
	tagRules []TagRule
	keep     int
	days     int
	noNotes  bool
}

// SelectorOptions configures a Selector.
type SelectorOptions struct {
	Rules *RuleFile
	// Komodo is the selection contributed by Komodo, nil when not configured.
	Komodo *KomodoSelection
	// ExtraTagRules are applied before the rule file's, and carry the simple
	// KOMODO_TAG case.
	ExtraTagRules []TagRule
	// RetentionKeep and RetentionDays seed the defaults.
	RetentionKeep int
	RetentionDays int
	// NoNotes suppresses the advisory notes.
	NoNotes bool
}

// NewSelector builds a selector.
func NewSelector(rt *runtime.Client, opts SelectorOptions) *Selector {
	rules := opts.Rules
	if rules == nil {
		rules = &RuleFile{}
	}
	return &Selector{
		rt:       rt,
		rules:    rules,
		komodo:   opts.Komodo,
		tagRules: append(opts.ExtraTagRules, rules.KomodoTags...),
		keep:     opts.RetentionKeep,
		days:     opts.RetentionDays,
		noNotes:  opts.NoNotes,
	}
}

// Select evaluates every container on the host.
func (s *Selector) Select(ctx context.Context, includeStopped bool) ([]*Candidate, error) {
	containers, err := s.rt.List(ctx, includeStopped)
	if err != nil {
		return nil, err
	}

	out := make([]*Candidate, 0, len(containers))
	for _, c := range containers {
		out = append(out, s.evaluate(ctx, c))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Container.Name < out[j].Container.Name
	})
	return out, nil
}

// SelectOne evaluates a single container by name or ID.
func (s *Selector) SelectOne(ctx context.Context, ref string) (*Candidate, error) {
	c, err := s.rt.Inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	return s.evaluate(ctx, c), nil
}

// evaluate resolves the policy for one container, in precedence order:
// defaults, then the rule file, then Komodo, then labels. Labels come last
// because they sit on the object itself.
func (s *Selector) evaluate(ctx context.Context, c *runtime.Container) *Candidate {
	cand := &Candidate{Container: c}
	d := &cand.Decision
	policy := Defaults(s.keep, s.days)
	d.record("default", "")

	applySettings(&policy, s.rules.Defaults, "config", "defaults", d)

	for i := range s.rules.Rules {
		r := &s.rules.Rules[i]
		if r.Match.Matches(c.Name, c.ComposeProject(), c.Image, c.Labels) {
			applySettings(&policy, r.Set, "rule", strings.TrimPrefix(r.label(i), "rule "), d)
		}
	}

	s.applyKomodoTags(&policy, c, d)

	applyLabels(&policy, c.Labels, d)

	cand.Decision.Policy = policy
	cand.Updatability = s.updatability(ctx, c)
	cand.ComposeFile = composeFile(c)
	cand.Strategy = resolveStrategy(policy.Strategy, cand.ComposeFile)

	if !s.noNotes {
		addNotes(cand)
	}
	return cand
}

// applyKomodoTags turns the Komodo tags a resource carries into settings.
//
// Tags are the way to configure a stack without touching its compose file: tag
// it in the Komodo UI and it is configured, with no commit to make and no
// redeploy to schedule.
func (s *Selector) applyKomodoTags(policy *Policy, c *runtime.Container, d *Decision) {
	if s.komodo == nil {
		return
	}

	var tags []string
	var what string
	if p := c.ComposeProject(); p != "" {
		if t, ok := s.komodo.Projects[p]; ok {
			tags, what = t, "stack "+p
		}
	}
	if tags == nil {
		if t, ok := s.komodo.Containers[c.Name]; ok {
			tags, what = t, "deployment "+c.Name
		}
	}
	if len(tags) == 0 {
		return
	}

	has := make(map[string]bool, len(tags))
	for _, t := range tags {
		has[t] = true
	}

	// The rules are applied in the order they are written, so a later tag can
	// refine an earlier one — the same way the rule list works.
	for _, tr := range s.tagRules {
		if !has[tr.Tag] {
			continue
		}
		applySettings(policy, tr.Set, "komodo tag", fmt.Sprintf("%s on %s", tr.Tag, what), d)
	}
}

// updatability asks whether a registry could answer a question about this image.
func (s *Selector) updatability(ctx context.Context, c *runtime.Container) Updatability {
	// A container whose configured image is a bare hex ID has nothing to
	// resolve: the tag it was built from is long gone.
	if c.Image == "" || looksLikeImageID(c.Image) {
		return UnnamedImage
	}
	img, err := s.rt.InspectImage(ctx, c.ImageID)
	if err != nil {
		// The image is gone locally; the reference may still be pullable.
		return Updatable
	}
	if !img.FromRegistry() {
		return LocalImage
	}
	return Updatable
}

func looksLikeImageID(ref string) bool {
	s := strings.TrimPrefix(ref, "sha256:")
	if len(s) < 12 || strings.ContainsAny(s, "/:._-") {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// composeFile returns the compose file this container came from, if it exists
// and is readable from here. Readability is the point: inside a container the
// path is only reachable when it has been mounted in, and a strategy that
// cannot run is worse than one that was never chosen.
func composeFile(c *runtime.Container) string {
	files := c.Labels["com.docker.compose.project.config_files"]
	if files == "" {
		return ""
	}
	// The label holds a comma-separated list; the first entry is the primary
	// compose file.
	first := strings.TrimSpace(strings.Split(files, ",")[0])
	if first == "" {
		return ""
	}
	if _, err := os.Stat(first); err != nil {
		return ""
	}
	return first
}

func resolveStrategy(want Strategy, composeFile string) Strategy {
	switch want {
	case StrategyCompose, StrategyRecreate:
		return want
	default:
		if composeFile != "" {
			return StrategyCompose
		}
		return StrategyRecreate
	}
}

// selfManaging lists stacks that ship their own update mechanism. This is
// advisory only and never blocks anything: whether to let backup-tower update
// them is the operator's decision, and a tool that quietly overrides that
// decision is worse than one that merely mentions it.
var selfManaging = []struct {
	prefix string
	what   string
}{
	{"nextcloud-aio-", "Nextcloud AIO updates its own containers through the mastercontainer"},
	{"mailcowdockerized-", "mailcow updates itself through its own update script"},
	{"komodo-", "Komodo manages its own deployment"},
	{"portainer", "Portainer manages its own updates"},
}

func addNotes(c *Candidate) {
	name := strings.ToLower(c.Container.Name)
	for _, sm := range selfManaging {
		if strings.HasPrefix(name, sm.prefix) {
			c.Decision.note("%s", sm.what)
			break
		}
	}
	if c.Decision.Policy.Enabled && c.Decision.Policy.Snapshot && len(c.Container.VolumeMounts()) == 0 {
		if len(c.Container.BindMounts()) > 0 && !c.Decision.Policy.IncludeBinds {
			c.Decision.note("stores its data in bind mounts, which are not archived unless %s is set", LabelBinds)
		}
	}
	if c.Decision.Policy.Rollback && !c.Decision.Policy.Snapshot {
		c.Decision.problem("automatic rollback is enabled but snapshots are off, so there would be nothing to roll back to")
	}
}

// Summary counts the outcome of a selection, for a one-line conclusion.
type Summary struct {
	Total        int
	Update       int
	MonitorOnly  int
	Snapshot     int
	Scheduled    int
	NotUpdatable int
	Problems     int
}

// Summarise counts candidates.
func Summarise(candidates []*Candidate) Summary {
	var s Summary
	s.Total = len(candidates)
	for _, c := range candidates {
		if c.WillUpdate() {
			s.Update++
		}
		if c.Decision.Policy.Enabled && c.Decision.Policy.MonitorOnly {
			s.MonitorOnly++
		}
		if c.Decision.Policy.Enabled && c.Decision.Policy.Snapshot {
			s.Snapshot++
		}
		if c.Decision.Policy.Schedule != "" {
			s.Scheduled++
		}
		if c.Decision.Policy.Enabled && c.Updatability != Updatable {
			s.NotUpdatable++
		}
		s.Problems += len(c.Decision.Problems)
	}
	return s
}

func (s Summary) String() string {
	return fmt.Sprintf("%d containers, %d would be updated, %d monitored only, %d scheduled backups",
		s.Total, s.Update, s.MonitorOnly, s.Scheduled)
}
