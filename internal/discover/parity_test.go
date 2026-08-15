package discover

import (
	"testing"
)

func ptr[T any](v T) *T { return &v }

// equivalents pairs every label with the rule-file setting that must do exactly
// the same thing.
//
// There are two ways to configure a container and they are not a hierarchy of
// capability: the rule file is for containers whose compose file is not yours to
// edit, a label is for when it is, and neither may be able to express something
// the other cannot. A setting reachable from only one of them is a trap — it
// works until the day you need it from the other side.
var equivalents = []struct {
	label      string
	labelValue string
	setting    Settings
}{
	{LabelEnable, "true", Settings{Enable: ptr(true)}},
	{LabelMonitorOnly, "true", Settings{MonitorOnly: ptr(true)}},
	{LabelSnapshot, "false", Settings{Snapshot: ptr(false)}},
	{LabelBinds, "true", Settings{Binds: ptr(true)}},
	{LabelStop, "always", Settings{Stop: ptr("always")}},
	{LabelSchedule, "0 3 * * *", Settings{Schedule: ptr("0 3 * * *")}},
	{LabelRetentionKeep, "7", Settings{RetentionKeep: ptr(7)}},
	{LabelRetentionDays, "30", Settings{RetentionDays: ptr(30)}},
	{LabelRollback, "true", Settings{Rollback: ptr(true)}},
	{LabelStrategy, "recreate", Settings{Strategy: ptr("recreate")}},
	{LabelHookPreSnap, "pg_dump -U app", Settings{Hooks: &Hooks{PreSnapshot: "pg_dump -U app"}}},
	{LabelHookPreUpdate, "touch /tmp/pre", Settings{Hooks: &Hooks{PreUpdate: "touch /tmp/pre"}}},
	{LabelHookPostUpd, "touch /tmp/post", Settings{Hooks: &Hooks{PostUpdate: "touch /tmp/post"}}},
}

func TestEverySettingIsReachableFromBothSources(t *testing.T) {
	for _, e := range equivalents {
		t.Run(e.label, func(t *testing.T) {
			viaLabel := Defaults(3, 14)
			var d1 Decision
			applyLabels(&viaLabel, map[string]string{e.label: e.labelValue}, &d1)

			viaRule := Defaults(3, 14)
			var d2 Decision
			applySettings(&viaRule, e.setting, "rule", "test", &d2)

			if viaLabel != viaRule {
				t.Errorf("the label and the rule produced different policies:\n label: %+v\n rule:  %+v", viaLabel, viaRule)
			}
			if len(d1.Problems) > 0 {
				t.Errorf("the label was rejected: %v", d1.Problems)
			}
			// A setting that changed nothing would make this test pass while
			// proving nothing at all.
			if viaLabel == Defaults(3, 14) {
				t.Errorf("%s did not change the policy, so this comparison is vacuous", e.label)
			}
		})
	}
}

// TestEveryKnownLabelIsCovered fails when a label is added without a rule-file
// counterpart being proven alongside it.
func TestEveryKnownLabelIsCovered(t *testing.T) {
	covered := make(map[string]bool, len(equivalents))
	for _, e := range equivalents {
		covered[e.label] = true
	}
	for label := range knownLabels {
		if !covered[label] {
			t.Errorf("%s has no rule-file equivalent in this test; add one, and add the setting if it does not exist", label)
		}
	}
}

// TestLabelsOverrideRules pins the precedence down. A label sits on the object
// being acted upon, so it is the most specific statement of intent available and
// has to win against a rule that merely matched it.
func TestLabelsOverrideRules(t *testing.T) {
	p := Defaults(3, 14)
	var d Decision

	// A rule enables the container and asks for automatic rollback.
	applySettings(&p, Settings{Enable: ptr(true), Rollback: ptr(true)}, "rule", "broad", &d)
	if !p.Enabled || !p.Rollback {
		t.Fatal("the rule did not apply")
	}

	// The container itself says no.
	applyLabels(&p, map[string]string{LabelEnable: "false"}, &d)

	if p.Enabled {
		t.Error("a label did not override the rule that enabled the container")
	}
	if d.EnabledBy != nil {
		t.Errorf("EnabledBy should have been cleared, got %v", d.EnabledBy)
	}
	if !p.Rollback {
		t.Error("the label reset a setting it never mentioned")
	}
}
