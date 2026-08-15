package discover

import "testing"

// A source that only says "this one is included" must not clear settings it
// never mentioned.
//
// This was once a real regression: an opt-in mechanism was expressed as
// enable+monitor_only rather than enable alone, which silently cleared a
// deliberate watch-only rule. A stack that had been merely observed for weeks
// would have started updating itself on the next pass, with nothing in the
// output saying so.
func TestMembershipDoesNotOverrideWatchOnly(t *testing.T) {
	enable := true
	watch := true

	p := Defaults(3, 14)
	var d Decision

	// A broad rule puts the container under observation.
	applySettings(&p, Settings{Enable: &enable, MonitorOnly: &watch}, "rule", "watch own images", &d)
	// A later, narrower rule says only "this one is included".
	applySettings(&p, Settings{Enable: &enable}, "rule", "include this stack", &d)

	if !p.Enabled {
		t.Error("the later rule did not enable the container")
	}
	if !p.MonitorOnly {
		t.Error("a membership-only rule cleared monitor_only and turned observation into updates")
	}
}

// A source that explicitly says so may of course clear it.
func TestExplicitSettingCanTurnOffWatchOnly(t *testing.T) {
	enable, watch, off := true, true, false

	p := Defaults(3, 14)
	var d Decision
	applySettings(&p, Settings{Enable: &enable, MonitorOnly: &watch}, "rule", "watch own images", &d)
	applySettings(&p, Settings{Enable: &enable, MonitorOnly: &off}, "rule", "update this stack", &d)

	if p.MonitorOnly {
		t.Error("a rule that explicitly sets monitor_only=false was ignored")
	}
}

// And the same holds for a label, which is the last word of all.
func TestLabelCanTurnOffWatchOnly(t *testing.T) {
	enable, watch := true, true

	p := Defaults(3, 14)
	var d Decision
	applySettings(&p, Settings{Enable: &enable, MonitorOnly: &watch}, "rule", "watch own images", &d)
	applyLabels(&p, map[string]string{LabelMonitorOnly: "false"}, &d)

	if p.MonitorOnly {
		t.Error("a label did not override the rule's monitor_only")
	}
	if !p.Enabled {
		t.Error("the label reset a setting it never mentioned")
	}
}
