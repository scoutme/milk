package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
	"github.com/vito/midterm"
)

// ptyPaneState holds the live state for an embedded PTY shell command.
type ptyPaneState struct {
	ptm      *os.File          // PTY master fd
	vt       *midterm.Terminal // VT100 screen emulator
	vtMu     sync.Mutex        // guards all vt.Write and vt.Render calls
	cmd      *exec.Cmd
	shellCmd string // original command, shown in status bar
	// scrollback accumulates lines pushed off the top of the VT screen.
	scrollback []string
	// pending is 1 when a ptyOutputMsg is already queued; prevents flooding.
	pending int32
}

// launchPTYPane starts shellCmd inside a PTY and wires the output reader.
func (m model) launchPTYPane(shellCmd string) (tea.Model, tea.Cmd) {
	vpH := m.viewportHeight()
	paneCols := m.mainWidth() - 1 // -1 for the scrollbar column
	if vpH < 3 {
		vpH = 24
	}
	if paneCols < 10 {
		paneCols = 80
	}

	cmd := exec.Command("sh", "-c", shellCmd)
	cmd.Env = os.Environ()
	// Auto-kill the child when the parent process dies (Linux).
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}

	ws := &pty.Winsize{Rows: uint16(vpH), Cols: uint16(paneCols)}
	ptm, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		return m, func() tea.Msg {
			return directBashDoneMsg{err: fmt.Errorf("pty start: %w", err)}
		}
	}

	vt := midterm.NewTerminal(vpH, paneCols)
	state := &ptyPaneState{
		ptm:      ptm,
		vt:       vt,
		cmd:      cmd,
		shellCmd: shellCmd,
	}

	m.ptyPane = state
	m.busy = true
	m.spinnerFrame = 0

	prog := m.st.program
	send := func(msg tea.Msg) {
		if prog != nil {
			prog.Send(msg)
		}
	}

	readCmd := func() tea.Msg {
		buf := make([]byte, 4096)
		for {
			n, readErr := ptm.Read(buf)
			if n > 0 {
				state.vtMu.Lock()
				state.vt.Write(buf[:n]) //nolint:errcheck
				state.vtMu.Unlock()
				if atomic.CompareAndSwapInt32(&state.pending, 0, 1) {
					send(ptyOutputMsg{})
				}
			}
			if readErr != nil {
				break
			}
		}
		waitErr := cmd.Wait()
		return directBashDoneMsg{err: waitErr}
	}

	return m, tea.Batch(spinnerTick(), readCmd)
}

// renderPTYPane renders the current VT terminal screen to a fixed-size string.
// Called from View() — must be fast and safe to call concurrently with the read goroutine.
func (m *model) renderPTYPane(h, w int) string {
	p := m.ptyPane
	p.vtMu.Lock()
	defer p.vtMu.Unlock()

	var sb strings.Builder
	if err := p.vt.Render(&sb); err != nil {
		return strings.Repeat("\n", h-1)
	}
	return sb.String()
}

// ptySnapshot returns the combined scrollback + current screen as cleaned text,
// used when appending PTY output to the transcript on exit.
func (m *model) ptySnapshot() string {
	p := m.ptyPane
	p.vtMu.Lock()
	var sbFinal strings.Builder
	p.vt.Render(&sbFinal) //nolint:errcheck
	p.vtMu.Unlock()

	parts := make([]string, 0, len(p.scrollback)+1)
	parts = append(parts, p.scrollback...)
	if s := sbFinal.String(); s != "" {
		parts = append(parts, s)
	}
	raw := strings.Join(parts, "\n")
	clean := stripANSI(raw)
	clean = strings.ReplaceAll(clean, "\r\n", "\n")
	clean = strings.ReplaceAll(clean, "\r", "\n")
	// Drop trailing blank lines produced by the fixed-size VT screen.
	lines := strings.Split(clean, "\n")
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if end == 0 {
		return ""
	}
	return strings.Join(lines[:end], "\n") + "\n"
}

// handlePTYKey forwards a key event to the PTY master fd.
func (m model) handlePTYKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.ptyPane == nil {
		return m, nil
	}
	if raw := ptyKeyToBytes(msg); len(raw) > 0 {
		m.ptyPane.ptm.Write(raw) //nolint:errcheck
	}
	return m, nil
}

// ptyKeyToBytes converts a bubbletea KeyMsg to the raw byte sequence the
// terminal expects. Covers the most common keys; unknown keys are dropped.
func ptyKeyToBytes(msg tea.KeyMsg) []byte {
	switch msg.Type {
	case tea.KeyCtrlC:
		return []byte{0x03}
	case tea.KeyCtrlD:
		return []byte{0x04}
	case tea.KeyCtrlZ:
		return []byte{0x1a}
	case tea.KeyCtrlL:
		return []byte{0x0c}
	case tea.KeyCtrlU:
		return []byte{0x15}
	case tea.KeyCtrlW:
		return []byte{0x17}
	case tea.KeyEnter:
		return []byte{'\r'}
	case tea.KeyBackspace:
		return []byte{0x7f}
	case tea.KeyTab:
		return []byte{'\t'}
	case tea.KeyEscape:
		return []byte{'\x1b'}
	case tea.KeySpace:
		return []byte{' '}
	case tea.KeyUp:
		return []byte{'\x1b', '[', 'A'}
	case tea.KeyDown:
		return []byte{'\x1b', '[', 'B'}
	case tea.KeyRight:
		return []byte{'\x1b', '[', 'C'}
	case tea.KeyLeft:
		return []byte{'\x1b', '[', 'D'}
	case tea.KeyHome:
		return []byte{'\x1b', '[', 'H'}
	case tea.KeyEnd:
		return []byte{'\x1b', '[', 'F'}
	case tea.KeyDelete:
		return []byte{'\x1b', '[', '3', '~'}
	case tea.KeyPgUp:
		return []byte{'\x1b', '[', '5', '~'}
	case tea.KeyPgDown:
		return []byte{'\x1b', '[', '6', '~'}
	case tea.KeyF1:
		return []byte{'\x1b', 'O', 'P'}
	case tea.KeyF2:
		return []byte{'\x1b', 'O', 'Q'}
	case tea.KeyF3:
		return []byte{'\x1b', 'O', 'R'}
	case tea.KeyF4:
		return []byte{'\x1b', 'O', 'S'}
	case tea.KeyRunes:
		if len(msg.Runes) > 0 {
			return []byte(string(msg.Runes))
		}
	}
	return nil
}
