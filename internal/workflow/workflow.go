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
	Session  *session.Session
	Runners  map[string]TurnRunner // role name → resolved TurnRunner adapter
	Send     func(tea.Msg)         // TUI message bus
	StateDir string                // directory for state and artefact files
}
