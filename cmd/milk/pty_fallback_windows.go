//go:build windows

package main

import (
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

// launchDirectBashFallback runs shellCmd via tea.ExecProcess, which suspends
// the TUI and hands the real console directly to the child process. Used
// when creack/pty reports ErrUnsupported — always the case on Windows, where
// the library ships a stub with no real PTY implementation.
//
// Direct-bash commands are typed assuming POSIX shell syntax (pipes,
// ls/grep/...), so a POSIX shell is preferred when one is on PATH (Git Bash,
// MSYS2, Cygwin, WSL interop all provide "bash" or "sh"); cmd.exe is the
// last resort and only understands native Windows commands.
func (m model) launchDirectBashFallback(shellCmd string) (tea.Model, tea.Cmd) {
	cmd := fallbackShellCommand(shellCmd)
	cmd.Env = os.Environ()
	setPdeathsig(cmd)

	m.appendTranscript(dim("[sh] no embedded terminal on Windows — running directly\n"))
	m.busy = true
	m.spinnerFrame = 0

	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return directBashDoneMsg{err: err}
	})
}

func fallbackShellCommand(shellCmd string) *exec.Cmd {
	for _, sh := range []string{"bash", "sh"} {
		if path, err := exec.LookPath(sh); err == nil {
			return exec.Command(path, "-c", shellCmd)
		}
	}
	return exec.Command("cmd", "/C", shellCmd)
}
