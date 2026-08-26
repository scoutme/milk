package interp

import (
	"encoding/json"
	"fmt"
	"os"
)

// TraceEntry records one completed "leaf" stage — agent_turn, user_checkpoint,
// or parallel_group (see Checkpoint's doc for why parallel_group is atomic
// here) — for checkpoint/resume. Entries are appended in the exact order
// their stages complete during a live run, which is also the order a
// re-run of the same Definition against the same restored Vars would visit
// them in — so resume only needs to replay this flat sequence positionally,
// without recording which loop iteration or dependency wave produced it.
type TraceEntry struct {
	StageID string `json:"stage_id"`
	SaveAs  string `json:"save_as,omitempty"`
	Value   any    `json:"value,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

// Checkpoint is the on-disk (JSON) record of an interpreter-driven run,
// written incrementally as each leaf stage completes.
//
// parallel_group is checkpointed atomically: one TraceEntry is appended only
// once every item (across every dependency wave) has finished, holding the
// full []ItemResult as Value. An interruption mid-fan-out has no partial
// record — resuming re-runs the entire parallel_group stage from scratch,
// including items that had already finished. This is a deliberate
// simplification: correctly resuming individual in-flight concurrent items
// (skipping finished ones, continuing partially-retried ones) is real
// additional complexity for comparatively little payoff, since a
// parallel_group's items are, by construction, cheaper to redo than a
// sequential sprint loop is to unwind.
type Checkpoint struct {
	DefinitionName string       `json:"definition_name"`
	Task           string       `json:"task"`
	Trace          []TraceEntry `json:"trace"`
	// AgentMap records role -> resolved agent name, so a resume can rebuild
	// the same TurnRunners without re-asking a role-selection wizard. Set via
	// Runner.WithAgentMap before the first Run call that should persist it.
	AgentMap map[string]string `json:"agent_map,omitempty"`
	// Done marks a checkpoint whose run reached the end of the Definition
	// successfully. LoadCheckpointForResume ignores a Done checkpoint (there
	// is nothing left to resume) rather than replaying it as a no-op.
	Done bool `json:"done,omitempty"`
}

// LoadCheckpoint reads and deserialises a Checkpoint from path.
// Returns (nil, nil) if the file does not exist.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SaveCheckpoint serialises c and writes it atomically to path.
func SaveCheckpoint(path string, c *Checkpoint) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadResumableTrace reads path and returns the trace to replay, or nil if
// there is nothing to resume (no file, or the run there already finished).
// definitionName/task must match the checkpoint's — a mismatch means the
// checkpoint belongs to a different run entirely (or the definition was
// swapped out since), and is reported as an error rather than silently
// discarded, since silently ignoring it would re-run turns the user may be
// paying real API cost for a second time without realising why.
func loadResumableTrace(path, definitionName, task string) ([]TraceEntry, error) {
	cp, err := LoadCheckpoint(path)
	if err != nil {
		return nil, fmt.Errorf("workflow: loading checkpoint %s: %w", path, err)
	}
	if cp == nil || cp.Done {
		return nil, nil
	}
	if cp.DefinitionName != definitionName || cp.Task != task {
		return nil, fmt.Errorf("workflow: checkpoint %s is for a different run (definition %q task %q) — expected definition %q task %q",
			path, cp.DefinitionName, cp.Task, definitionName, task)
	}
	return cp.Trace, nil
}
