package runtime

import "testing"

// A listing gives back the image reference a container resolves to — except
// when that image has lost its tags, where the engine substitutes the id. Both
// the rule file's image globs and the update check read the field as a
// reference, so failing to notice the substitution turns a container that is
// perfectly checkable into one that reports "image has no name".
func TestAnImageIDInPlaceOfAReferenceIsRecognised(t *testing.T) {
	const id = "sha256:69a3d64d93bdfd32ff0865c1831d225b0ab3629b5d3b3e75a11f0951fcfe05a2"

	if !summaryGaveTheImageID("69a3d64d93bd", id) {
		t.Error("the short id form was not recognised")
	}
	if !summaryGaveTheImageID(id, id) {
		t.Error("the full id form was not recognised")
	}
	if !summaryGaveTheImageID("", id) {
		t.Error("an empty image field should be treated as missing")
	}

	if summaryGaveTheImageID("b4bz/homer", id) {
		t.Error("a real reference was mistaken for an id")
	}
	if summaryGaveTheImageID("postgres:17-alpine", id) {
		t.Error("a tagged reference was mistaken for an id")
	}
	if summaryGaveTheImageID("ghcr.io/hudint/the-list:latest", id) {
		t.Error("a registry reference was mistaken for an id")
	}
	// A reference that merely starts with hex must not match a different image.
	if summaryGaveTheImageID("deadbeefcafe", id) {
		t.Error("a name was matched against an unrelated id")
	}
}
