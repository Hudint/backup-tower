package discover

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Hudint/backup-tower/internal/schedule"
)

// RuleFile is the optional YAML configuration.
//
// Rules exist for the cases labels cannot reach: containers you do not control
// the compose file for, or a policy that should apply to a whole group without
// touching every service definition.
type RuleFile struct {
	// Defaults override the built-in defaults for every container.
	Defaults Settings `yaml:"defaults"`
	// Rules are applied in order; later matches override earlier ones. That
	// makes the natural way to write them broad-first, specific-last.
	Rules []Rule `yaml:"rules"`
	// KomodoTags map Komodo tag names to settings, so a stack can be configured
	// by tagging it in the Komodo UI rather than by editing its compose file.
	KomodoTags []TagRule `yaml:"komodo_tags,omitempty"`
}

// Rule is one conditional block of settings.
type Rule struct {
	// Name is optional and only used to identify the rule in plan output.
	Name  string   `yaml:"name,omitempty"`
	Match Match    `yaml:"match"`
	Set   Settings `yaml:"set"`
}

// Match selects containers. All specified conditions must hold; an empty Match
// matches everything, which is how a broad default rule is written.
type Match struct {
	// Name matches the container name as a glob, e.g. "web-*".
	Name []string `yaml:"name,omitempty"`
	// NameRegex matches the container name as a regular expression.
	NameRegex string `yaml:"name_regex,omitempty"`
	// ComposeProject matches the compose project as a glob.
	ComposeProject []string `yaml:"compose_project,omitempty"`
	// Image matches the configured image reference as a glob, e.g.
	// "ghcr.io/hudint/*".
	Image []string `yaml:"image,omitempty"`
	// Labels requires each of these container labels to have the given value.
	// An empty value matches any value, so presence alone can be tested.
	Labels map[string]string `yaml:"labels,omitempty"`

	regex *regexp.Regexp
}

// Settings mirrors Policy with pointers, so "not mentioned" is distinguishable
// from "set to the zero value". Without that distinction a rule that only wants
// to change one setting would silently reset every other one.
type Settings struct {
	Enable        *bool   `yaml:"enable,omitempty"`
	MonitorOnly   *bool   `yaml:"monitor_only,omitempty"`
	Snapshot      *bool   `yaml:"snapshot,omitempty"`
	Binds         *bool   `yaml:"binds,omitempty"`
	Stop          *string `yaml:"stop,omitempty"`
	Schedule      *string `yaml:"schedule,omitempty"`
	RetentionKeep *int    `yaml:"retention_keep,omitempty"`
	RetentionDays *int    `yaml:"retention_days,omitempty"`
	Rollback      *bool   `yaml:"rollback,omitempty"`
	Strategy      *string `yaml:"strategy,omitempty"`
	Hooks         *Hooks  `yaml:"hooks,omitempty"`
}

// LoadRuleFile reads and validates a rule file. A missing file is not an error:
// running without one is the normal case.
func LoadRuleFile(path string) (*RuleFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &RuleFile{}, nil
		}
		return nil, fmt.Errorf("read rule file %q: %w", path, err)
	}

	var f RuleFile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true) // a misspelled key must not be silently ignored
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse rule file %q: %w", path, err)
	}

	if err := f.compile(); err != nil {
		return nil, fmt.Errorf("rule file %q: %w", path, err)
	}
	return &f, nil
}

func (f *RuleFile) compile() error {
	for i := range f.Rules {
		r := &f.Rules[i]
		if r.Match.NameRegex != "" {
			re, err := regexp.Compile(r.Match.NameRegex)
			if err != nil {
				return fmt.Errorf("%s: name_regex: %w", r.label(i), err)
			}
			r.Match.regex = re
		}
		for _, pattern := range append(append([]string{}, r.Match.Name...), append(r.Match.ComposeProject, r.Match.Image...)...) {
			if _, err := path.Match(pattern, ""); err != nil {
				return fmt.Errorf("%s: %q is not a valid pattern: %w", r.label(i), pattern, err)
			}
		}
		if err := validateSettings(r.Set); err != nil {
			return fmt.Errorf("%s: %w", r.label(i), err)
		}
	}
	for i, tr := range f.KomodoTags {
		if tr.Tag == "" {
			return fmt.Errorf("komodo_tags[%d]: tag must not be empty", i)
		}
		if err := validateSettings(tr.Set); err != nil {
			return fmt.Errorf("komodo_tags[%d] (%s): %w", i, tr.Tag, err)
		}
	}
	return validateSettings(f.Defaults)
}

func (r *Rule) label(i int) string {
	if r.Name != "" {
		return fmt.Sprintf("rule %q", r.Name)
	}
	return fmt.Sprintf("rule[%d]", i)
}

func validateSettings(s Settings) error {
	if s.Stop != nil {
		if _, err := ParseStopPolicy(*s.Stop); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
	}
	if s.Strategy != nil {
		if _, err := ParseStrategy(*s.Strategy); err != nil {
			return fmt.Errorf("strategy: %w", err)
		}
	}
	if s.Schedule != nil && *s.Schedule != "" {
		if _, err := schedule.Parse(*s.Schedule); err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
	}
	if s.RetentionKeep != nil && *s.RetentionKeep < 0 {
		return fmt.Errorf("retention_keep must not be negative")
	}
	if s.RetentionDays != nil && *s.RetentionDays < 0 {
		return fmt.Errorf("retention_days must not be negative")
	}
	return nil
}

// Matches reports whether a container satisfies the match block.
func (m *Match) Matches(name, composeProject, image string, labels map[string]string) bool {
	if len(m.Name) > 0 && !matchesAny(m.Name, name) {
		return false
	}
	if m.regex != nil && !m.regex.MatchString(name) {
		return false
	}
	if len(m.ComposeProject) > 0 && !matchesAny(m.ComposeProject, composeProject) {
		return false
	}
	if len(m.Image) > 0 && !matchesAny(m.Image, image) {
		return false
	}
	for k, want := range m.Labels {
		got, ok := labels[k]
		if !ok {
			return false
		}
		if want != "" && got != want {
			return false
		}
	}
	return true
}

func matchesAny(patterns []string, value string) bool {
	for _, p := range patterns {
		// path.Match treats "/" specially, which is wrong for image references
		// like ghcr.io/hudint/app: a pattern of "ghcr.io/*" would not match the
		// second slash. Matching on the flattened string avoids that surprise.
		if ok, err := path.Match(strings.ReplaceAll(p, "/", "\x00"), strings.ReplaceAll(value, "/", "\x00")); err == nil && ok {
			return true
		}
	}
	return false
}

// apply overlays settings onto a policy, recording where each came from.
func applySettings(p *Policy, s Settings, source, detail string, d *Decision) {
	touched := false
	set := func() { touched = true }

	if s.Enable != nil {
		p.Enabled = *s.Enable
		if *s.Enable {
			d.EnabledBy = &Origin{Source: source, Detail: detail}
		} else {
			d.EnabledBy = nil
		}
		set()
	}
	if s.MonitorOnly != nil {
		p.MonitorOnly = *s.MonitorOnly
		set()
	}
	if s.Snapshot != nil {
		p.Snapshot = *s.Snapshot
		set()
	}
	if s.Binds != nil {
		p.IncludeBinds = *s.Binds
		set()
	}
	if s.Stop != nil {
		if v, err := ParseStopPolicy(*s.Stop); err == nil {
			p.Stop = v
			set()
		}
	}
	if s.Schedule != nil {
		p.Schedule = *s.Schedule
		set()
	}
	if s.RetentionKeep != nil {
		p.RetentionKeep = *s.RetentionKeep
		set()
	}
	if s.RetentionDays != nil {
		p.RetentionDays = *s.RetentionDays
		set()
	}
	if s.Rollback != nil {
		p.Rollback = *s.Rollback
		set()
	}
	if s.Strategy != nil {
		if v, err := ParseStrategy(*s.Strategy); err == nil {
			p.Strategy = v
			set()
		}
	}
	if s.Hooks != nil {
		p.Hooks = *s.Hooks
		set()
	}

	if touched {
		d.record(source, detail)
	}
}
