package discover

import "testing"

// A Komodo tag that merely marks membership must not override a deliberate
// watch-only rule. Expressing KOMODO_TAG as a tag rule once set monitor_only
// alongside enable, which silently widened what got updated — a stack that had
// been watched for weeks would have started updating itself on the next pass.
func TestMembershipTagDoesNotOverrideWatchOnly(t *testing.T) {
	enable := true
	watch := true

	p := Defaults(3, 14)
	var d Decision

	// A broad rule puts the container under observation.
	applySettings(&p, Settings{Enable: &enable, MonitorOnly: &watch}, "rule", "watch own images", &d)
	// The membership tag then says only "this one is included".
	applySettings(&p, Settings{Enable: &enable}, "komodo tag", "slow-update", &d)

	if !p.Enabled {
		t.Error("the tag did not enable the container")
	}
	if !p.MonitorOnly {
		t.Error("a membership tag cleared monitor_only and turned observation into updates")
	}
}

// A tag that explicitly says so may of course clear it.
func TestPolicyTagCanTurnOffWatchOnly(t *testing.T) {
	enable, watch, off := true, true, false

	p := Defaults(3, 14)
	var d Decision
	applySettings(&p, Settings{Enable: &enable, MonitorOnly: &watch}, "rule", "watch own images", &d)
	applySettings(&p, Settings{Enable: &enable, MonitorOnly: &off}, "komodo tag", "bt-update", &d)

	if p.MonitorOnly {
		t.Error("a tag that explicitly sets monitor_only=false was ignored")
	}
}
