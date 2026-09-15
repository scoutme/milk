//go:build !windows

package main

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// launchDirectBashFallback is only reachable on Windows in practice: every
// other platform creack/pty supports (Linux, macOS, the BSDs) has a real PTY
// implementation, so pty.StartWithSize should never return ErrUnsupported
// here. Surface the original error instead of silently no-oping.
func (m model) launchDirectBashFallback(_ string) (tea.Model, tea.Cmd) {
	return m, func() tea.Msg {
		return directBashDoneMsg{err: fmt.Errorf("pty start: unsupported on this platform")}
	}
}
