package discover

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImageGlobsMatchAcrossSlashes(t *testing.T) {
	// path.Match refuses to let * cross a slash, which is right for file paths
	// and wrong for image references: "ghcr.io/hudint/*" has to match
	// "ghcr.io/hudint/app", and "ghcr.io/*" has to match the whole thing.
	m := Match{Image: []string{"ghcr.io/hudint/*"}}
	if !m.Matches("x", "", "ghcr.io/hudint/the-list:latest", nil) {
		t.Error("a registry glob did not match an image in that namespace")
	}
	if m.Matches("x", "", "ghcr.io/someone-else/app:latest", nil) {
		t.Error("a registry glob matched an image from a different namespace")
	}

	wide := Match{Image: []string{"ghcr.io/*"}}
	if !wide.Matches("x", "", "ghcr.io/hudint/the-list:latest", nil) {
		t.Error("a wildcard did not cross the second slash of an image reference")
	}
}

func TestMatchRequiresEveryStatedCondition(t *testing.T) {
	m := Match{
		Name:           []string{"web-*"},
		ComposeProject: []string{"infra"},
	}
	if !m.Matches("web-api", "infra", "nginx", nil) {
		t.Error("a container satisfying both conditions did not match")
	}
	if m.Matches("web-api", "other", "nginx", nil) {
		t.Error("a container matched despite belonging to a different project")
	}
	if m.Matches("db", "infra", "nginx", nil) {
		t.Error("a container matched despite a different name")
	}
}

func TestEmptyMatchMatchesEverything(t *testing.T) {
	// This is how a broad default rule is written, so it must not accidentally
	// match nothing.
	var m Match
	if !m.Matches("anything", "", "", nil) {
		t.Error("an empty match block did not match")
	}
}

func TestLabelMatchDistinguishesPresenceFromValue(t *testing.T) {
	labels := map[string]string{"env": "prod", "team": ""}

	if !(&Match{Labels: map[string]string{"env": ""}}).Matches("x", "", "", labels) {
		t.Error("an empty expected value should match any value")
	}
	if !(&Match{Labels: map[string]string{"env": "prod"}}).Matches("x", "", "", labels) {
		t.Error("an exact label value did not match")
	}
	if (&Match{Labels: map[string]string{"env": "staging"}}).Matches("x", "", "", labels) {
		t.Error("a different label value matched")
	}
	if (&Match{Labels: map[string]string{"missing": ""}}).Matches("x", "", "", labels) {
		t.Error("an absent label matched")
	}
}

func TestLaterRulesOverrideEarlierOnes(t *testing.T) {
	// Broad first, specific last is the natural way to write rules, and only
	// works if the last match wins.
	path := writeRules(t, `
rules:
  - name: everything in the stack
    match:
      compose_project: ["tower-test"]
    set:
      enable: true
  - name: except the database
    match:
      name: ["tower-test-db-*"]
    set:
      enable: false
`)
	f, err := LoadRuleFile(path)
	if err != nil {
		t.Fatal(err)
	}

	p := Defaults(3, 14)
	var d Decision
	for i := range f.Rules {
		r := &f.Rules[i]
		if r.Match.Matches("tower-test-db-1", "tower-test", "postgres:17", nil) {
			applySettings(&p, r.Set, "rule", r.label(i), &d)
		}
	}
	if p.Enabled {
		t.Error("the later, more specific rule did not override the earlier one")
	}
	if d.EnabledBy != nil {
		t.Errorf("EnabledBy should have been cleared, got %v", d.EnabledBy)
	}
}

func TestUnsetFieldsLeavePolicyAlone(t *testing.T) {
	// A rule that only changes one setting must not silently reset the rest.
	p := Defaults(3, 14)
	p.Snapshot = false
	p.RetentionKeep = 9

	enable := true
	var d Decision
	applySettings(&p, Settings{Enable: &enable}, "rule", "test", &d)

	if !p.Enabled {
		t.Error("enable was not applied")
	}
	if p.Snapshot {
		t.Error("an unmentioned setting was reset to its zero value")
	}
	if p.RetentionKeep != 9 {
		t.Errorf("RetentionKeep = %d, want 9", p.RetentionKeep)
	}
}

func TestMisspelledKeysAreRejected(t *testing.T) {
	// Silently ignoring an unknown key means a rule that does nothing and says
	// nothing, which is the worst way to learn about a typo.
	path := writeRules(t, `
rules:
  - match:
      nmae: ["web-*"]
    set:
      enable: true
`)
	if _, err := LoadRuleFile(path); err == nil {
		t.Error("a misspelled match key was accepted")
	}
}

func TestInvalidValuesAreRejectedAtLoadTime(t *testing.T) {
	path := writeRules(t, `
rules:
  - match: {}
    set:
      stop: sometimes
`)
	if _, err := LoadRuleFile(path); err == nil {
		t.Error("an invalid stop policy was accepted")
	}
}

func TestMissingRuleFileIsNotAnError(t *testing.T) {
	// Running without a rule file is the normal case.
	f, err := LoadRuleFile(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("a missing rule file was treated as an error: %v", err)
	}
	if len(f.Rules) != 0 {
		t.Error("a missing rule file produced rules")
	}
}

func TestLooksLikeImageID(t *testing.T) {
	if !looksLikeImageID("2e2c3ff3d03f") {
		t.Error("a bare hex image ID was not recognised")
	}
	if !looksLikeImageID("sha256:2e2c3ff3d03fa1b2c3d4e5f6") {
		t.Error("a prefixed image ID was not recognised")
	}
	if looksLikeImageID("postgres") {
		t.Error("a plain image name was mistaken for an ID")
	}
	if looksLikeImageID("adminer") {
		t.Error("an image name of only hex-ish letters was mistaken for an ID")
	}
	if looksLikeImageID("ghcr.io/hudint/app:latest") {
		t.Error("a full image reference was mistaken for an ID")
	}
}

func writeRules(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKomodoTagsMapToSettings(t *testing.T) {
	// A tag is only a name. Mapping tags to settings is what lets the whole
	// policy live in Komodo, with no compose file to edit and no redeploy.
	path := writeRules(t, `
komodo_tags:
  - tag: bt-update
    set:
      enable: true
      monitor_only: false
  - tag: bt-stop
    set:
      stop: always
`)
	f, err := LoadRuleFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.KomodoTags) != 2 {
		t.Fatalf("parsed %d tag rules, want 2", len(f.KomodoTags))
	}
	if f.KomodoTags[0].Tag != "bt-update" || f.KomodoTags[1].Tag != "bt-stop" {
		t.Errorf("tag order lost: %+v", f.KomodoTags)
	}
	if f.KomodoTags[0].Set.Enable == nil || !*f.KomodoTags[0].Set.Enable {
		t.Error("enable was not parsed")
	}
}

func TestKomodoTagRulesAreValidated(t *testing.T) {
	// A tag rule that never applies because of a typo would do nothing and say
	// nothing, which is the worst way to find out.
	if _, err := LoadRuleFile(writeRules(t, "komodo_tags:\n  - tag: \"\"\n    set: {}\n")); err == nil {
		t.Error("an empty tag name was accepted")
	}
	if _, err := LoadRuleFile(writeRules(t, "komodo_tags:\n  - tag: x\n    set:\n      stop: sometimes\n")); err == nil {
		t.Error("an invalid stop policy was accepted inside a tag rule")
	}
}
