package eval

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func tmuxNewSession(name string, width, height int) error {
	return exec.Command("tmux", "new-session", "-d", "-s", name,
		"-x", fmt.Sprint(width), "-y", fmt.Sprint(height)).Run()
}

func tmuxKillSession(name string) error {
	return exec.Command("tmux", "kill-session", "-t", name).Run()
}

// tmuxSendKeys sends literal text to the pane. Trailing newlines are stripped:
// YAML folded scalars (">") append a trailing "\n", and sending that via "-l"
// inserts a literal newline into the target textarea instead of submitting —
// TUIs like Claude Code and milk require an explicit Enter key event (sent
// separately via tmuxSendEnter) to submit, so a trailing "\n" here just leaves
// the prompt sitting unsubmitted and the adapter hangs waiting for a response
// that never starts.
func tmuxSendKeys(session, text string) error {
	text = strings.TrimRight(text, "\n")
	return exec.Command("tmux", "send-keys", "-t", session, "-l", text).Run()
}

func tmuxSendEnter(session string) error {
	return exec.Command("tmux", "send-keys", "-t", session, "Enter").Run()
}

func tmuxCapturePane(session string) (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-t", session, "-p").Output()
	return string(out), err
}

// pollPaneContains polls the tmux pane until it contains the given substring.
func pollPaneContains(ctx context.Context, session, substr string, interval time.Duration) error {
	for {
		content, err := tmuxCapturePane(session)
		if err == nil && strings.Contains(content, substr) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// pollPaneNotContains polls the tmux pane until it no longer contains substr.
func pollPaneNotContains(ctx context.Context, session, substr string, interval time.Duration) error {
	for {
		content, err := tmuxCapturePane(session)
		if err == nil && !strings.Contains(content, substr) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// pollPaneChanged polls the tmux pane until its content differs from before,
// or ctx is done. Returns true if a change was observed.
func pollPaneChanged(ctx context.Context, session, before string, interval time.Duration) bool {
	for {
		content, err := tmuxCapturePane(session)
		if err == nil && content != before {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
}
