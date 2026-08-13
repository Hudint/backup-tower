// Package discover decides which containers backup-tower acts on, and with what
// settings.
//
// Auto-update is strictly opt-in. There is no built-in blocklist: which
// containers are safe to update is the operator's call, not the tool's. What the
// package does instead is make the decision visible — every effective setting
// records where it came from, so `plan` can explain itself rather than just
// assert.
package discover

import (
	"fmt"
	"strings"

	"github.com/Hudint/backup-tower/internal/snapshot"
)

// Strategy selects how an update is applied.
type Strategy string

const (
	// StrategyAuto picks compose when the container came from a compose file
	// that is readable, and falls back to recreating it through the API.
	StrategyAuto Strategy = "auto"
	// StrategyCompose runs `docker compose up -d` for the service.
	StrategyCompose Strategy = "compose"
	// StrategyRecreate rebuilds the container from its inspected spec.
	StrategyRecreate Strategy = "recreate"
)

// Hooks are commands executed inside the container around an update.
type Hooks struct {
	PreSnapshot string `yaml:"pre_snapshot,omitempty"`
	PreUpdate   string `yaml:"pre_update,omitempty"`
	PostUpdate  string `yaml:"post_update,omitempty"`
}

// Policy is the effective configuration for one container.
type Policy struct {
	// Enabled opts the container into automatic updates.
	Enabled bool
	// MonitorOnly checks for updates and reports them without applying any.
	MonitorOnly bool

	// Snapshot takes a snapshot before updating.
	Snapshot bool
	// IncludeBinds archives bind-mounted host paths as well.
	IncludeBinds bool
	// Stop decides whether the container is stopped while its data is read.
	Stop snapshot.StopPolicy

	// Schedule is a cron expression for backups independent of updates.
	Schedule string

	RetentionKeep int
	RetentionDays int

	// Rollback undoes an update automatically when the health gate fails.
	Rollback bool

	Strategy Strategy
	Hooks    Hooks
}

// Defaults returns the policy a container gets when nothing says otherwise.
// Every default is the cautious choice: no updates, no rollback magic, no
// downtime that was not asked for.
func Defaults(retentionKeep, retentionDays int) Policy {
	return Policy{
		Enabled:       false,
		MonitorOnly:   false,
		Snapshot:      true,
		IncludeBinds:  false,
		Stop:          snapshot.StopAuto,
		RetentionKeep: retentionKeep,
		RetentionDays: retentionDays,
		Rollback:      false,
		Strategy:      StrategyAuto,
	}
}

// Origin records where a setting came from, so a decision can be explained
// rather than merely asserted.
type Origin struct {
	// Source is a short identifier: "default", "config", "rule[2]", "komodo",
	// or "label".
	Source string
	// Detail names the specific setting or rule involved.
	Detail string
}

func (o Origin) String() string {
	if o.Detail == "" {
		return o.Source
	}
	return o.Source + ": " + o.Detail
}

// Decision is the outcome for one container.
type Decision struct {
	Policy Policy
	// EnabledBy records what turned automatic updates on, empty when nothing
	// did. This is the single most important thing to be able to explain.
	EnabledBy *Origin
	// Origins lists every source that contributed, in application order.
	Origins []Origin
	// Problems are configuration mistakes: an unparseable label, an unknown
	// setting. They are reported rather than ignored, because a typo in the
	// label that was meant to protect a container is exactly the kind of
	// mistake that stays invisible until it matters.
	Problems []string
	// Notes are advisory only and never change behaviour.
	Notes []string
}

func (d *Decision) note(format string, args ...any) {
	d.Notes = append(d.Notes, fmt.Sprintf(format, args...))
}

func (d *Decision) problem(format string, args ...any) {
	d.Problems = append(d.Problems, fmt.Sprintf(format, args...))
}

func (d *Decision) record(source, detail string) {
	d.Origins = append(d.Origins, Origin{Source: source, Detail: detail})
}

// ParseStrategy validates a strategy name.
func ParseStrategy(s string) (Strategy, error) {
	switch Strategy(strings.ToLower(strings.TrimSpace(s))) {
	case StrategyAuto:
		return StrategyAuto, nil
	case StrategyCompose:
		return StrategyCompose, nil
	case StrategyRecreate:
		return StrategyRecreate, nil
	default:
		return "", fmt.Errorf("must be auto, compose or recreate, got %q", s)
	}
}

// ParseStopPolicy validates a stop policy name.
func ParseStopPolicy(s string) (snapshot.StopPolicy, error) {
	switch snapshot.StopPolicy(strings.ToLower(strings.TrimSpace(s))) {
	case snapshot.StopAuto:
		return snapshot.StopAuto, nil
	case snapshot.StopAlways:
		return snapshot.StopAlways, nil
	case snapshot.StopNever:
		return snapshot.StopNever, nil
	default:
		return "", fmt.Errorf("must be auto, always or never, got %q", s)
	}
}
