//go:build windows

package main

import (
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

// launchDirectBashFallback runs shellCmd with stdout/stderr piped and
// streamed live into the transcript via chunkMsg — the same mechanism used
// for streaming LLM output (sendWriter, repl.go) — since there is no real
// PTY to embed a VT pane against. creack/pty ships only a stub on Windows,
// so pty.StartWithSize always reports ErrUnsupported there.
//
// Unlike the embedded-PTY path, the child gets no stdin, so interactive
// prompts (editors, y/n confirmations) won't work — acceptable for the
// one-shot commands direct-bash targets.
//
// Direct-bash commands are typed assuming POSIX shell syntax (pipes,
// ls/grep/...), so a POSIX shell is preferred when one is on PATH (Git Bash,
// MSYS2, Cygwin, WSL interop all provide "bash" or "sh"); cmd.exe is the
// last resort and only understands native Windows commands.
func (m model) launchDirectBashFallback(shellCmd string) (tea.Model, tea.Cmd) {
	cmd := fallbackShellCommand(shellCmd)
	cmd.Env = os.Environ()
	setPdeathsig(cmd)

	prog := m.st.program
	send := func(msg tea.Msg) {
		if prog != nil {
			prog.Send(msg)
		}
	}
	sw := &sendWriter{send: send}
	cmd.Stdout = sw
	cmd.Stderr = sw

	if err := cmd.Start(); err != nil {
		return m, func() tea.Msg {
			return directBashDoneMsg{err: err}
		}
	}

	// See launchPTYPane's comment: tells directBashDoneMsg's cleanup not to
	// clobber an already-in-progress turn's busy/cancelTurn state.
	m.directBashConcurrentTurn = m.busy
	m.busy = true
	m.spinnerFrame = 0

	return m, func() tea.Msg {
		return directBashDoneMsg{err: cmd.Wait()}
	}
}

func fallbackShellCommand(shellCmd string) *exec.Cmd {
	for _, sh := range []string{"bash", "sh"} {
		if path, err := exec.LookPath(sh); err == nil {
			return exec.Command(path, "-c", shellCmd)
		}
	}
	return exec.Command("cmd", "/C", shellCmd)
}
