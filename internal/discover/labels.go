package discover

import (
	"sort"
	"strconv"
	"strings"

	"github.com/Hudint/backup-tower/internal/schedule"
)

// LabelPrefix is the only namespace backup-tower reads.
//
// Labels belonging to other tools — Watchtower's in particular — are
// deliberately ignored. Acting on them would mean touching containers that were
// marked for something else entirely, which is not a decision this tool gets to
// make on the operator's behalf.
const LabelPrefix = "tower."

// Label names, kept in one place so the plan output and the docs cannot drift
// from what is actually read.
const (
	LabelEnable        = "tower.enable"
	LabelMonitorOnly   = "tower.monitor-only"
	LabelSnapshot      = "tower.snapshot"
	LabelBinds         = "tower.snapshot.binds"
	LabelStop          = "tower.snapshot.stop"
	LabelSchedule      = "tower.schedule"
	LabelRetentionKeep = "tower.retention.keep"
	LabelRetentionDays = "tower.retention.days"
	LabelRollback      = "tower.rollback"
	LabelStrategy      = "tower.strategy"
	LabelHookPreSnap   = "tower.hook.pre-snapshot"
	LabelHookPreUpdate = "tower.hook.pre-update"
	LabelHookPostUpd   = "tower.hook.post-update"
)

var knownLabels = map[string]bool{
	LabelEnable:        true,
	LabelMonitorOnly:   true,
	LabelSnapshot:      true,
	LabelBinds:         true,
	LabelStop:          true,
	LabelSchedule:      true,
	LabelRetentionKeep: true,
	LabelRetentionDays: true,
	LabelRollback:      true,
	LabelStrategy:      true,
	LabelHookPreSnap:   true,
	LabelHookPreUpdate: true,
	LabelHookPostUpd:   true,
}

// applyLabels overlays container labels onto a policy. Labels have the last
// word: they sit on the object being acted upon, so they are the most specific
// statement of intent available.
func applyLabels(p *Policy, labels map[string]string, d *Decision) {
	// Sorted for stable output; a plan that reorders itself between runs is
	// harder to diff than one that does not.
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if strings.HasPrefix(k, LabelPrefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := labels[k]
		if !knownLabels[k] {
			// A typo in the label that was meant to enable or protect a
			// container does nothing at all, and silence is the worst possible
			// response to that.
			d.problem("unknown label %s (value %q) — check the spelling", k, v)
			continue
		}

		switch k {
		case LabelEnable:
			if b, ok := parseBool(v, k, d); ok {
				p.Enabled = b
				if b {
					d.EnabledBy = &Origin{Source: "label", Detail: k}
				} else {
					d.EnabledBy = nil
				}
			}
		case LabelMonitorOnly:
			if b, ok := parseBool(v, k, d); ok {
				p.MonitorOnly = b
			}
		case LabelSnapshot:
			if b, ok := parseBool(v, k, d); ok {
				p.Snapshot = b
			}
		case LabelBinds:
			if b, ok := parseBool(v, k, d); ok {
				p.IncludeBinds = b
			}
		case LabelRollback:
			if b, ok := parseBool(v, k, d); ok {
				p.Rollback = b
			}
		case LabelStop:
			if s, err := ParseStopPolicy(v); err != nil {
				d.problem("label %s: %v", k, err)
			} else {
				p.Stop = s
			}
		case LabelStrategy:
			if s, err := ParseStrategy(v); err != nil {
				d.problem("label %s: %v", k, err)
			} else {
				p.Strategy = s
			}
		case LabelSchedule:
			expr := strings.TrimSpace(v)
			if expr != "" {
				// Validate here rather than at fire time: a schedule that never
				// runs because of a typo is indistinguishable from one that was
				// never set.
				if _, err := schedule.Parse(expr); err != nil {
					d.problem("label %s: %v — no scheduled backup will run", k, err)
					break
				}
			}
			p.Schedule = expr
		case LabelRetentionKeep:
			if n, ok := parseInt(v, k, d); ok {
				p.RetentionKeep = n
			}
		case LabelRetentionDays:
			if n, ok := parseInt(v, k, d); ok {
				p.RetentionDays = n
			}
		case LabelHookPreSnap:
			p.Hooks.PreSnapshot = v
		case LabelHookPreUpdate:
			p.Hooks.PreUpdate = v
		case LabelHookPostUpd:
			p.Hooks.PostUpdate = v
		}
	}

	if len(keys) > 0 {
		d.record("label", strings.Join(keys, ", "))
	}
}

func parseBool(v, label string, d *Decision) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	default:
		d.problem("label %s: must be true or false, got %q — leaving it unset", label, v)
		return false, false
	}
}

func parseInt(v, label string, d *Decision) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		d.problem("label %s: must be a number, got %q — leaving it unset", label, v)
		return 0, false
	}
	if n < 0 {
		d.problem("label %s: must not be negative, got %d — leaving it unset", label, n)
		return 0, false
	}
	return n, true
}
