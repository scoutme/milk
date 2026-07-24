package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const directBashTimeout = 30 * time.Second

// handleDirectBashKey handles y/N keypresses while pendingDirectBash is set.
// 'y' or Enter runs the command; anything else (including 'n', Esc, Ctrl+C)
// routes the original input to the agent as plain text.
func (m model) handleDirectBashKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	cmd := *m.pendingDirectBash
	m.pendingDirectBash = nil

	switch msg.String() {
	case "y", "Y", "enter":
		m.appendTranscript("y\n")
		m.busy = true
		m.spinnerFrame = 0
		return m, tea.Batch(spinnerTick(), m.execDirectBash(cmd))
	default:
		// User declined — route to agent as normal input.
		m.appendTranscript("n\n")
		m.refreshPrompt()
		return m.dispatchAgent(cmd)
	}
}

// execDirectBash returns a Cmd that runs shellCmd via sh -c, streams the output
// to the transcript, and shows the exit code if non-zero. Does NOT forward to agent.
func (m model) execDirectBash(shellCmd string) tea.Cmd {
	send := func(msg tea.Msg) {
		if m.st.program != nil {
			m.st.program.Send(msg)
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), directBashTimeout)
		defer cancel()

		var buf bytes.Buffer
		c := exec.CommandContext(ctx, "sh", "-c", shellCmd)
		c.Stdout = &buf
		c.Stderr = &buf

		err := c.Run()

		output := buf.String()
		if output != "" && !strings.HasSuffix(output, "\n") {
			output += "\n"
		}

		var sb strings.Builder
		if output != "" {
			sb.WriteString(dim(output))
		}
		if err != nil {
			if ctx.Err() != nil {
				sb.WriteString(fmt.Sprintf("%s direct-bash: command timed out after %s\n", milkTag(), directBashTimeout))
			} else if exitErr, ok := err.(*exec.ExitError); ok {
				sb.WriteString(fmt.Sprintf("%s exit %d\n", dim("[sh]"), exitErr.ExitCode()))
			} else {
				sb.WriteString(fmt.Sprintf("%s direct-bash error: %v\n", milkTag(), err))
			}
		}

		text := sb.String()
		send(prefixChunkMsg{text: text})
		return agentDoneMsg{err: nil}
	}
}
