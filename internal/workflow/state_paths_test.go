package workflow_test

import (
	"testing"

	"github.com/scoutme/milk/internal/workflow"
)

// TestPathHelpers_IDZeroMatchesLegacyNaming verifies that workflow ID 0
// reproduces the exact suffix-less filenames used before per-workflow IDs
// existed, so every pre-existing on-disk session keeps working unchanged.
func TestPathHelpers_IDZeroMatchesLegacyNaming(t *testing.T) {
	dir := "/tmp/sessions"
	sessionID := "sess-1"

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"state", workflow.StatePath(dir, sessionID, 0), dir + "/sess-1.workflow.json"},
		{"plan", workflow.PlanPath(dir, sessionID, 0), dir + "/sess-1.workflow.plan.md"},
		{"sprint", workflow.SprintPath(dir, sessionID, 0, 3), dir + "/sess-1.workflow.sprint3.md"},
		{"findings", workflow.FindingsPath(dir, sessionID, 0, 3), dir + "/sess-1.workflow.findings3.md"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestPathHelpers_NonZeroIDInsertsSuffix verifies that workflow IDs beyond
// the session's first workflow get a distinct, non-colliding filename.
func TestPathHelpers_NonZeroIDInsertsSuffix(t *testing.T) {
	dir := "/tmp/sessions"
	sessionID := "sess-1"

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"state", workflow.StatePath(dir, sessionID, 2), dir + "/sess-1.workflow.2.json"},
		{"plan", workflow.PlanPath(dir, sessionID, 2), dir + "/sess-1.workflow.2.plan.md"},
		{"sprint", workflow.SprintPath(dir, sessionID, 2, 3), dir + "/sess-1.workflow.2.sprint3.md"},
		{"findings", workflow.FindingsPath(dir, sessionID, 2, 3), dir + "/sess-1.workflow.2.findings3.md"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
