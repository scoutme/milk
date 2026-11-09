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
	"time"

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
	// lastCtrlC stamps the most recent Ctrl+C forwarded into the pane; a
	// second press inside ptyCtrlCKillWindow escalates to forceKillPTYPane.
	// Touched on the UI goroutine only (handlePTYKey runs there).
	lastCtrlC time.Time
}

// ptyCtrlCKillWindow is the double-press window of the pane's force-kill
// escape: the first Ctrl+C is forwarded as ^C like any terminal; a second
// one this soon after means the child is not responding to interrupts and
// the user wants out of the pane.
const ptyCtrlCKillWindow = 2 * time.Second

// launchPTYPane starts shellCmd inside a PTY and wires the output reader.
func (m model) launchPTYPane(shellCmd string) (tea.Model, tea.Cmd) {
	// Single choke point for every command milk executes on the user's
	// behalf (bang-mode and allow-listed direct-bash runs): record it in
	// milk's shell history so bang-mode Ctrl+R finds it later.
	m.recordShellCommand(shellCmd)
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
		return directBashDoneMsg{err: waitErr, pane: state}
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
	raw := ptyKeyToBytes(msg)
	// Ctrl+C is forwarded as ^C (the status bar says so) — but ^C alone can
	// be swallowed: network-stuck FTP clients, task runners trapping SIGINT,
	// raw-mode tools reading 0x03 as data. Update gives the pane top
	// key-dispatch priority, so milk's own Ctrl+C stack never runs while a
	// pane is up — such a child would trap the user in the pane forever. A
	// second Ctrl+C inside the window escalates to forceKillPTYPane, the
	// pane's escape hatch (advertised in the status bar).
	if len(raw) == 1 && raw[0] == 0x03 {
		now := time.Now()
		if !m.ptyPane.lastCtrlC.IsZero() && now.Sub(m.ptyPane.lastCtrlC) <= ptyCtrlCKillWindow {
			return m.forceKillPTYPane(), nil
		}
		m.ptyPane.lastCtrlC = now
	}
	if len(raw) > 0 {
		m.ptyPane.ptm.Write(raw) //nolint:errcheck
	}
	return m, nil
}

// forceKillPTYPane is the pane's escape hatch (the status bar's
// "Ctrl+C ×2 = force-kill"): SIGKILL the child's whole process group, close
// the master fd, and tear the pane's state down right here instead of
// waiting for the read goroutine. The eager teardown is deliberate — the
// waiter's cmd.Wait may never return (a descendant leaked the slave fd, or
// the child is wedged in an unkillable state), and the user must get back
// to milk either way. A late directBashDoneMsg from the orphaned waiter is
// ignored by identity — see its handler.
func (m model) forceKillPTYPane() tea.Model {
	p := m.ptyPane
	if p == nil {
		return m
	}
	// Snapshot before the kill: the transcript gets the same hand-off a
	// normal exit produces, plus an explicit reason for the stop.
	out := m.ptySnapshot()
	m.appendTranscript(fmt.Sprintf("%s direct-bash force-killed: %s\n", milkTag(), p.shellCmd))
	if out != "" {
		m.appendTranscript(dimLines(out))
		m.currentTurnChars += int64(len(out))
	}
	// Mirror directBashDoneMsg's busy bookkeeping: never clobber a turn that
	// was already running when the bang launched (issue #128) — queue the
	// output for that turn's next prompt instead of only the transcript.
	concurrentTurn := m.directBashConcurrentTurn
	m.directBashConcurrentTurn = false
	if !concurrentTurn {
		m.busy = false
		m.cancelTurn = nil
		m.busyHint = ""
	}
	if out != "" && concurrentTurn && m.st != nil && m.st.sess != nil {
		m.st.sess.PendingBangOutput = append(m.st.sess.PendingBangOutput,
			fmt.Sprintf("[direct command %q force-killed — output:\n%s]\n", p.shellCmd, out))
	}
	killPTYProcessGroup(p.cmd)
	_ = p.ptm.Close()
	m.ptyPane = nil
	m.refreshPrompt()
	m.syncLayout()
	return m
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
