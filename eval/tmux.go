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

func tmuxSendKeys(session, text string) error {
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
