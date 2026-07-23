package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
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
}

// VerdictEntry records the evaluator's verdict for one sprint/pass pair.
type VerdictEntry struct {
	Sprint  int    `json:"sprint"`
	Pass    int    `json:"pass"`
	Verdict string `json:"verdict"`
}

// StatePath returns the canonical path for a session's workflow state file.
func StatePath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, sessionID+".workflow.json")
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
