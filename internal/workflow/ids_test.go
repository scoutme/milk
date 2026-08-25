package workflow_test

import (
	"os"
	"testing"

	"github.com/scoutme/milk/internal/workflow"
)

func TestNextWorkflowID_EmptySessionIsZero(t *testing.T) {
	dir := t.TempDir()
	got, err := workflow.NextWorkflowID(dir, "sess-1")
	if err != nil {
		t.Fatalf("NextWorkflowID: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0 for a session with no workflow files", got)
	}
}

func TestNextWorkflowID_LegacyFilesReserveIDZero(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-1"
	writeFile(t, dir, sessionID+".workflow.json", "{}")

	got, err := workflow.NextWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("NextWorkflowID: %v", err)
	}
	if got != 1 {
		t.Errorf("got %d, want 1 (legacy file occupies ID 0)", got)
	}
}

func TestNextWorkflowID_ClearedFileStillReservesItsID(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-1"
	writeFile(t, dir, sessionID+".workflow.json.cleared", "{}")

	got, err := workflow.NextWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("NextWorkflowID: %v", err)
	}
	if got != 1 {
		t.Errorf("got %d, want 1 — a cleared state file must not free its ID for reuse", got)
	}
}

func TestNextWorkflowID_SkipsAheadOfHighestArtifactID(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-1"
	// Only a plan/findings file exists for ID 2 (no state file) — the ID must
	// still be treated as used so a new run can't reuse its filenames.
	writeFile(t, dir, sessionID+".workflow.2.findings1.md", "notes")

	got, err := workflow.NextWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("NextWorkflowID: %v", err)
	}
	if got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestNextWorkflowID_UnrelatedSessionIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "other-sess.workflow.5.json", "{}")

	got, err := workflow.NextWorkflowID(dir, "sess-1")
	if err != nil {
		t.Fatalf("NextWorkflowID: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0 — another session's files must not affect this session's ID", got)
	}
}

func TestCurrentWorkflowID_NoFilesNotOK(t *testing.T) {
	dir := t.TempDir()
	_, kind, err := workflow.CurrentWorkflowID(dir, "sess-1")
	if err != nil {
		t.Fatalf("CurrentWorkflowID: %v", err)
	}
	if kind != workflow.WorkflowKindNone {
		t.Errorf("expected WorkflowKindNone for a session with no workflow files, got %v", kind)
	}
}

func TestCurrentWorkflowID_LegacyLiveFile(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-1"
	writeFile(t, dir, sessionID+".workflow.json", "{}")

	id, kind, err := workflow.CurrentWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("CurrentWorkflowID: %v", err)
	}
	if kind != workflow.WorkflowKindDev || id != 0 {
		t.Errorf("id=%d kind=%v, want id=0 kind=Dev", id, kind)
	}
}

func TestCurrentWorkflowID_PicksHighestLiveOverCleared(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-1"
	// ID 0 was cleared; ID 1 is live — current must resolve to 1, not 0.
	writeFile(t, dir, sessionID+".workflow.json.cleared", "{}")
	writeFile(t, dir, sessionID+".workflow.1.json", "{}")

	id, kind, err := workflow.CurrentWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("CurrentWorkflowID: %v", err)
	}
	if kind != workflow.WorkflowKindDev || id != 1 {
		t.Errorf("id=%d kind=%v, want id=1 kind=Dev", id, kind)
	}
}

func TestCurrentWorkflowID_AllClearedNotOK(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-1"
	writeFile(t, dir, sessionID+".workflow.json.cleared", "{}")
	writeFile(t, dir, sessionID+".workflow.1.json.cleared", "{}")

	_, kind, err := workflow.CurrentWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("CurrentWorkflowID: %v", err)
	}
	if kind != workflow.WorkflowKindNone {
		t.Errorf("expected WorkflowKindNone when every workflow for this session has been cleared, got %v", kind)
	}
}

func TestCurrentWorkflowID_InterpCheckpoint(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-1"
	writeFile(t, dir, sessionID+".workflow.interp.json", "{}")

	id, kind, err := workflow.CurrentWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("CurrentWorkflowID: %v", err)
	}
	if kind != workflow.WorkflowKindInterp || id != 0 {
		t.Errorf("id=%d kind=%v, want id=0 kind=Interp", id, kind)
	}
}

// TestNextWorkflowID_DevAndInterpShareIDSpace verifies a dev workflow and a
// later interp-driven workflow in the same session never collide on the
// same ID — InterpCheckpointPath reuses StatePath's naming scheme
// specifically so sessionWorkflowIDs (which NextWorkflowID is built on)
// accounts for both kinds.
func TestNextWorkflowID_DevAndInterpShareIDSpace(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-1"
	writeFile(t, dir, sessionID+".workflow.json", "{}") // dev, ID 0

	next, err := workflow.NextWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("NextWorkflowID: %v", err)
	}
	if next != 1 {
		t.Fatalf("NextWorkflowID after a dev workflow = %d, want 1", next)
	}

	writeFile(t, dir, sessionID+".workflow.1.interp.json", "{}") // interp, ID 1

	next2, err := workflow.NextWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("NextWorkflowID: %v", err)
	}
	if next2 != 2 {
		t.Errorf("NextWorkflowID after a dev workflow (ID 0) + an interp workflow (ID 1) = %d, want 2", next2)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
}
