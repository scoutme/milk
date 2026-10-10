package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/scoutme/milk/internal/livebuf"
)

// State is the persisted workflow checkpoint written after each role completes.
// Role records the next role to run on resume:
//   - "generator" → generator has not yet run for this sprint/pass
//   - "evaluator" → generator completed; evaluator has not yet run
type State struct {
	WorkflowName   string            `json:"workflow_name"`
	Task           string            `json:"task,omitempty"`
	Sprint         int               `json:"sprint"`
	Pass           int               `json:"pass"`
	Role           string            `json:"role"`
	VerdictHistory []VerdictEntry    `json:"verdict_history"`
	AgentMap       map[string]string `json:"agent_map"` // role → resolved agent name
	// TotalSprints is the sprint count derived from the designer's plan, once
	// known. 0 means not yet known (e.g. still in the designer role) or a
	// state file saved before this field existed — callers should fall back
	// to showing just Sprint in that case, not "sprint N/0".
	TotalSprints int `json:"total_sprints,omitempty"`
	// WorkflowID identifies which of this session's workflow runs this state
	// belongs to (see NextWorkflowID/CurrentWorkflowID); 0 for the session's
	// first workflow, including every state file saved before this field
	// existed. Carried in-memory so a run can be rebuilt (e.g. the
	// passes-exhausted "continue?" prompt) without re-resolving the ID.
	WorkflowID int `json:"workflow_id,omitempty"`
	// StageTree is set (in-memory only — dev.go never persists it) by an
	// interpreter-driven run at launch from the workflow definition. See
	// ProgressMsg's doc.
	StageTree *StageNode `json:"-"`
	// ActiveStageTree is set (in-memory only) from ProgressMsg.ActivePaths and
	// marks the currently executing path inside StageTree.
	ActiveStageTree *StageNode `json:"-"`
	// CompletedStageTree is set (in-memory only) from ProgressMsg.CompletedPaths
	// and tracks dynamic branches, such as completed parallel fanout items, that
	// should remain visible after they are no longer active.
	CompletedStageTree *StageNode `json:"-"`
	// Generic marks a State populated by an interpreter-driven run (via
	// ProgressMsg) rather than dev.go's own Sprint/Pass/TotalSprints/
	// VerdictHistory reporting — panel rendering branches on this rather
	// than on StageTree's emptiness, since a top-level (not-yet-nested-in-
	// a-loop) stage has an empty StageTree too.
	Generic bool `json:"-"`
	// ParallelExec is set at launch when the definition contains a
	// parallel_group, whose workers don't stream live output; the attach view
	// shows a note about it.
	ParallelExec bool `json:"-"`
	// Live accumulates this run's streamed stage output (see
	// internal/livebuf) for the TUI's attach view (ADR-0047), kept off the
	// main session transcript. In-memory only, like ActiveStageTree — never
	// persisted, never fed back into a resumed run's prompt. Set once when
	// the State is created and never reassigned, so it's safe to read
	// concurrently with the workflow goroutine's writes without any lock
	// beyond livebuf.Buffer's own.
	Live *livebuf.Buffer `json:"-"`
}

// LiveBuffer returns s.Live, lazily creating it on first use so callers never
// need a separate nil check. The buffer, once created, is never replaced.
func (s *State) LiveBuffer() *livebuf.Buffer {
	if s.Live == nil {
		s.Live = livebuf.New(0)
	}
	return s.Live
}

// VerdictEntry records the evaluator's verdict for one sprint/pass pair.
type VerdictEntry struct {
	Sprint  int    `json:"sprint"`
	Pass    int    `json:"pass"`
	Verdict string `json:"verdict"`
}

// workflowFileName builds a workflow artefact filename for the given session
// and workflow ID. workflowID 0 is the original, single-workflow-per-session
// naming scheme (no ID segment) — this keeps every pre-existing on-disk file
// working unchanged; only the second and later workflow runs in a session
// (workflowID >= 1) get a ".<id>." segment, so they never collide with an
// earlier run's files.
func workflowFileName(sessionID string, workflowID int, suffix string) string {
	if workflowID == 0 {
		return sessionID + ".workflow." + suffix
	}
	return fmt.Sprintf("%s.workflow.%d.%s", sessionID, workflowID, suffix)
}

// StatePath returns the canonical path for a session's workflow state file.
func StatePath(stateDir, sessionID string, workflowID int) string {
	return filepath.Join(stateDir, workflowFileName(sessionID, workflowID, "json"))
}

// PlanPath returns the canonical path for a workflow's designer plan file.
func PlanPath(stateDir, sessionID string, workflowID int) string {
	return filepath.Join(stateDir, workflowFileName(sessionID, workflowID, "plan.md"))
}

// SprintPath returns the canonical path for a sprint's generator output file.
func SprintPath(stateDir, sessionID string, workflowID, sprint int) string {
	return filepath.Join(stateDir, workflowFileName(sessionID, workflowID, fmt.Sprintf("sprint%d.md", sprint)))
}

// FindingsPath returns the canonical path for a sprint's evaluator findings file.
func FindingsPath(stateDir, sessionID string, workflowID, sprint int) string {
	return filepath.Join(stateDir, workflowFileName(sessionID, workflowID, fmt.Sprintf("findings%d.md", sprint)))
}

// InterpCheckpointPath returns the canonical path for an interpreter-driven
// workflow's checkpoint file (see internal/workflow/interp.Runner.WithCheckpoint).
// Uses the same "<sessionID>.workflow.<id>." naming scheme as StatePath so
// NextWorkflowID/sessionWorkflowIDs already account for it — an interp-driven
// run and a dev run in the same session never collide on the same ID.
func InterpCheckpointPath(stateDir, sessionID string, workflowID int) string {
	return filepath.Join(stateDir, workflowFileName(sessionID, workflowID, "interp.json"))
}

// LoadState reads and deserialises a State from path.
// Returns (nil, nil) if the file does not exist.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SaveState serialises s and writes it atomically to path.
func SaveState(path string, s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ApplyProgress folds a ProgressMsg into s in place, so fields set once at
// launch (AgentMap, the full StageTree) survive every later progress update.
// Shared by every host that renders workflow progress (TUI panel, ACP plan).
func (s *State) ApplyProgress(msg ProgressMsg) {
	s.WorkflowName = msg.WorkflowName
	s.Task = msg.Task
	s.WorkflowID = msg.WorkflowID
	s.Role = msg.Role
	if msg.ActivePaths != nil {
		s.ActiveStageTree = msg.ActivePaths.Root
	}
	if msg.CompletedPaths != nil {
		s.CompletedStageTree = msg.CompletedPaths.Root
	}
	s.Generic = true
}
