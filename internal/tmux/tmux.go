// Package tmux drives tmux sessions: the mechanism behind `milk eval`'s
// claude-code adapter (a real terminal for a real CLI, driven and observed
// exactly as a human would) and the showcase recorder (`cmd/showcase`, issue
// #387 — state-synchronized instead of sleep-driven, which is why a scripted
// session survives live model latency where a fixed-timing tape rots).
//
// Everything here observes through capture-pane: send keys, then poll the
// pane until it *shows* the state the step waits for. No step ever guesses
// how long a backend needs.
package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// NewSession starts a detached tmux session of the given size (tmux cells:
// columns/rows). With a start directory, the session's shell opens there.
func NewSession(name string, width, height int, startDir ...string) error {
	args := []string{"new-session", "-d", "-s", name,
		"-x", fmt.Sprint(width), "-y", fmt.Sprint(height)}
	if len(startDir) > 0 && startDir[0] != "" {
		args = append(args, "-c", startDir[0])
	}
	return exec.Command("tmux", args...).Run()
}

// KillSession tears a session down. Missing sessions are not an error the
// caller usually cares about, but the raw result is returned so callers with
// a real opinion can have one.
func KillSession(name string) error {
	return exec.Command("tmux", "kill-session", "-t", name).Run()
}

// SendKeys sends literal text to the pane. Trailing newlines are stripped:
// YAML folded scalars (">") append a trailing "\n", and sending that via "-l"
// inserts a literal newline into the target textarea instead of submitting —
// TUIs like Claude Code and milk require an explicit Enter key event (sent
// separately via SendEnter) to submit, so a trailing "\n" here just leaves
// the prompt sitting unsubmitted and the adapter hangs waiting for a response
// that never starts.
func SendKeys(session, text string) error {
	text = strings.TrimRight(text, "\n")
	return exec.Command("tmux", "send-keys", "-t", session, "-l", text).Run()
}

// SendEnter submits whatever the target textarea holds.
func SendEnter(session string) error {
	return exec.Command("tmux", "send-keys", "-t", session, "Enter").Run()
}

// SendKey sends a named key (e.g. "Down", "Enter", "C-j", "F6") to the pane.
func SendKey(session, key string) error {
	return exec.Command("tmux", "send-keys", "-t", session, key).Run()
}

// SendBackspace sends the Backspace key n times in a single tmux call —
// clearing a textarea via n separate execs would spend the beat's opening
// second in process spawns.
func SendBackspace(session string, n int) error {
	if n <= 0 {
		return nil
	}
	args := make([]string, 0, 5+n)
	args = append(args, "send-keys", "-t", session)
	for i := 0; i < n; i++ {
		args = append(args, "BSpace")
	}
	return exec.Command("tmux", args...).Run()
}

// CapturePane returns the pane's visible text (no escape sequences).
func CapturePane(session string) (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-t", session, "-p").Output()
	return string(out), err
}

// PollPaneContains polls the tmux pane until it contains the given substring.
func PollPaneContains(ctx context.Context, session, substr string, interval time.Duration) error {
	for {
		content, err := CapturePane(session)
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

// PollPaneNotContains polls the tmux pane until it no longer contains substr.
func PollPaneNotContains(ctx context.Context, session, substr string, interval time.Duration) error {
	for {
		content, err := CapturePane(session)
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
