package workflow

import (
	"context"
	"io"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/session"
)

// TurnRunner is the simplified interface workflow stages use to invoke an agent.
// cmd/milk wraps its TurnRunner implementations into this interface when building
// RunConfig, capturing config/session/memory context in the adapter so workflow
// code stays free of those dependencies.
type TurnRunner interface {
	Name() string
	// Run executes one prompt turn, streams output to out, and returns the
	// agent's plain-text response. Each role must manage its own session ID
	// across passes (resume within the same role) via the adapter.
	Run(ctx context.Context, prompt string, out io.Writer) (string, error)
}

// Workflow is a named multi-agent pipeline.
type Workflow interface {
	Name() string
	// Run executes the workflow to completion or until an unrecoverable error.
	Run(ctx context.Context, cfg RunConfig) error
}

// RunConfig carries everything a workflow stage needs from the outside.
type RunConfig struct {
	Session    *session.Session
	Runners    map[string]TurnRunner // role name → resolved TurnRunner adapter
	Send       func(tea.Msg)         // TUI message bus
	StateDir   string                // directory for state and artefact files
	WorkflowID int                   // disambiguates multiple workflow runs within one session; see workflow.NextWorkflowID

	// Designer disambiguation: when set, the designer sends questions via
	// cfg.Send and waits for user answers on AnswersCh before finalizing.
	AnswersCh chan string
}

// WorkflowQuestionsMsg is sent to the TUI when the designer identifies
// ambiguities that need user input before proceeding.
type WorkflowQuestionsMsg struct {
	Questions string        // raw designer output with ## Questions and ## Defaults
	AnswersCh chan<- string // channel to send user answers back to the workflow goroutine
}

// StageNode is one node in the tree of currently-active execution paths.
// Each parallel item or loop iteration is a node; siblings under the same
// parent represent concurrent or sequential sub-paths. The tree is rebuilt
// from scratch on each ProgressMsg so it always reflects the live state.
type StageNode struct {
	Label    string       `json:"label"`
	Children []*StageNode `json:"children,omitempty"`
}

// PathSnapshot carries the full tree of active execution paths. The panel
// renders it as an indented tree where the deepest path (last child at each
// level) is highlighted.
type PathSnapshot struct {
	Root *StageNode `json:"root"`
}

// ProgressMsg is sent to the TUI after each state transition by an
// interpreter-driven run (internal/workflow/interp). ActivePaths carries
// the full tree of currently active execution paths (loops, parallel
// items), and Role is the role of the turn in flight, if any (empty
// between turns, e.g. while a loop decides whether to continue).
type ProgressMsg struct {
	WorkflowName   string
	Task           string
	WorkflowID     int
	ActivePaths    *PathSnapshot
	CompletedPaths *PathSnapshot
	Role           string
}

// WorkflowDoneMsg is sent to the TUI when a workflow run ends (success or error).
type WorkflowDoneMsg struct {
	Err error
}
