package main

import (
	"charm.land/bubbles/v2/key"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
	setPdeathsig(cmd)

	ws := &pty.Winsize{Rows: uint16(vpH), Cols: uint16(paneCols)}
	ptm, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		if errors.Is(err, pty.ErrUnsupported) {
			// No real PTY implementation on this platform (Windows: creack/pty
			// ships a stub there). Fall back to handing the real console to the
			// child process directly instead of failing outright.
			return m.launchDirectBashFallback(shellCmd)
		}
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

	// m.ptyPane is nil whenever launchPTYPane can be reached (PTY-active takes
	// top dispatch priority, so a second bang can't stack), so m.busy already
	// true here means an agent turn is running underneath — a bang run while
	// busy (issue #128). directBashConcurrentTurn tells directBashDoneMsg's
	// cleanup not to clobber that turn's busy/cancelTurn state.
	m.directBashConcurrentTurn = m.busy
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
	clean := ansi.Strip(raw)
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
func (m model) handlePTYKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.ptyPane == nil {
		return m, nil
	}
	// Toast dismissal (issue #162) works in shell mode too — ctrl+g is not
	// in ptyKeyToBytes (it would be dropped anyway), so intercept it first.
	if key.Matches(msg, keys.dismissToasts) && m.dismissToasts() {
		return m, nil
	}
	if raw := ptyKeyToBytes(msg); len(raw) > 0 {
		m.ptyPane.ptm.Write(raw) //nolint:errcheck
	}
	return m, nil
}

// ptyKeyToBytes converts a bubbletea KeyMsg to the raw byte sequence the
// terminal expects. Covers the most common keys; unknown keys are dropped.
func ptyKeyToBytes(msg tea.KeyPressMsg) []byte {
	if msg.Mod == tea.ModCtrl {
		if raw, ok := ptyCtrlBytes[msg.Code]; ok {
			return raw
		}
	}
	if raw, ok := ptyNamedKeyBytes[msg.Code]; ok {
		return raw
	}
	return []byte(msg.Text)
}

var ptyCtrlBytes = map[rune][]byte{
	'c': {0x03},
	'd': {0x04},
	'z': {0x1a},
	'l': {0x0c},
	'u': {0x15},
	'w': {0x17},
}

var ptyNamedKeyBytes = map[rune][]byte{
	tea.KeyEnter:     {'\r'},
	tea.KeyBackspace: {0x7f},
	tea.KeyTab:       {'\t'},
	tea.KeyEscape:    {'\x1b'},
	tea.KeySpace:     {' '},
	tea.KeyUp:        {'\x1b', '[', 'A'},
	tea.KeyDown:      {'\x1b', '[', 'B'},
	tea.KeyRight:     {'\x1b', '[', 'C'},
	tea.KeyLeft:      {'\x1b', '[', 'D'},
	tea.KeyHome:      {'\x1b', '[', 'H'},
	tea.KeyEnd:       {'\x1b', '[', 'F'},
	tea.KeyDelete:    {'\x1b', '[', '3', '~'},
	tea.KeyPgUp:      {'\x1b', '[', '5', '~'},
	tea.KeyPgDown:    {'\x1b', '[', '6', '~'},
	tea.KeyF1:        {'\x1b', 'O', 'P'},
	tea.KeyF2:        {'\x1b', 'O', 'Q'},
	tea.KeyF3:        {'\x1b', 'O', 'R'},
	tea.KeyF4:        {'\x1b', 'O', 'S'},
}
