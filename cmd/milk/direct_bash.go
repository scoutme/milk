package main

import (
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

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
		return m, execDirectBash(cmd)
	default:
		// User declined — route to agent as normal input.
		m.appendTranscript("n\n")
		m.refreshPrompt()
		return m.dispatchAgent(cmd)
	}
}

// execDirectBash runs shellCmd inside script(1) so that:
//   - the process gets a real PTY (sudo, passwd, vim, etc. work normally)
//   - all output is recorded to a temp file
//
// After the process exits, directBashDoneMsg carries the transcript path so
// the Update handler can read it and append the output to the milk transcript.
func execDirectBash(shellCmd string) tea.Cmd {
	tmp, err := os.CreateTemp("", "milk-bash-*.log")
	if err != nil {
		// Can't create temp file — fall back to plain ExecProcess with no recording.
		c := exec.Command("sh", "-c", shellCmd)
		return tea.ExecProcess(c, func(err error) tea.Msg {
			return directBashDoneMsg{err: err}
		})
	}
	tmp.Close()
	logPath := tmp.Name()

	// script -q -e -c <cmd> <logfile>
	//   -q  quiet (no "Script started/done" lines)
	//   -e  exit with the child's exit code
	//   -c  run a single command rather than an interactive shell
	c := exec.Command("script", "-q", "-e", "-c", shellCmd, logPath)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return directBashDoneMsg{err: err, logPath: logPath}
	})
}

// readDirectBashLog reads the script(1) typescript file, strips ANSI escape
// sequences and carriage returns, and returns the cleaned output.
func readDirectBashLog(path string) string {
	data, err := os.ReadFile(path)
	os.Remove(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	// script prepends a timing header when using --logging-format; with -q it
	// writes raw PTY output. Strip ANSI and normalize line endings.
	clean := stripANSI(string(data))
	clean = strings.ReplaceAll(clean, "\r\n", "\n")
	clean = strings.ReplaceAll(clean, "\r", "\n")
	return clean
}
