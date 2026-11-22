package setup

import "testing"

func TestConfigChangesListsOnlyChangedFields(t *testing.T) {
	before := []byte("version: 1\nrunner:\n  max_concurrent: 2\nimage:\n  ubuntu:\n    amd64:\n      snapshot: old\n")
	after := []byte("version: 1\nrunner:\n  max_concurrent: 4\n  execute: true\n")
	changes, err := configChanges(before, after)
	if err != nil {
		t.Fatal(err)
	}
	want := "image.ubuntu.amd64.snapshot  old → (unset)\nrunner.execute  (unset) → true\nrunner.max_concurrent  2 → 4\n"
	if changes != want {
		t.Fatalf("changes = %q, want %q", changes, want)
	}
}
