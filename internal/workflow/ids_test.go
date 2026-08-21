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
	_, ok, err := workflow.CurrentWorkflowID(dir, "sess-1")
	if err != nil {
		t.Fatalf("CurrentWorkflowID: %v", err)
	}
	if ok {
		t.Error("expected ok=false for a session with no workflow files")
	}
}

func TestCurrentWorkflowID_LegacyLiveFile(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-1"
	writeFile(t, dir, sessionID+".workflow.json", "{}")

	id, ok, err := workflow.CurrentWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("CurrentWorkflowID: %v", err)
	}
	if !ok || id != 0 {
		t.Errorf("id=%d ok=%v, want id=0 ok=true", id, ok)
	}
}

func TestCurrentWorkflowID_PicksHighestLiveOverCleared(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-1"
	// ID 0 was cleared; ID 1 is live — current must resolve to 1, not 0.
	writeFile(t, dir, sessionID+".workflow.json.cleared", "{}")
	writeFile(t, dir, sessionID+".workflow.1.json", "{}")

	id, ok, err := workflow.CurrentWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("CurrentWorkflowID: %v", err)
	}
	if !ok || id != 1 {
		t.Errorf("id=%d ok=%v, want id=1 ok=true", id, ok)
	}
}

func TestCurrentWorkflowID_AllClearedNotOK(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess-1"
	writeFile(t, dir, sessionID+".workflow.json.cleared", "{}")
	writeFile(t, dir, sessionID+".workflow.1.json.cleared", "{}")

	_, ok, err := workflow.CurrentWorkflowID(dir, sessionID)
	if err != nil {
		t.Fatalf("CurrentWorkflowID: %v", err)
	}
	if ok {
		t.Error("expected ok=false when every workflow for this session has been cleared")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
}
