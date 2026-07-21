package workflow

import (
	"context"
	"io"

	tea "github.com/charmbracelet/bubbletea"
)

// WorkflowChunkMsg carries a chunk of streamed workflow-stage output to the TUI.
// cmd/milk handles this type in its Update loop (case WorkflowChunkMsg).
type WorkflowChunkMsg struct{ Text string }

// workflowSendWriter forwards each Write as a WorkflowChunkMsg via send.
type workflowSendWriter struct {
	send func(tea.Msg)
}

func (w *workflowSendWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.send(WorkflowChunkMsg{Text: string(p)})
	}
	return len(p), nil
}

// Turn invokes one agent turn for a workflow stage, streams output to the TUI
// via send, and returns the agent's plain-text response.
// If send is nil, output is discarded.
func Turn(ctx context.Context, r TurnRunner, prompt string, send func(tea.Msg)) (string, error) {
	var out io.Writer
	if send != nil {
		out = &workflowSendWriter{send: send}
	} else {
		out = io.Discard
	}
	text, err := r.Run(ctx, prompt, out)
	if err != nil {
		return "", err
	}
	return text, nil
}
