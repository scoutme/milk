package workflow_test

import (
	"context"
	"io"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/workflow"
)

type mockRunner struct {
	name     string
	response string
	err      error
}

func (r *mockRunner) Name() string { return r.name }
func (r *mockRunner) Run(_ context.Context, _ string, out io.Writer) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	_, _ = out.Write([]byte(r.response))
	return r.response, nil
}

func TestTurn_ReturnsResponse(t *testing.T) {
	runner := &mockRunner{name: "test", response: "hello from agent"}
	got, err := workflow.Turn(context.Background(), runner, "prompt", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello from agent" {
		t.Errorf("got %q, want %q", got, "hello from agent")
	}
}

func TestTurn_SendCalled(t *testing.T) {
	runner := &mockRunner{name: "test", response: "output text"}
	var msgs []tea.Msg
	send := func(msg tea.Msg) { msgs = append(msgs, msg) }

	_, err := workflow.Turn(context.Background(), runner, "prompt", send)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) == 0 {
		t.Error("expected at least one message sent to TUI, got none")
	}
	found := false
	for _, msg := range msgs {
		if chunk, ok := msg.(workflow.WorkflowChunkMsg); ok && chunk.Text == "output text" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected WorkflowChunkMsg{Text: %q} in sent messages %v", "output text", msgs)
	}
}
