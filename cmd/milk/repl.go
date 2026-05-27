package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	rw "github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"

	"github.com/atotto/clipboard"
	"github.com/aymanbagabas/go-osc52/v2"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/claudesettings"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
)

const agentTimeout = 10 * time.Minute

// undoDebugLog is a debug-only file writer for undo/redo diagnostics.
// Set undoDebugLogPath to a non-empty path to enable; leave empty to disable.
const undoDebugLogPath = "/tmp/milk_undo_debug.log"

var undoDebugFile *os.File

func undoDebugLog(format string, args ...any) {
	if undoDebugLogPath == "" {
		return
	}
	if undoDebugFile == nil {
		var err error
		undoDebugFile, err = os.OpenFile(undoDebugLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return
		}
	}
	fmt.Fprintf(undoDebugFile, "[%s] "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
}

const memoryPanelWidth = 33 // chars for the memory panel (32 inner + 1 right scrollbar)
const memoryPanelInner = 32 // usable inner chars; scrollbar is a separate column in View()
const memoryPollInterval = 5 * time.Second

// dispatchAgents holds the agents and their availability for a turn.
type dispatchAgents struct {
	local       *local.Agent
	claude      *claude.Agent
	localAvail  bool
	claudeAvail bool
}

// --- TUI message types ---

// chunkMsg carries a chunk of streamed agent output.
type chunkMsg struct{ text string }

// agentDoneMsg signals the agent goroutine finished.
type agentDoneMsg struct{ err error }

type spinnerTickMsg struct{}

// copyFeedbackClearMsg clears the transient copy confirmation in the status bar.
type copyFeedbackClearMsg struct{}

// busyHintClearMsg clears the transient "agent is responding" hint in the status bar.
type busyHintClearMsg struct{}

// quitPendingClearMsg clears the "press ctrl+c again to exit" hint.
type quitPendingClearMsg struct{}

// memoryRefreshMsg fires on a periodic tick to redraw the memory panel.
type memoryRefreshMsg struct{}

// toolUseMsg carries the name of a tool Claude just started calling.
type toolUseMsg struct{ name string }

// credRefreshReadyMsg is sent when a background credential refresh completes.
// label identifies the provider (e.g. "AWS", "token_cmd"). err is non-nil on
// failure; creds carries new AWS credentials when applicable (nil for token_cmd).
type credRefreshReadyMsg struct {
	label string
	creds *claude.AWSCreds
	err   error
}

// permRequestMsg is sent by the agent goroutine when it needs a y/n answer.
// The agent blocks on respCh until the TUI sends a permResponseMsg back.
type permRequestMsg struct {
	prompt string
	respCh chan string
}

// forgetState holds the pending /forget confirmation dialog.
type forgetState struct {
	candidates []memory.Percept // matched percepts shown to the user
}

// addProviderState tracks state for the multi-step /provider add wizard.
// Fields are filled one at a time when the user doesn't supply them inline.
type addProviderState struct {
	ac   config.LocalAgentConfig
	step addProviderStep
}

type addProviderStep int

const (
	addStepName addProviderStep = iota
	addStepURL
	addStepModel
	addStepProvider
	addStepAPIKey    // only when provider is bearer
	addStepAWSRegion // only when provider is bedrock
	addStepDone
)

// undoEntry records a textarea snapshot for undo/redo.
type undoEntry struct {
	value  string
	cursor int // rune offset
}

const undoMaxDepth = 100
const undoCoalesceWindow = 2 * time.Second

// sendWriter is an io.Writer that forwards each Write as a chunkMsg
// via tea.Program.Send, enabling live streaming into the TUI viewport.
type sendWriter struct {
	send func(msg tea.Msg)
}

func (w *sendWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.send(chunkMsg{text: string(p)})
	}
	return len(p), nil
}

// tuiInputReader implements inputReader for the TUI: sends a permRequestMsg
// and blocks until the user responds via the TUI input area.
type tuiInputReader struct {
	send func(msg tea.Msg)
}

func (r *tuiInputReader) readLine(prompt string) (string, error) {
	respCh := make(chan string, 1)
	r.send(permRequestMsg{prompt: prompt, respCh: respCh})
	return <-respCh, nil
}

// --- Styles ---

var (
	styleHeaderBar = lipgloss.NewStyle().
			Background(lipgloss.AdaptiveColor{Light: "#1E2A4A", Dark: "#0E0E1A"}).
			Foreground(lipgloss.AdaptiveColor{Light: "#D8E4F8", Dark: "#AABBCC"}).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(lipgloss.AdaptiveColor{Light: "#4466AA", Dark: "#334466"})
	styleStatusBar = lipgloss.NewStyle().
			Background(lipgloss.AdaptiveColor{Light: "#E5E5E5", Dark: "#2B2B2B"})
	styleStatusBarPerm = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#1A1A00")).
				Background(lipgloss.AdaptiveColor{Light: "#FFD700", Dark: "#B8860B"})
	styleBorder = lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).
			BorderTop(true).
			BorderForeground(lipgloss.AdaptiveColor{Light: "#AAA", Dark: "#555"})
)

// --- model ---

type model struct {
	vp     viewport.Model
	ta     textarea.Model
	width  int
	height int
	ready  bool

	// transcript accumulator (pointer — strings.Builder must not be copied by value)
	transcript *strings.Builder

	// spinner state
	busy         bool
	spinnerFrame int

	// history navigation
	sessionHistory   []string // entries for this session only (default navigation)
	globalHistory    []string // entries across all sessions
	useGlobalHistory bool     // when true, navigate globalHistory instead
	histIdx          int
	saved            string

	// ctrl+r / ctrl+s incremental search state
	searching     bool
	searchForward bool // false = reverse (ctrl+r), true = forward (ctrl+s)
	searchQuery   strings.Builder
	searchIdx     int // position in activeHistory() we last matched

	// tab completion
	tabMatches []string
	tabIdx     int
	tabLine    int      // line index the current tabMatches were built for
	tabPrefix  string   // what the user had typed when Tab was first pressed
	tabHints   []string // hint lines shown below viewport while completing a slash command

	// pending permission request (non-nil while waiting for user y/n) and queue
	// for tool-use permission prompts that arrive while a prior one is active.
	pendingPerm *permRequestMsg
	permQueue   []permRequestMsg

	// cancelTurn cancels the context of the running agent turn; nil when idle.
	cancelTurn  context.CancelFunc
	interrupted bool // set when user cancels a turn via ctrl+c

	// active tool use — non-empty while Claude is executing a tool call
	activeToolUse string

	// memory panel
	panelMemory        bool
	panelOffset        int
	mem                *memory.Store
	lastPanelClickID   string
	lastPanelClickTime time.Time

	// pending /forget confirmation
	pendingForget *forgetState

	// pending /provider add wizard
	pendingAdd *addProviderState

	// prompt width (visual columns) set by the most recent refreshPrompt call;
	// used by taRows() to compute the exact content wrap width.
	promptWidth int

	// click-to-select state (content-space coordinates; -1 = none)
	selAnchorLine  int
	selAnchorCol   int
	selEndLine     int
	selEndCol      int
	selDragging    bool   // true once the mouse has moved after the initial press
	selText        string // plain text of the selected range (populated after release)
	copyFeedback   string // transient "[copied N chars]" shown in status bar
	busyHint       string // transient "agent is responding" shown in status bar
	credRefreshing bool   // true while any background credential refresh is running
	credLabel      string // which credential is being refreshed (e.g. "AWS", "token")
	credStatus     string // non-empty after refresh completes: last result message
	credOK         bool   // true if last refresh succeeded, false if failed

	// keyboard selection state in the input area (rune offsets into ta.Value(); -1 = none)
	taSelAnchor int
	taSelEnd    int

	// undo/redo stacks for the input textarea
	undoStack     []undoEntry
	redoStack     []undoEntry
	lastUndoTime  time.Time
	lastUndoValue string // ta.Value() at the time of the last pushed entry

	// quit confirmation state
	quitPending bool

	// hasLocalAgentConfig is true when the user has explicitly configured a
	// local-agent backend. Used to show setup hints on the welcome screen.
	hasLocalAgentConfig bool

	colorizeMode ColorizeMode

	// colorize cache: avoid re-running chroma/glamour on every streamed token.
	// The cache is invalidated when the transcript grows by ≥ colorizeLineThresh
	// new lines, or when the viewport offset/width changes, or when the caller
	// explicitly sets colorizeForce = true (e.g. after agentDoneMsg, scroll, resize).
	colorizeCached    string // last colorized output
	colorizeTransLen  int    // transcript byte length when cache was built
	colorizeVPOffset  int    // vp.YOffset when cache was built
	colorizeVPWidth   int    // vpWidth when cache was built
	colorizeForce     bool   // if true, bypass cache on next render
	colorizeLinesSeen int    // new lines since last full re-colorize

	// injected dependencies
	ctx    context.Context
	st     *interactiveState
	rtr    *router.Router
	agents dispatchAgents
}

func newModel(ctx context.Context, st *interactiveState, rtr *router.Router, agents dispatchAgents, mem *memory.Store) model {
	ta := buildTextarea()
	return model{
		histIdx:       -1,
		ctx:           ctx,
		st:            st,
		rtr:           rtr,
		agents:        agents,
		ta:            ta,
		transcript:    &strings.Builder{},
		mem:           mem,
		panelMemory:   true,
		selAnchorLine: -1,
		selEndLine:    -1,
		taSelAnchor:   -1,
		taSelEnd:      -1,
		lastUndoValue: "\x00", // sentinel: never equals real textarea value, so first push always succeeds
	}
}

func buildTextarea() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = ""
	ta.ShowLineNumbers = false
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.BlurredStyle.Base = lipgloss.NewStyle()
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = lipgloss.NewStyle()
	ta.BlurredStyle.Prompt = lipgloss.NewStyle()
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.SetHeight(1)

	km := textarea.DefaultKeyMap
	km.InsertNewline.SetKeys("shift+enter", "alt+enter", "ctrl+n")
	km.WordBackward.SetKeys("alt+left", "ctrl+left")
	km.WordForward.SetKeys("alt+right", "ctrl+right")
	ta.KeyMap = km

	ta.Focus() //nolint:errcheck
	return ta
}

// refreshPrompt updates the textarea prompt label and width to match the current mode.
func (m *model) refreshPrompt() {
	var label string
	if m.searching {
		dir := "r"
		if m.searchForward {
			dir = "f"
		}
		label = yellow("("+dir+"-search)") + " ❯ "
	} else {
		label = promptLabel(m.st)
	}
	plain := stripANSI(label)
	m.promptWidth = rw.StringWidth(plain)

	m.ta.SetPromptFunc(m.promptWidth, func(lineIdx int) string {
		if lineIdx == 0 {
			return label
		}
		return ""
	})
	if m.width > 0 {
		m.ta.SetWidth(m.mainWidth())
	}
}

// inputLocked returns true when agent is running.
func (m *model) inputLocked() bool { return m.busy }

// handleBusyKey handles key events while an agent turn is running.
// Enter is blocked (with a one-time hint); ctrl+c cancels; all other keys
// are forwarded to the textarea so the user can pre-compose the next message.
func (m model) handleBusyKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		if m.cancelTurn != nil {
			m.cancelTurn()
			m.cancelTurn = nil
			m.interrupted = true
		}
		return m, nil
	case "enter", "ctrl+m":
		m.busyHint = "agent is responding — Ctrl+C to interrupt"
		return m, busyHintClearCmd()
	}
	var cmd tea.Cmd
	m.undoPush(true)
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// handlePermKey routes key events while a permission prompt is pending.
// Only enter submits; anything else is passed to the textarea normally.
func (m model) handlePermKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.pendingPerm.respCh <- "n"
		m.appendTranscript("n\n")
		m.pendingPerm = nil
		m.dequeueNextPerm()
		return m, nil
	case "enter":
		answer := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript(answer + "\n")
		m.pendingPerm.respCh <- answer
		m.pendingPerm = nil
		m.dequeueNextPerm()
		return m, nil
	}
	var cmd tea.Cmd
	m.undoPush(true)
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// dequeueNextPerm promotes the next queued permission prompt, if any.
func (m *model) dequeueNextPerm() {
	if len(m.permQueue) == 0 {
		return
	}
	next := m.permQueue[0]
	m.permQueue = m.permQueue[1:]
	m.pendingPerm = &next
	m.appendTranscript(next.prompt)
	m.ta.Reset()
	m.syncLayout()
}

func (m model) handlePermRequest(msg permRequestMsg) (tea.Model, tea.Cmd) {
	if m.pendingPerm != nil {
		m.permQueue = append(m.permQueue, msg)
		return m, nil
	}
	m.pendingPerm = &msg
	m.appendTranscript(msg.prompt)
	m.ta.Reset()
	m.syncLayout()
	return m, nil
}

func (m model) handleAgentDone(msg agentDoneMsg) (tea.Model, tea.Cmd) {
	m.busy = false
	m.activeToolUse = ""
	m.cancelTurn = nil
	m.busyHint = ""
	if m.interrupted {
		m.interrupted = false
		m.appendTranscript(dim("[interrupted]") + "\n")
	} else if msg.err != nil {
		m.appendTranscript(milkTag() + " error: " + msg.err.Error() + "\n")
	}
	m.appendTranscript("\n")
	m.colorizeForce = true // turn finished — force a clean full re-colorize
	m.refreshPrompt()
	m.syncLayout()
	return m, nil
}

func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	ev := tea.MouseEvent(msg)
	inPanel := m.panelMemory && ev.X >= m.mainWidth()
	switch ev.Button {
	case tea.MouseButtonWheelUp:
		if inPanel {
			if m.panelOffset > 0 {
				m.panelOffset--
			}
		} else {
			m.vp.ScrollUp(3)
		}
	case tea.MouseButtonWheelDown:
		if inPanel {
			m.panelOffset++
		} else {
			m.vp.ScrollDown(3)
		}
	case tea.MouseButtonLeft:
		if inPanel && ev.Action == tea.MouseActionPress {
			const panelRowStart = 2 // same header offset as the main viewport
			panelLine := m.panelOffset + (ev.Y - panelRowStart)
			ids := buildPanelLineIDs(m.mem)
			if panelLine >= 0 && panelLine < len(ids) {
				id := ids[panelLine]
				if id != "" {
					now := time.Now()
					if id == m.lastPanelClickID && now.Sub(m.lastPanelClickTime) <= 400*time.Millisecond {
						// Double-click: show full percept details in the transcript.
						result := execMemoryShow("#"+id[:min(6, len(id))], m.st)
						m.appendTranscript(result + "\n")
						m.vp.GotoBottom()
						m.lastPanelClickID = ""
					} else {
						m.lastPanelClickID = id
						m.lastPanelClickTime = now
					}
				}
			}
			break
		}
		// Only handle events inside the viewport area (rows 2..height-2).
		const vpRowStart = 2
		vpRowEnd := m.height - 2
		if ev.Y < vpRowStart || ev.Y >= vpRowEnd || inPanel {
			break
		}
		contentLine := m.vp.YOffset + (ev.Y - vpRowStart)
		switch ev.Action {
		case tea.MouseActionPress:
			m.selAnchorLine = contentLine
			m.selAnchorCol = ev.X
			m.selEndLine = -1
			m.selEndCol = 0
			m.selDragging = false
			m.selText = ""
			m.setViewportContent()
		case tea.MouseActionMotion:
			if m.selAnchorLine >= 0 {
				m.selDragging = true
				m.selEndLine = contentLine
				m.selEndCol = ev.X
				m.setViewportContent()
			}
		case tea.MouseActionRelease:
			if m.selAnchorLine >= 0 {
				if contentLine == m.selAnchorLine && ev.X == m.selAnchorCol {
					m.clearSelection()
					m.setViewportContent()
					return m, nil
				}
				m.selEndLine = contentLine
				m.selEndCol = ev.X
				m.selText = m.selectionText()
				m.setViewportContent()
			}
		}
	case tea.MouseButtonRight:
		if ev.Action == tea.MouseActionPress {
			// Transcript selection takes priority; then keyboard input selection.
			if m.selText != "" {
				copyToClipboard(m.selText)
				m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(m.selText)))
				m.clearSelection()
				m.setViewportContent()
				return m, copyFeedbackClearCmd()
			}
			if t := m.taSelText(); t != "" {
				copyToClipboard(t)
				m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(t)))
				m.taClearSel()
				m.setViewportContent()
				return m, copyFeedbackClearCmd()
			}
			// No selection: paste clipboard content into the textarea.
			text, err := clipboard.ReadAll()
			if err == nil && text != "" {
				// Pre-expand to terminal height so repositionView() inside
				// InsertString never scrolls on a multiline clipboard paste.
				m.ta.SetHeight(m.height)
				m.ta.InsertString(text)
			}
		}
	}
	return m, nil
}

// transcriptLines returns the lines of the currently displayed transcript area
// (welcome screen when empty, wrapped transcript otherwise), without selection
// highlighting applied. Used for selection text extraction.
func (m *model) transcriptLines() []string {
	if m.transcript.Len() == 0 {
		return strings.Split(m.welcomeScreen(), "\n")
	}
	vw := m.vpWidth()
	raw := m.transcript.String()
	if vw <= 0 {
		return strings.Split(raw, "\n")
	}
	return strings.Split(ansi.Wrap(raw, vw, ""), "\n")
}

// selectionText extracts the plain text between the selection anchor and end,
// respecting column boundaries on the first and last lines.
func (m *model) selectionText() string {
	lines := m.transcriptLines()
	loLine, loCol := m.selAnchorLine, m.selAnchorCol
	hiLine, hiCol := m.selEndLine, m.selEndCol
	if hiLine < loLine || (hiLine == loLine && hiCol < loCol) {
		loLine, loCol, hiLine, hiCol = hiLine, hiCol, loLine, loCol
	}
	if loLine < 0 {
		loLine = 0
	}
	if hiLine >= len(lines) {
		hiLine = len(lines) - 1
	}
	var sb strings.Builder
	for i := loLine; i <= hiLine; i++ {
		plain := []rune(ansi.Strip(lines[i]))
		start, end := 0, len(plain)
		if i == loLine {
			if loCol < len(plain) {
				start = loCol
			} else {
				start = len(plain)
			}
		}
		if i == hiLine {
			if hiCol < len(plain) {
				end = hiCol
			}
		}
		if start > end {
			start = end
		}
		sb.WriteString(string(plain[start:end]))
		if i < hiLine {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// copyToClipboard writes text to the system clipboard via atotto/clipboard (which
// handles WSL via clip.exe, Wayland via wl-copy, X11 via xclip/xsel) and also
// emits an OSC 52 sequence as a fallback for SSH or tmux environments.
func copyToClipboard(text string) {
	// Primary: OS clipboard (works on WSL, X11, Wayland, macOS).
	_ = clipboard.WriteAll(text)
	// Secondary: OSC 52 — picked up by terminals that support it (kitty, iTerm2, tmux).
	osc52.New(text).WriteTo(os.Stderr)
}

// copyFeedbackClearCmd returns a command that clears the copy feedback after 2s.
func copyFeedbackClearCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return copyFeedbackClearMsg{} })
}

// busyHintClearCmd returns a command that clears the busy hint after 3s.
func busyHintClearCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return busyHintClearMsg{} })
}

// quitPendingClearCmd clears the quit-confirmation state after 3s of inaction.
func quitPendingClearCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return quitPendingClearMsg{} })
}

// clearSelection resets selection state.
func (m *model) clearSelection() {
	m.selAnchorLine = -1
	m.selAnchorCol = 0
	m.selEndLine = -1
	m.selEndCol = 0
	m.selDragging = false
	m.selText = ""
}

// taCursorOffset returns the global rune offset of the textarea cursor within ta.Value().
func (m *model) taCursorOffset() int {
	lines := strings.Split(m.ta.Value(), "\n")
	row := m.ta.Line()
	col := m.ta.LineInfo().ColumnOffset
	offset := 0
	for i := 0; i < row && i < len(lines); i++ {
		offset += len([]rune(lines[i])) + 1 // +1 for the '\n'
	}
	return offset + col
}

// taClearSel clears the keyboard selection in the input area.
func (m *model) taClearSel() {
	m.taSelAnchor = -1
	m.taSelEnd = -1
}

// taDeleteSelection removes the selected rune range from the textarea, placing
// the cursor at the start of the deleted range. Does nothing if no selection.
func (m model) taDeleteSelection() model {
	if m.taSelText() == "" {
		return m
	}
	m.undoPush(false)
	runes := []rune(m.ta.Value())
	lo, hi := m.taSelAnchor, m.taSelEnd
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo < 0 {
		lo = 0
	}
	if hi > len(runes) {
		hi = len(runes)
	}
	// SetValue(prefix) positions cursor at end of prefix (= lo), then InsertString
	// appends the suffix without moving the cursor.
	m.ta.SetValue(string(runes[:lo]))
	m.ta.InsertString(string(runes[hi:]))
	m.taClearSel()
	return m
}

// taSelText returns the plain text of the current keyboard selection, or "".
func (m *model) taSelText() string {
	if m.taSelAnchor < 0 || m.taSelEnd < 0 || m.taSelAnchor == m.taSelEnd {
		return ""
	}
	runes := []rune(m.ta.Value())
	lo, hi := m.taSelAnchor, m.taSelEnd
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo < 0 {
		lo = 0
	}
	if hi > len(runes) {
		hi = len(runes)
	}
	if lo > hi {
		return ""
	}
	return string(runes[lo:hi])
}

// taRows returns the number of display rows the textarea content needs.
// It replicates the exact word-wrap logic from bubbles/textarea wrap() so the
// count matches what the textarea will render — using uniseg.StringWidth for
// display widths and word-boundary splitting, same as the upstream code.
func (m *model) taRows() int {
	w := m.ta.Width()
	if w <= 0 {
		w = 1
	}
	lines := strings.Split(m.ta.Value(), "\n")
	total := 0
	for _, line := range lines {
		total += taWrapRows([]rune(line), w)
	}
	if total < 1 {
		return 1
	}
	return total
}

// taWrapRows counts soft-wrapped rows for a single logical line, mirroring
// the wrap() function in charmbracelet/bubbles/textarea/textarea.go.
func taWrapRows(runes []rune, width int) int {
	var (
		lineW  int    // display width of current soft row
		word   []rune // current word being accumulated
		spaces int    // pending spaces after last word
		rows   = 1
	)
	flush := func() {
		pending := uniseg.StringWidth(string(word)) + spaces
		if lineW+pending > width {
			rows++
			lineW = uniseg.StringWidth(string(word)) + spaces
		} else {
			lineW += pending
		}
		word = nil
		spaces = 0
	}
	for _, r := range runes {
		if r == ' ' || r == '\t' {
			if len(word) > 0 || spaces > 0 {
				spaces++
			}
		} else {
			if spaces > 0 {
				flush()
			}
			word = append(word, r)
			// hard-wrap a single word that fills the entire width
			lastW := rw.RuneWidth(word[len(word)-1])
			if uniseg.StringWidth(string(word))+lastW > width {
				if lineW > 0 {
					rows++
					lineW = 0
				}
				lineW += uniseg.StringWidth(string(word))
				word = nil
			}
		}
	}
	// flush remaining word+spaces
	pending := uniseg.StringWidth(string(word)) + spaces
	if lineW+pending >= width {
		rows++
	}
	return rows
}

// setViewportContent rebuilds the full viewport content:
// transcript + separator + input area. The input area scrolls with the transcript.
func (m *model) setViewportContent() {
	rows := m.taRows()
	if m.ta.Height() != rows {
		m.ta.SetHeight(rows)
	}
	vw := m.vpWidth()
	sep := styleBorder.Width(vw).Render("")
	transcript := m.wrappedTranscript()
	content := transcript + "\n" + sep + "\n" + m.colorizeInput(m.ta.View())
	m.vp.SetContent(content)
}

// welcomeScreen returns a centered welcome message shown when the transcript is empty.
func (m *model) welcomeScreen() string {
	vpH := m.vp.Height
	if vpH <= 0 {
		vpH = m.viewportHeight()
	}
	localAvail := m.agents.localAvail
	claudeAvail := m.agents.claudeAvail

	lines := []string{
		pulseColors[8] + "◈" + ansiReset + " " + "\033[1;38;2;255;208;96mmilk\033[0m",
		dim("switch models, not context."),
		"",
	}

	switch {
	case !m.hasLocalAgentConfig:
		// No provider configured at all — show setup guidance regardless of Claude.
		lines = append(lines,
			yellow("no local agent configured"),
			"",
			dim("quickstart — add a backend with /provider add:"),
			"",
			dim("llama.cpp · Ollama"),
			"› /provider add url=http://localhost:8080 provider=local model=qwen2.5-coder",
			"",
			dim("AWS Bedrock"),
			"› /provider add url=https://bedrock-runtime.<region>.amazonaws.com provider=bedrock model=<arn>",
			"",
			dim("OpenRouter · Together · Groq"),
			"› /provider add url=https://openrouter.ai/api/v1 provider=bearer api_key=<key> model=<id>",
			"",
		)
		if !claudeAvail {
			lines = append(lines,
				dim("claude CLI not found — install Claude Code to enable escalation"),
				"",
			)
		}
		lines = append(lines, dim("/help for all commands"))
	case !localAvail && !claudeAvail:
		lines = append(lines,
			yellow("no agents available"),
			"",
			dim("local agent unreachable — check your provider config with /provider"),
			dim("claude CLI not found — install Claude Code to enable escalation"),
			"",
			dim("/help for available commands"),
		)
	case !localAvail:
		lines = append(lines,
			dim("type a message and press Enter to start"),
			dim("local agent unreachable — use /provider to check or switch backends"),
			dim("/help for available commands"),
		)
	case !claudeAvail:
		lines = append(lines,
			dim("type a message and press Enter to start"),
			dim("claude CLI not found — escalation unavailable"),
			dim("/help for available commands"),
		)
	default:
		lines = append(lines,
			dim("type a message and press Enter to start"),
			dim("/help for available commands"),
		)
	}

	padTop := (vpH - len(lines)) / 2
	if padTop < 0 {
		padTop = 0
	}
	var sb strings.Builder
	for i := 0; i < padTop; i++ {
		sb.WriteString("\n")
	}
	centered := lipgloss.NewStyle().Width(m.vpWidth()).Align(lipgloss.Center)
	for _, l := range lines {
		sb.WriteString(centered.Render(l))
		sb.WriteString("\n")
	}
	return sb.String()
}

// appendTranscript adds text to the transcript.
// Sticky-bottom: only auto-scrolls when already at the bottom.
func (m *model) appendTranscript(text string) {
	m.transcript.WriteString(text)
	if m.ready {
		atBottom := m.vp.AtBottom()
		m.setViewportContent()
		if atBottom {
			m.vp.GotoBottom()
		}
	}
}

// colorizeLineThresh is the number of new lines that must accumulate before
// a mid-stream re-colorization is triggered. Keeps chroma/glamour from running
// on every individual streamed token.
const colorizeLineThresh = 8

// wrappedTranscript returns the transcript (or welcome screen) word-wrapped to
// the viewport content width. When a selection range is active, the selected
// text region is highlighted with an inverted background, respecting column
// boundaries on the first and last lines.
func (m *model) wrappedTranscript() string {
	if m.transcript.Len() == 0 {
		return m.applySelectionHighlight(m.welcomeScreen())
	}
	vw := m.vpWidth()
	raw := m.transcript.String()
	if vw <= 0 {
		return raw
	}
	if m.colorizeMode == ColorizeOff {
		return m.applySelectionHighlight(ansi.Wrap(raw, vw, ""))
	}

	txLen := m.transcript.Len()
	vpOffset := m.vp.YOffset

	// Check cache validity: skip heavy colorization if viewport and content
	// position haven't changed significantly since the last full render.
	vpChanged := vw != m.colorizeVPWidth || vpOffset != m.colorizeVPOffset
	txGrew := txLen - m.colorizeTransLen

	// Count new lines since last re-colorize to decide if threshold is met.
	newLines := 0
	if txGrew > 0 {
		newLines = strings.Count(raw[m.colorizeTransLen:], "\n")
		m.colorizeLinesSeen += newLines
	}

	if !m.colorizeForce && !vpChanged && m.colorizeCached != "" && m.colorizeLinesSeen < colorizeLineThresh {
		// Return cached result — append plain-wrapped new text as a fast suffix
		// so the user sees new content immediately even without re-colorizing.
		if txGrew > 0 {
			newText := ansi.Wrap(raw[m.colorizeTransLen:], vw, "")
			return m.applySelectionHighlight(m.colorizeCached + newText)
		}
		return m.applySelectionHighlight(m.colorizeCached)
	}

	// Full re-colorize.
	m.colorizeForce = false
	m.colorizeLinesSeen = 0
	plainWrapped := ansi.Wrap(raw, vw, "")
	colorized := colorizeTranscriptWrapped(plainWrapped, m.colorizeMode)

	// Update cache.
	m.colorizeCached = colorized
	m.colorizeTransLen = txLen
	m.colorizeVPOffset = vpOffset
	m.colorizeVPWidth = vw

	return m.applySelectionHighlight(colorized)
}

// applySelectionHighlight applies the selection background highlight to the
// given content string. Returns content unchanged if no selection is active.
func (m *model) applySelectionHighlight(content string) string {
	if m.selAnchorLine < 0 || m.selEndLine < 0 {
		return content
	}
	loLine, loCol := m.selAnchorLine, m.selAnchorCol
	hiLine, hiCol := m.selEndLine, m.selEndCol
	if hiLine < loLine || (hiLine == loLine && hiCol < loCol) {
		loLine, loCol, hiLine, hiCol = hiLine, hiCol, loLine, loCol
	}
	lines := strings.Split(content, "\n")
	selStyle := lipgloss.NewStyle().Reverse(true)
	for i := range lines {
		if i < loLine || i > hiLine {
			continue
		}
		plain := []rune(ansi.Strip(lines[i]))
		start, end := 0, len(plain)
		if i == loLine {
			if loCol < len(plain) {
				start = loCol
			} else {
				start = len(plain)
			}
		}
		if i == hiLine {
			if hiCol < len(plain) {
				end = hiCol
			}
		}
		if start > end {
			start = end
		}
		before := string(plain[:start])
		sel := selStyle.Render(string(plain[start:end]))
		after := string(plain[end:])
		lines[i] = before + sel + after
	}
	return strings.Join(lines, "\n")
}

// viewportHeight is the full terminal height minus the header bar, two separator newlines, status bar, and hint lines.
// View() layout: headerBar + "\n" + mainArea + "\n" + statusBar → chrome = 1+1+1+1 = 4 lines.
func (m *model) viewportHeight() int {
	h := m.height - 4 - len(m.tabHints)
	return max(h, 3)
}

// mainWidth returns the width available for the transcript+input area.
// When the memory panel is open it is reduced by the panel width.
func (m *model) mainWidth() int {
	w := m.width
	if m.panelMemory {
		w -= memoryPanelWidth
	}
	if w < 20 {
		w = 20
	}
	return w
}

// vpWidth is the viewport content width: mainWidth minus 1 column reserved for the scrollbar.
func (m *model) vpWidth() int {
	return m.mainWidth() - 1
}

// syncLayout rebuilds viewport content after textarea size changes.
// Sticky-bottom: scrolls to bottom only when already there.
func (m *model) syncLayout() {
	if !m.ready {
		return
	}
	vw := m.vpWidth()
	vpH := m.viewportHeight()
	atBottom := m.vp.AtBottom()
	if m.vp.Width != vw {
		m.vp.Width = vw
		m.colorizeForce = true // width changed — rewrap and re-colorize
	}
	if m.vp.Height != vpH {
		m.vp.Height = vpH
	}
	m.setViewportContent()
	if atBottom {
		m.vp.GotoBottom()
	}
}

// headerBar renders the persistent application header.
// Left: animated logo + tagline. Right: repo link + session info + /help hint.
func (m *model) headerBar() string {
	frame := 8 // static peak (bright gold) when idle
	if m.busy {
		frame = m.spinnerFrame
	}
	logo := headerLogo(frame)
	tagline := dim("switch models, not context.")
	taglinePlain := "switch models, not context."

	sessID := m.st.sess.ID
	if len(sessID) > 8 {
		sessID = sessID[:8]
	}
	ac := m.st.cfg.ActiveLocalAgent()
	model := ac.Name
	if model == "" {
		model = ac.Model
	}
	if model == "" {
		model = "local"
	}
	const repoURL = "github.com/scoutme/milk"
	rightFull := dim(repoURL + "  sess:" + sessID + "  model:" + model + "  /help")
	rightFulPlain := repoURL + "  sess:" + sessID + "  model:" + model + "  /help"
	rightShort := dim("sess:" + sessID + "  /help")
	rightShortPlain := "sess:" + sessID + "  /help"

	logoPlain := stripANSI(logo)
	available := m.width - 2
	rightPart, rightPlain := rightFull, rightFulPlain
	if available < len(logoPlain)+2+len(taglinePlain)+2+len(rightFulPlain) {
		rightPart, rightPlain = rightShort, rightShortPlain
	}
	left := " " + logo + "  " + tagline
	leftPlain := " " + logoPlain + "  " + taglinePlain
	gap := max(available-len(leftPlain)-len(rightPlain), 1)
	bar := left + strings.Repeat(" ", gap) + rightPart + " "
	if isTTY {
		return styleHeaderBar.Width(m.width).Render(bar)
	}
	return bar
}

// statusBar renders the one-line status bar.
func (m *model) statusBar() string {
	sessID := m.st.sess.ID
	if len(sessID) > 8 {
		sessID = sessID[:8]
	}
	left := fmt.Sprintf(" %s  %s  %s", dim("session:"+sessID), dim("state:"+string(m.st.sess.State)), dim("agent:")+m.statusAgent())
	right := dim(m.statusCwd() + " ")
	if m.credRefreshing {
		left += dim(" [refreshing " + m.credLabel + " credentials…]")
	} else if m.credStatus != "" {
		if m.credOK {
			left += dim(" [" + m.credLabel + " creds: " + m.credStatus + "]")
		} else {
			left += yellow(" [" + m.credLabel + " creds failed: " + m.credStatus + "]")
		}
	}
	if m.quitPending {
		left += yellow(" [press ctrl+c again to exit]")
	} else if m.busyHint != "" {
		left += yellow(" [" + m.busyHint + "]")
	} else if m.copyFeedback != "" {
		left += green(" [" + m.copyFeedback + "]")
	} else if m.taSelAnchor >= 0 && m.taSelEnd >= 0 && m.taSelAnchor != m.taSelEnd {
		n := len([]rune(m.taSelText()))
		left += yellow(fmt.Sprintf(" [%d chars selected — ctrl+c copy · ctrl+x cut · del delete · type to replace]", n))
	} else if m.selAnchorLine >= 0 && m.selDragging {
		var selStatus string
		if m.selText != "" {
			selStatus = yellow(fmt.Sprintf(" [%d chars — ctrl+c / right-click to copy]", len([]rune(m.selText))))
		} else {
			selStatus = yellow(fmt.Sprintf(" [selecting: line %d col %d — release to end]", m.selAnchorLine+1, m.selAnchorCol+1))
		}
		left += selStatus
	}
	gap := max(m.width-len(stripANSI(left))-len(ansi.Strip(right)), 1)
	bar := left + strings.Repeat(" ", gap) + right
	if isTTY {
		if m.pendingPerm != nil {
			return styleStatusBarPerm.Width(m.width).Render(bar)
		}
		return styleStatusBar.Width(m.width).Render(bar)
	}
	return bar
}

func (m *model) statusAgent() string {
	if m.searching {
		label := "reverse-i-search"
		if m.searchForward {
			label = "forward-i-search"
		}
		return dim("(" + label + ")`" + m.searchQuery.String() + "'")
	}
	agent := dim(agentLabel(m.st))
	if m.pendingPerm != nil {
		return "? " + agent + " [allow?]"
	}
	if m.busy {
		frame := yellow(bold(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]))
		pulsed := pulse(agentLabel(m.st), m.spinnerFrame)
		if m.activeToolUse != "" {
			return frame + " " + pulsed + dim(" ["+m.activeToolUse+"]")
		}
		return frame + " " + pulsed
	}
	return agent
}

func agentLabel(st *interactiveState) string {
	localName := st.cfg.ActiveLocalAgent().Name
	if localName == "" {
		localName = "local"
	}
	switch {
	case st.stickyEscalate:
		return "claude (pinned)"
	case st.forceEscalate:
		return "claude (forced)"
	case st.stickyLocal:
		return localName + " (pinned)"
	case st.forceLocal:
		return localName + " (forced)"
	case st.sess.State == session.StateClaude || st.sess.State == session.StateClaudeWaiting:
		return "claude"
	default:
		return localName
	}
}

func (m *model) statusCwd() string {
	cwd := m.st.cwd
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, cwd); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return cwd
}

// --- Init ---

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		textarea.Blink,
		tea.EnableBracketedPaste,
		tea.EnterAltScreen,
	}
	if m.panelMemory {
		cmds = append(cmds, memoryPollTick())
	}
	return tea.Batch(cmds...)
}

// --- Update ---

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleResize(msg)

	case tea.KeyMsg:
		if m.pendingPerm != nil {
			return m.handlePermKey(msg)
		}
		if m.pendingForget != nil {
			return m.handleForgetKey(msg)
		}
		if m.pendingAdd != nil {
			return m.handleAddProviderKey(msg)
		}
		if m.inputLocked() {
			return m.handleBusyKey(msg)
		}
		return m.handleKey(msg)

	case permRequestMsg:
		return m.handlePermRequest(msg)

	case toolUseMsg:
		m.activeToolUse = msg.name
		return m, nil

	case chunkMsg:
		m.appendTranscript(msg.text)
		return m, nil

	case agentDoneMsg:
		return m.handleAgentDone(msg)

	case spinnerTickMsg:
		if m.busy {
			m.spinnerFrame++
			return m, spinnerTick()
		}
		return m, nil

	case copyFeedbackClearMsg:
		m.copyFeedback = ""
		return m, nil

	case busyHintClearMsg:
		m.busyHint = ""
		return m, nil

	case credRefreshReadyMsg:
		m.credRefreshing = false
		m.credLabel = msg.label
		if msg.err != nil {
			m.credStatus = msg.err.Error()
			m.credOK = false
		} else {
			m.credStatus = "ok"
			m.credOK = true
			if msg.creds != nil {
				// AWS: apply fresh credentials and rebuild the local agent.
				ac := m.st.cfg.ActiveLocalAgent()
				ac.AWSKeyID = msg.creds.AccessKeyID
				ac.AWSSecret = msg.creds.SecretAccessKey
				ac.AWSToken = msg.creds.SessionToken
				newAgent := local.NewFromConfig(ac)
				if od, err := config.OtelDir(); err == nil {
					newAgent.WithOtelDir(od)
				}
				m.agents.local = newAgent
				m.agents.localAvail = newAgent.Ping(m.ctx) == nil
				m.rtr = router.New(m.st.cfg, newAgent)
			}
			// For token_cmd providers the transport already holds the token
			// internally; no agent rebuild is needed.
		}
		return m, nil

	case quitPendingClearMsg:
		m.quitPending = false
		return m, nil

	case memoryRefreshMsg:
		if m.panelMemory {
			return m, memoryPollTick()
		}
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	}

	// Pass remaining messages to viewport and textarea.
	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)
	cmd = m.updateTA(msg)
	if _, isMouseMsg := msg.(tea.MouseMsg); !isMouseMsg {
		m.syncLayout()
	}
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m model) handleResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height

	vw := m.vpWidth()
	vpH := m.viewportHeight()
	if !m.ready {
		m.vp = viewport.New(vw, vpH)
		m.ready = true
		m.refreshPrompt()
		m.setViewportContent()
		m.vp.GotoBottom()
	} else {
		atBottom := m.vp.AtBottom()
		m.vp.Width = vw
		m.vp.Height = vpH
		m.refreshPrompt()
		m.setViewportContent()
		if atBottom {
			m.vp.GotoBottom()
		}
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Bracketed paste — let the textarea handle it directly.
	// Pre-expand to terminal height so repositionView() inside ta.Update never
	// scrolls on a large paste (updateTA's +1 is insufficient for multi-line pastes).
	if msg.Paste {
		m.undoPush(false) // paste is always its own undo step; updateTA will skip (same value)
		m.ta.SetHeight(m.height)
		var cmd tea.Cmd
		cmd = m.updateTA(msg)
		m.syncLayout()
		return m, cmd
	}

	// Any key other than ctrl+c cancels a pending quit confirmation.
	if m.quitPending && msg.String() != "ctrl+c" {
		m.quitPending = false
	}

	// ctrl+r search mode: intercept most keys.
	if m.searching {
		return m.handleSearchKey(msg)
	}

	switch msg.String() {
	case "ctrl+z":
		undoDebugLog("KEY ctrl+z val=%q undoStack=%d redoStack=%d lastUndoValue=%q", m.ta.Value(), len(m.undoStack), len(m.redoStack), m.lastUndoValue)
		if m.undoApply(&m.undoStack, &m.redoStack) {
			m.syncLayout()
		}
		return m, nil
	case "ctrl+y":
		undoDebugLog("KEY ctrl+y val=%q undoStack=%d redoStack=%d lastUndoValue=%q", m.ta.Value(), len(m.undoStack), len(m.redoStack), m.lastUndoValue)
		if m.undoApply(&m.redoStack, &m.undoStack) {
			m.syncLayout()
		}
		return m, nil
	case "esc":
		if m.taSelAnchor >= 0 {
			m.taClearSel()
			m.setViewportContent()
			return m, nil
		}
		if m.selAnchorLine >= 0 {
			m.clearSelection()
			m.setViewportContent()
			return m, nil
		}
	case "ctrl+c":
		return m.handleCtrlC()
	case "ctrl+d":
		if m.ta.Value() == "" {
			return m, tea.Quit
		}
	case "ctrl+r":
		m.searching = true
		m.searchForward = false
		m.searchQuery.Reset()
		m.searchIdx = -1
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	case "ctrl+s":
		m.searching = true
		m.searchForward = true
		m.searchQuery.Reset()
		m.searchIdx = -1
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	case "enter":
		return m.handleEnter()
	case "up":
		li := m.ta.LineInfo()
		if m.ta.Line() == 0 && li.RowOffset == 0 {
			m = m.historyBack()
			m.syncLayout()
			return m, nil
		}
	case "down":
		li := m.ta.LineInfo()
		if m.ta.Line() == m.ta.LineCount()-1 && li.RowOffset == li.Height-1 {
			m = m.historyForward()
			m.syncLayout()
			return m, nil
		}
	case "ctrl+up":
		m = m.historyBack()
		m.syncLayout()
		return m, nil
	case "ctrl+down":
		m = m.historyForward()
		m.syncLayout()
		return m, nil
	case "shift+left", "shift+right", "shift+up", "shift+down", "shift+home", "shift+end",
		"shift+ctrl+left", "shift+ctrl+right", "shift+alt+left", "shift+alt+right":
		return m.handleShiftArrow(msg)
	case "tab":
		m = m.handleTab()
		return m, nil
	case "pgup", "ctrl+u":
		m.vp.HalfPageUp()
		return m, nil
	case "pgdown", "ctrl+f":
		m.vp.HalfPageDown()
		return m, nil
	}

	// Non-Tab key resets tab cycling
	m.tabMatches = nil
	m.tabIdx = -1
	m.tabPrefix = ""
	m.tabHints = nil

	// When a keyboard selection is active, special keys act on it.
	if m.taSelText() != "" {
		switch msg.String() {
		case "ctrl+x":
			// Cut: copy then delete.
			t := m.taSelText()
			copyToClipboard(t)
			m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(t)))
			m = m.taDeleteSelection()
			m.syncLayout()
			return m, copyFeedbackClearCmd()
		case "backspace", "delete", "ctrl+h":
			// Delete selection without copying.
			m = m.taDeleteSelection()
			m.syncLayout()
			return m, nil
		default:
			// Any printable key replaces the selection.
			if len(msg.Runes) > 0 || msg.Type == tea.KeySpace {
				m = m.taDeleteSelection()
				// Fall through to let the textarea insert the typed key.
			}
		}
	}

	// Any non-shift key clears the keyboard selection.
	m.taClearSel()

	var cmd tea.Cmd
	m.undoPush(true)
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// handleShiftArrow manages keyboard selection in the input textarea.
// Shift+Arrow keys extend the selection; the anchor is set on the first shift press.
// The bare direction key is forwarded to the textarea to move the cursor.
func (m model) handleShiftArrow(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Set anchor at current cursor position before moving.
	if m.taSelAnchor < 0 {
		m.taSelAnchor = m.taCursorOffset()
	}

	// Map shift+arrow → bare direction for the textarea.
	var bareKey tea.KeyMsg
	switch msg.String() {
	case "shift+left":
		bareKey = tea.KeyMsg{Type: tea.KeyLeft}
	case "shift+right":
		bareKey = tea.KeyMsg{Type: tea.KeyRight}
	case "shift+up":
		bareKey = tea.KeyMsg{Type: tea.KeyUp}
	case "shift+down":
		bareKey = tea.KeyMsg{Type: tea.KeyDown}
	case "shift+home":
		bareKey = tea.KeyMsg{Type: tea.KeyHome}
	case "shift+end":
		bareKey = tea.KeyMsg{Type: tea.KeyEnd}
	case "shift+ctrl+left", "shift+alt+left":
		bareKey = tea.KeyMsg{Type: tea.KeyLeft, Alt: true}
	case "shift+ctrl+right", "shift+alt+right":
		bareKey = tea.KeyMsg{Type: tea.KeyRight, Alt: true}
	default:
		bareKey = tea.KeyMsg{Type: tea.KeyRight}
	}

	cmd := m.updateTA(bareKey)
	m.taSelEnd = m.taCursorOffset()
	m.syncLayout()
	return m, cmd
}

func (m model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.selText != "" {
		copyToClipboard(m.selText)
		m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(m.selText)))
		m.clearSelection()
		m.setViewportContent()
		return m, copyFeedbackClearCmd()
	}
	if t := m.taSelText(); t != "" {
		copyToClipboard(t)
		m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(t)))
		m.taClearSel()
		m.setViewportContent()
		return m, copyFeedbackClearCmd()
	}
	if m.ta.Value() != "" {
		m.ta.Reset()

		m.tabMatches = nil
		m.tabIdx = -1
		m.tabPrefix = ""
		m.tabHints = nil
		m.syncLayout()
		return m, nil
	}
	if m.quitPending {
		return m, tea.Quit
	}
	m.quitPending = true
	return m, quitPendingClearCmd()
}

func (m model) handleEnter() (tea.Model, tea.Cmd) {
	input := strings.TrimSpace(m.ta.Value())
	m.ta.Reset()

	m.tabMatches = nil
	m.tabIdx = -1
	m.tabPrefix = ""
	m.tabHints = nil
	m.syncLayout()
	m.histIdx = -1
	m.saved = ""
	if input == "" {
		return m, nil
	}

	// Append echo to transcript
	label := promptLabel(m.st)
	m.appendTranscript(label + colorizeTokens(input) + "\n")

	// Append to both histories (deduped)
	m.sessionHistory = appendDeduped(m.sessionHistory, input, maxPersistedHistory)
	m.globalHistory = appendDeduped(m.globalHistory, input, maxPersistedHistory)

	if input == cmdPaste {
		m.appendTranscript(dim("[milk]") + " hint: paste multi-line text directly, or use Ctrl+N / Shift+Alt+Enter to insert a newline\n")
		return m, nil
	}

	if cmd, rest, found := extractSlashCommand(input); found {
		return m.handleSlashInput(cmd, rest)
	}

	return m.dispatchAgent(input)
}

func (m model) handleSlashInput(cmd, rest string) (tea.Model, tea.Cmd) {
	if cmd == cmdHistory {
		return m.handleHistoryCmd(strings.TrimSpace(rest)), nil
	}
	if cmd == cmdPanel {
		return m.handlePanelCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdForget {
		return m.handleForgetCmd(strings.TrimSpace(rest)), nil
	}
	if cmd == cmdProvider {
		return m.handleProviderCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdColorize {
		return m.handleColorizeCmd(strings.TrimSpace(rest)), nil
	}
	exit, dispatch, output := handleSlashCommand(cmd, rest, m.st)
	m.refreshPrompt()
	if exit {
		return m, tea.Quit
	}
	if output != "" {
		m.colorizeForce = true // slash command output may be large — force full re-colorize
		m.appendTranscript(output + "\n")
	}
	if dispatch != "" {
		return m.dispatchAgent(dispatch)
	}
	return m, nil
}

// handleColorizeCmd handles `/colorize [off|fenced|balanced|full]`.
// With no arg: shows the current mode. With a valid mode: switches live and saves config.
func (m model) handleColorizeCmd(arg string) model {
	output := execColorize(arg, m.st)
	if arg != "" {
		// Update the live model colorize mode so the change takes effect immediately
		// (only for valid modes; invalid args are reported by execColorize).
		validModes := map[string]bool{"off": true, "fenced": true, "balanced": true, "full": true}
		if validModes[arg] {
			m.colorizeMode = ParseColorizeMode(arg)
			m.colorizeForce = true
		}
	}
	m.appendTranscript(output + "\n")
	return m
}

// handleForgetCmd starts a /forget flow: searches for candidates and
// either deletes immediately (single exact #id match) or prompts.
func (m model) handleForgetCmd(pat string) model {
	if pat == "" {
		m.appendTranscript(milkTag() + " usage: /forget <description> or /forget #<id>\n")
		return m
	}
	if m.mem == nil {
		m.appendTranscript(milkTag() + " memory store not available\n")
		return m
	}

	var candidates []memory.Percept
	if strings.HasPrefix(pat, "#") {
		candidates = m.mem.FindByIDPrefix(pat[1:])
	} else {
		candidates = m.mem.List(memory.ListOpts{Pattern: pat})
	}

	if len(candidates) == 0 {
		m.appendTranscript(milkTag() + " no percepts match " + fmt.Sprintf("%q", pat) + "\n")
		return m
	}

	if len(candidates) == 1 {
		// Single match: show and ask y/N
		m.appendTranscript(forgetCandidateList(candidates))
		m.appendTranscript(milkTag() + " delete this percept? [y/N] ")
		m.pendingForget = &forgetState{candidates: candidates}
		return m
	}

	// Multiple matches: show numbered list
	m.appendTranscript(forgetCandidateList(candidates))
	m.appendTranscript(milkTag() + " enter position (1-" + fmt.Sprintf("%d", len(candidates)) + "), #id, or empty to cancel: ")
	m.pendingForget = &forgetState{candidates: candidates}
	return m
}

// handleProviderCmd handles `/provider [list|switch <name>|add [key=val ...]]`.
func (m model) handleProviderCmd(arg string) (model, tea.Cmd) {
	// Re-read config so externally added providers are visible.
	if fresh, err := config.Load(); err == nil {
		// Preserve the in-session active selection if the user hasn't changed it.
		if m.st.cfg.LocalAgent != "" {
			fresh.LocalAgent = m.st.cfg.LocalAgent
		}
		m.st.cfg = fresh
	}

	switch {
	case arg == "" || arg == "status":
		m.appendTranscript(execProvider(m.st) + "\n")

	case arg == "list":
		m.appendTranscript(execProviderList(m.st) + "\n")

	case strings.HasPrefix(arg, "switch "):
		name := strings.TrimSpace(arg[len("switch "):])
		if name == "" {
			m.appendTranscript(milkTag() + " usage: /provider switch <name>\n")
			return m, nil
		}
		found := false
		for _, a := range m.st.cfg.LocalAgents {
			if strings.EqualFold(a.Name, name) {
				found = true
				break
			}
		}
		if !found {
			var names []string
			for _, a := range m.st.cfg.LocalAgents {
				names = append(names, a.Name)
			}
			m.appendTranscript(fmt.Sprintf("%s unknown agent %q — available: %s\n",
				milkTag(), name, strings.Join(names, ", ")))
			return m, nil
		}
		m.st.cfg.LocalAgent = name
		if err := config.Save(m.st.cfg); err != nil {
			m.appendTranscript(fmt.Sprintf("%s warning: could not persist provider switch: %v\n", milkTag(), err))
		}
		// Build agent without blocking — creds are fetched async below.
		newAgent := local.NewFromConfig(m.st.cfg.ActiveLocalAgent())
		if od, err := config.OtelDir(); err == nil {
			newAgent.WithOtelDir(od)
		}
		m.agents.local = newAgent
		m.agents.localAvail = newAgent.Ping(m.ctx) == nil
		// Clear stale credential status from the previous provider.
		m.credStatus = ""
		m.credLabel = ""
		m.credOK = false
		m.appendTranscript(execProvider(m.st) + "\n")
		if newAgent.HasTokenCmd() {
			m.credRefreshing = true
			m.credLabel = "token"
			return m, func() tea.Msg {
				err := newAgent.WarmToken()
				return credRefreshReadyMsg{label: "token", err: err}
			}
		}
		if needsAWSRefresh(m.st.cfg) {
			m.credRefreshing = true
			m.credLabel = "AWS"
			ctx := m.ctx
			return m, func() tea.Msg {
				cmd := claudesettings.AWSAuthRefreshCommand()
				refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				creds, err := claude.ResolveAWSCredsContext(refreshCtx, cmd)
				return credRefreshReadyMsg{label: "AWS", creds: creds, err: err}
			}
		}
		m.credRefreshing = false
		return m, nil

	case strings.HasPrefix(arg, "add"):
		inline := strings.TrimSpace(arg[len("add"):])
		return m.startAddProvider(inline), nil

	default:
		m.appendTranscript(milkTag() + " usage: /provider [list|switch <name>|add [name=... url=... model=... provider=...]]\n")
	}
	return m, nil
}

// execProviderList formats all configured local-agent backends.
func execProviderList(st *interactiveState) string {
	agents := st.cfg.LocalAgents
	if len(agents) == 0 {
		// show the single backward-compat entry
		agents = []config.LocalAgentConfig{st.cfg.ActiveLocalAgent()}
	}
	active := strings.ToLower(strings.TrimSpace(st.cfg.ActiveLocalAgent().Name))
	var b strings.Builder
	fmt.Fprintf(&b, "%s local agents (%d):\n", milkTag(), len(agents))
	for _, a := range agents {
		marker := "  "
		if strings.EqualFold(a.Name, active) {
			marker = bold("* ")
		}
		provider := a.Provider
		if provider == "" {
			provider = "local"
		}
		fmt.Fprintf(&b, "%s%s  %s  %s  [%s]", marker, bold(a.Name), dim(a.URL), dim(a.Model), provider)
		if a.Name != agents[len(agents)-1].Name {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// startAddProvider handles `/provider add [key=val ...]`.
// Known keys: name, url, model, provider, api_key, aws_region.
// Missing required fields (name, url, model) are prompted interactively.
func (m model) startAddProvider(inline string) model {
	ac := parseProviderInlineArgs(inline)

	// If all required fields are present, add immediately.
	if ac.Name != "" && ac.URL != "" && ac.Model != "" {
		return m.commitAddProvider(ac)
	}

	// Otherwise start the wizard from the first missing required field.
	st := &addProviderState{ac: ac}
	st.step = firstMissingStep(ac)
	m.pendingAdd = st
	m.appendTranscript(addProviderPrompt(st.step) + " ")
	m.ta.Reset()
	return m
}

// parseProviderInlineArgs parses "key=val key2=val2 ..." into a LocalAgentConfig.
func parseProviderInlineArgs(s string) config.LocalAgentConfig {
	var ac config.LocalAgentConfig
	for _, tok := range strings.Fields(s) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		switch k {
		case "name":
			ac.Name = v
		case "url":
			ac.URL = v
		case "model":
			ac.Model = v
		case "provider":
			ac.Provider = v
		case "api_key":
			ac.APIKey = v
		case "aws_region":
			ac.AWSRegion = v
		}
	}
	return ac
}

// firstMissingStep returns the first wizard step that still needs input.
func firstMissingStep(ac config.LocalAgentConfig) addProviderStep {
	if ac.Name == "" {
		return addStepName
	}
	if ac.URL == "" {
		return addStepURL
	}
	if ac.Model == "" {
		return addStepModel
	}
	if ac.Provider == "" {
		return addStepProvider
	}
	p := strings.ToLower(ac.Provider)
	if p != "" && p != "local" && p != "bedrock" && ac.APIKey == "" {
		return addStepAPIKey
	}
	if p == "bedrock" && ac.AWSRegion == "" {
		return addStepAWSRegion
	}
	return addStepDone
}

// addProviderPrompt returns the prompt string for a wizard step.
func addProviderPrompt(step addProviderStep) string {
	switch step {
	case addStepName:
		return milkTag() + " name:"
	case addStepURL:
		return milkTag() + " url:"
	case addStepModel:
		return milkTag() + " model:"
	case addStepProvider:
		return milkTag() + " provider [local/bedrock/<bearer-name>, enter to skip]:"
	case addStepAPIKey:
		return milkTag() + " api_key:"
	case addStepAWSRegion:
		return milkTag() + " aws_region:"
	default:
		return ""
	}
}

// handleAddProviderKey handles keypresses during the /provider add wizard.
func (m model) handleAddProviderKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingAdd = nil
		m.appendTranscript("\n" + milkTag() + " cancelled\n")
		return m, nil
	case "enter":
		answer := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript(answer + "\n")

		st := m.pendingAdd
		switch st.step {
		case addStepName:
			if answer == "" {
				m.appendTranscript(milkTag() + " name is required\n" + addProviderPrompt(addStepName) + " ")
				return m, nil
			}
			st.ac.Name = answer
		case addStepURL:
			if answer == "" {
				m.appendTranscript(milkTag() + " url is required\n" + addProviderPrompt(addStepURL) + " ")
				return m, nil
			}
			st.ac.URL = answer
		case addStepModel:
			if answer == "" {
				m.appendTranscript(milkTag() + " model is required\n" + addProviderPrompt(addStepModel) + " ")
				return m, nil
			}
			st.ac.Model = answer
		case addStepProvider:
			st.ac.Provider = answer // empty = local, which is fine
		case addStepAPIKey:
			st.ac.APIKey = answer
		case addStepAWSRegion:
			st.ac.AWSRegion = answer
		}

		// Advance to next missing step.
		st.step = firstMissingStep(st.ac)
		if st.step == addStepDone {
			m.pendingAdd = nil
			m = m.commitAddProvider(st.ac)
		} else {
			m.appendTranscript(addProviderPrompt(st.step) + " ")
		}
		return m, nil
	}
	var cmd tea.Cmd
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// commitAddProvider appends the new agent to config, saves, and confirms.
func (m model) commitAddProvider(ac config.LocalAgentConfig) model {
	// Check for name collision.
	for _, existing := range m.st.cfg.LocalAgents {
		if strings.EqualFold(existing.Name, ac.Name) {
			m.appendTranscript(fmt.Sprintf("%s agent %q already exists — use /provider switch %s to activate it\n",
				milkTag(), ac.Name, ac.Name))
			return m
		}
	}
	isFirst := len(m.st.cfg.LocalAgents) == 0
	m.st.cfg.LocalAgents = append(m.st.cfg.LocalAgents, ac)
	if isFirst {
		m.st.cfg.LocalAgent = ac.Name
	}
	if err := config.Save(m.st.cfg); err != nil {
		m.appendTranscript(fmt.Sprintf("%s error saving config: %v\n", milkTag(), err))
		return m
	}
	m.hasLocalAgentConfig = true
	if isFirst {
		freshAC := applyFreshAWSCreds(m.st.cfg, m.st.cfg.ActiveLocalAgent())
		newAgent := local.NewFromConfig(freshAC)
		if od, err := config.OtelDir(); err == nil {
			newAgent.WithOtelDir(od)
		}
		m.agents.local = newAgent
		m.agents.localAvail = newAgent.Ping(m.ctx) == nil
		m.rtr = router.New(m.st.cfg, newAgent)
	}
	provider := ac.Provider
	if provider == "" {
		provider = "local"
	}
	m.appendTranscript(fmt.Sprintf("%s added agent %s  (%s | %s | %s)\n",
		milkTag(), bold(ac.Name), ac.URL, ac.Model, provider))
	if isFirst {
		m.appendTranscript(milkTag() + " activated as the default provider\n")
	} else {
		m.appendTranscript(milkTag() + " use /provider switch " + ac.Name + " to activate it\n")
	}
	return m
}

// forgetCandidateList formats the candidate list for display.
func forgetCandidateList(candidates []memory.Percept) string {
	var b strings.Builder
	for i, p := range candidates {
		shortID := "#" + p.ID[:6]
		scope := "global"
		// session percepts have no EngramID and their ID is set per store; we can't
		// distinguish here without passing scope through — use content length as proxy.
		// A cleaner approach would tag the Percept; for now show scope as unknown.
		_ = scope
		fmt.Fprintf(&b, "  %d. %s  %s\n", i+1, dim(shortID), p.Content)
	}
	return b.String()
}

// handleForgetKey handles keypresses while a /forget confirmation is pending.
func (m model) handleForgetKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingForget = nil
		m.appendTranscript("\n" + milkTag() + " cancelled\n")
		return m, nil
	case "enter":
		answer := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()

		m.syncLayout()
		m.appendTranscript(answer + "\n")
		return m.resolveForget(answer), nil
	}
	var cmd tea.Cmd
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// resolveForget interprets the user's answer and deletes the chosen percept.
func (m model) resolveForget(answer string) model {
	candidates := m.pendingForget.candidates
	m.pendingForget = nil

	var target *memory.Percept

	switch {
	case answer == "" || strings.ToLower(answer) == "n" || strings.ToLower(answer) == "no":
		m.appendTranscript(milkTag() + " cancelled\n")
		return m
	case len(candidates) == 1 && (strings.ToLower(answer) == "y" || strings.ToLower(answer) == "yes"):
		target = &candidates[0]
	case strings.HasPrefix(answer, "#"):
		prefix := answer[1:]
		for i := range candidates {
			if strings.HasPrefix(candidates[i].ID, prefix) {
				target = &candidates[i]
				break
			}
		}
	default:
		// Try numeric position
		var pos int
		if _, err := fmt.Sscanf(answer, "%d", &pos); err == nil && pos >= 1 && pos <= len(candidates) {
			target = &candidates[pos-1]
		}
	}

	if target == nil {
		m.appendTranscript(milkTag() + " unrecognised selection — cancelled\n")
		return m
	}

	ok, err := m.mem.Delete(target.ID)
	if err != nil {
		m.appendTranscript(fmt.Sprintf("%s error: %v\n", milkTag(), err))
		return m
	}
	if !ok {
		m.appendTranscript(milkTag() + " percept not found (already deleted?)\n")
		return m
	}
	m.appendTranscript(fmt.Sprintf("%s deleted percept %s\n", milkTag(), dim("#"+target.ID[:6])))
	return m
}

// handleSearchKey handles keypresses while ctrl+r search is active.
// Printable chars extend the query; ctrl+r searches again (older);
// backspace shrinks the query; enter/esc/ctrl+c accept and exit search.
func (m model) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+r":
		m.searchForward = false
		m = m.historySearchBack()
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	case "ctrl+s":
		m.searchForward = true
		m = m.historySearchForward()
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	case "ctrl+c", "esc":
		// Cancel search — restore saved text
		m.searching = false
		m.searchQuery.Reset()
		m.ta.SetValue(m.saved)
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	case "enter":
		// Accept current match
		m.searching = false
		m.searchQuery.Reset()
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	case "backspace", "ctrl+h":
		s := m.searchQuery.String()
		if len(s) > 0 {
			m.searchQuery.Reset()
			m.searchQuery.WriteString(s[:len(s)-1])
			m.searchIdx = -1
			m = m.historySearchBack()
		}
		m.syncLayout()
		return m, nil
	default:
		// Accept printable single-rune input
		if r := msg.String(); len(r) == 1 {
			if m.searchQuery.Len() == 0 {
				m.saved = m.ta.Value()
				m.searchIdx = -1
			}
			m.searchQuery.WriteString(r)
			m.searchIdx = -1
			m = m.historySearchBack()
			m.syncLayout()
		}
		return m, nil
	}
}

func (m model) handleHistoryCmd(sub string) model {
	switch sub {
	case "global":
		m.useGlobalHistory = true
		m.histIdx = -1
		m.appendTranscript(milkTag() + " history navigation: global\n")
	case "session":
		m.useGlobalHistory = false
		m.histIdx = -1
		m.appendTranscript(milkTag() + " history navigation: session\n")
	default:
		mode := "session"
		if m.useGlobalHistory {
			mode = "global"
		}
		m.appendTranscript(fmt.Sprintf("%s history mode: %s  (session: %d entries, global: %d entries)\n",
			milkTag(), mode, len(m.sessionHistory), len(m.globalHistory)))
	}
	return m
}

func (m model) handlePanelCmd(sub string) (tea.Model, tea.Cmd) {
	switch sub {
	case "memory":
		m.panelMemory = !m.panelMemory
		m.refreshPrompt()
		m.syncLayout()
		var tick tea.Cmd
		if m.panelMemory {
			m.appendTranscript(milkTag() + " memory panel: on\n")
			tick = memoryPollTick()
		} else {
			m.appendTranscript(milkTag() + " memory panel: off\n")
		}
		return m, tick
	default:
		m.appendTranscript(milkTag() + " usage: /panel memory\n")
		return m, nil
	}
}

func (m model) dispatchAgent(input string) (tea.Model, tea.Cmd) {
	m.busy = true
	m.spinnerFrame = 0

	turnCtx, cancel := context.WithCancel(m.ctx)
	m.cancelTurn = cancel

	st := m.st
	rtr := m.rtr
	agents := m.agents

	send := func(msg tea.Msg) { st.program.Send(msg) }
	st.toolFutures = map[string]chan string{}

	tuiAgents := agents
	ir0 := &tuiInputReader{send: send}
	tuiAgents.claude = agents.claude.
		WithSkipPermissions(st.skipPermissions).
		WithOnToolUse(func(name string) {
			send(toolUseMsg{name: name})
		}).
		WithOnToolUseReady(func(name string, input map[string]any) {
			summary := claudeToolArgSummary(input)
			var hint string
			if summary != "" {
				hint = fmt.Sprintf("\n\033[2m⚙ %s: %s\033[0m\n", name, summary)
			} else {
				hint = fmt.Sprintf("\n\033[2m⚙ %s\033[0m\n", name)
			}
			send(chunkMsg{text: hint})
		}).
		WithOnThinking(func(text string) { send(chunkMsg{text: dim(text)}) }).
		WithPermissionHandler(makeTUIPermissionHandler(ir0, st.cs))
	return m, tea.Batch(
		spinnerTick(),
		func() tea.Msg {
			defer cancel()
			sw := &sendWriter{send: send}
			err := runTurn(turnCtx, st, rtr, tuiAgents, input, sw, ir0)
			return agentDoneMsg{err: err}
		},
	)
}

// renderScrollbar returns a single-column string of h lines showing a dim │
// track with a bright ▌ thumb proportional to scroll position.
// When all content fits in the viewport, returns a blank column.
// renderSeparator renders the 1-column separator between the viewport and the
// memory panel (or right edge when the panel is closed).
//
// Visibility rules:
//   - panel open: always show a dim │ track; overlay ▌ thumb when scrollable
//   - panel closed + scrollable: show dim │ track with ▌ thumb
//   - panel closed + fits: blank column (no visual noise)
func (m *model) renderSeparator(h int) string {
	total := m.vp.TotalLineCount()
	scrollable := total > h
	visible := m.panelMemory || scrollable

	var rows []string
	if !visible {
		for range h {
			rows = append(rows, " ")
		}
		return strings.Join(rows, "\n")
	}

	var thumbTop, thumbBot int
	if scrollable {
		thumbTop, thumbBot = scrollThumb(h, total, m.vp.YOffset)
	}
	for i := range h {
		if scrollable && i >= thumbTop && i <= thumbBot {
			rows = append(rows, dim("▌"))
		} else {
			rows = append(rows, dim("│"))
		}
	}
	return strings.Join(rows, "\n")
}

// --- View ---

func (m model) View() string {
	if !m.ready {
		return ""
	}
	vpH := m.viewportHeight()
	sep := m.renderSeparator(vpH)
	mainArea := lipgloss.JoinHorizontal(lipgloss.Top, m.vp.View(), sep)
	if m.panelMemory {
		panel := m.renderMemoryPanel(vpH)
		pbar := m.renderPanelScrollbar(vpH)
		mainArea = lipgloss.JoinHorizontal(lipgloss.Top, mainArea, panel, pbar)
	}
	if len(m.tabHints) > 0 {
		return m.headerBar() + "\n" + mainArea + "\n" + strings.Join(m.tabHints, "\n") + "\n" + m.statusBar()
	}
	return m.headerBar() + "\n" + mainArea + "\n" + m.statusBar()
}

// --- Memory panel poll ---

func memoryPollTick() tea.Cmd {
	return tea.Tick(memoryPollInterval, func(time.Time) tea.Msg {
		return memoryRefreshMsg{}
	})
}

// --- Spinner ---

func spinnerTick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// --- History ---

// activeHistory returns the slice used for navigation (session or global).
func (m *model) activeHistory() []string {
	if m.useGlobalHistory {
		return m.globalHistory
	}
	return m.sessionHistory
}

func appendDeduped(h []string, entry string, max int) []string {
	if len(h) > 0 && h[len(h)-1] == entry {
		return h
	}
	h = append(h, entry)
	if len(h) > max {
		h = h[1:]
	}
	return h
}

func (m model) historyBack() model {
	h := m.activeHistory()
	if len(h) == 0 {
		return m
	}
	if m.histIdx == -1 {
		m.saved = m.ta.Value()
		m.histIdx = len(h) - 1
	} else if m.histIdx > 0 {
		m.histIdx--
	}
	m.ta.SetValue(h[m.histIdx])
	m.taClearSel()
	return m
}

func (m model) historyForward() model {
	h := m.activeHistory()
	if m.histIdx == -1 {
		return m
	}
	m.histIdx++
	if m.histIdx >= len(h) {
		m.histIdx = -1
		m.ta.SetValue(m.saved)
	} else {
		m.ta.SetValue(h[m.histIdx])
	}
	m.taClearSel()
	return m
}

// searchBack returns the index of the nearest match for q in h, searching
// backwards from start (exclusive). Returns -1 if no match found.
func searchBack(h []string, q string, start int) int {
	if start < 0 {
		start = len(h)
	}
	for i := start - 1; i >= 0; i-- {
		if strings.Contains(h[i], q) {
			return i
		}
	}
	return -1
}

// searchForward returns the index of the nearest match for q in h, searching
// forwards from start (exclusive). Returns -1 if no match found.
func searchForward(h []string, q string, start int) int {
	from := start + 1
	if start < 0 {
		from = 0
	}
	for i := from; i < len(h); i++ {
		if strings.Contains(h[i], q) {
			return i
		}
	}
	return -1
}

// historySearchBack finds the most recent entry in activeHistory() that contains
// searchQuery, starting from searchIdx-1 (or end if searchIdx==-1).
func (m model) historySearchBack() model {
	h := m.activeHistory()
	if len(h) == 0 || m.searchQuery.Len() == 0 {
		return m
	}
	if idx := searchBack(h, m.searchQuery.String(), m.searchIdx); idx >= 0 {
		m.searchIdx = idx
		m.ta.SetValue(h[idx])
	}
	return m
}

// historySearchForward finds the next (newer) entry in activeHistory() that
// contains searchQuery, starting from searchIdx+1.
func (m model) historySearchForward() model {
	h := m.activeHistory()
	if len(h) == 0 || m.searchQuery.Len() == 0 {
		return m
	}
	if idx := searchForward(h, m.searchQuery.String(), m.searchIdx); idx >= 0 {
		m.searchIdx = idx
		m.ta.SetValue(h[idx])
	}
	return m
}

// --- Tab completion ---

// cmdVariants is derived from interactiveHelp at init time so hints can never
// drift from the canonical help text.
var cmdVariants = buildCmdVariants()

type cmdVariant struct {
	sig  string // full signature, e.g. "/memory show <pat|#id>"
	desc string
}

// buildCmdVariants parses interactiveHelp and returns a map from bare slash
// command to its ordered list of variants (sig + desc).
func buildCmdVariants() map[string][]cmdVariant {
	result := map[string][]cmdVariant{}

	for line := range strings.SplitSeq(interactiveHelp, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "/") {
			continue
		}
		parts := strings.SplitN(trimmed, "  ", 2)
		if len(parts) < 2 {
			continue
		}
		sig := strings.TrimSpace(parts[0])
		desc := strings.TrimSpace(parts[1])
		if desc == "" {
			continue
		}
		cmd := strings.Fields(sig)[0]
		result[cmd] = append(result[cmd], cmdVariant{sig: sig, desc: desc})
	}
	return result
}

func (m model) handleTab() model {
	fullInput := m.ta.Value()
	lines := strings.Split(fullInput, "\n")
	curLine := m.ta.Line()
	if curLine >= len(lines) {
		curLine = len(lines) - 1
	}
	lineInput := lines[curLine]

	if len(m.tabMatches) == 0 || curLine != m.tabLine {
		m.tabMatches, m.tabIdx = buildTabMatches(lineInput, m.st.cwd)
		m.tabLine = curLine
		if len(m.tabMatches) == 0 {
			return m
		}
		// Capture what the user typed before completion — used to bold the
		// matching prefix in hints and the completed text in the textarea.
		m.tabPrefix = tabInputPrefix(lineInput)
	} else {
		m.tabIdx = (m.tabIdx + 1) % len(m.tabMatches)
	}
	completed := m.tabMatches[m.tabIdx]
	lines[curLine] = applyTabCompletion(lineInput, completed)
	m.ta.SetValue(strings.Join(lines, "\n"))
	m.ta.CursorEnd()

	// Build hint lines for the completed slash command.
	m.tabHints = nil
	if vs, ok := cmdVariants[completed]; ok {
		cycleIndicator := ""
		if len(m.tabMatches) > 1 {
			cycleIndicator = dim(fmt.Sprintf("  (%d/%d)", m.tabIdx+1, len(m.tabMatches)))
		}
		prefix := m.tabPrefix
		for i, v := range vs {
			suffix := ""
			if i == len(vs)-1 {
				suffix = cycleIndicator
			}
			// Bold the typed prefix within the sig; rest in normal yellow.
			sig := v.sig
			if prefix != "" && strings.HasPrefix(sig, prefix) {
				sig = boldYellow(prefix) + yellow(sig[len(prefix):])
			} else {
				sig = yellow(sig)
			}
			m.tabHints = append(m.tabHints, " "+sig+"  "+dim(v.desc)+suffix)
		}
	}

	m.syncLayout()
	return m
}

// tabInputPrefix extracts the slash-command prefix the user had typed in input.
// Uses the last /cmd token so that "/help foo /exp<TAB>" completes /exp, not /help.
// isSlashCmdToken returns true when w looks like a slash command token
// (/word…), as opposed to bare slashes or paths like "////".
func isSlashCmdToken(w string) bool {
	return len(w) >= 2 && w[0] == '/' && w[1] != '/'
}

func tabInputPrefix(input string) string {
	if input == "" || input[len(input)-1] == ' ' || input[len(input)-1] == '\t' {
		return ""
	}
	words := strings.Fields(input)
	if len(words) == 0 {
		return ""
	}
	last := words[len(words)-1]
	if isSlashCmdToken(last) || strings.HasPrefix(last, "@") {
		return last
	}
	return ""
}

// applyTabCompletion replaces the relevant token in input with completed,
// preserving all surrounding whitespace (no space collapsing).
func applyTabCompletion(input, completed string) string {
	if strings.HasPrefix(completed, "@") {
		result, found := replaceLastToken(input, func(w string) bool { return strings.HasPrefix(w, "@") }, completed)
		if found {
			return result
		}
		return completed
	}
	result, found := replaceLastToken(input, isSlashCmdToken, completed)
	if found {
		return result
	}
	return completed
}

// replaceLastToken finds the last token in input matching pred and replaces it
// with replacement, preserving all surrounding whitespace (no space collapsing).
// Returns the result and whether a matching token was found.
func replaceLastToken(input string, pred func(string) bool, replacement string) (string, bool) {
	lastStart, lastEnd := -1, -1
	i := 0
	for i < len(input) {
		// skip whitespace
		for i < len(input) && (input[i] == ' ' || input[i] == '\t') {
			i++
		}
		if i >= len(input) {
			break
		}
		// find token end
		j := i
		for j < len(input) && input[j] != ' ' && input[j] != '\t' {
			j++
		}
		w := input[i:j]
		if pred(w) {
			lastStart, lastEnd = i, j
		}
		i = j
	}
	if lastStart < 0 {
		return input, false
	}
	return input[:lastStart] + replacement + input[lastEnd:], true
}

func buildTabMatches(input, cwd string) ([]string, int) {
	// Never complete when the cursor is between words (trailing whitespace).
	if input == "" || input[len(input)-1] == ' ' || input[len(input)-1] == '\t' {
		return nil, 0
	}
	words := strings.Fields(input)
	if len(words) == 0 {
		return nil, 0
	}
	// Only complete the last word — the token the cursor is actively on.
	last := words[len(words)-1]
	if strings.HasPrefix(last, "@") {
		pathPrefix := last[1:]
		matches := expandPath(pathPrefix, cwd)
		atMatches := make([]string, len(matches))
		for j, m := range matches {
			atMatches[j] = "@" + m
		}
		return atMatches, 0
	}
	if !isSlashCmdToken(last) {
		return nil, 0
	}
	var matches []string
	for _, cmd := range slashCommands {
		if strings.HasPrefix(cmd, last) {
			matches = append(matches, cmd)
		}
	}
	return matches, 0
}

func expandPath(prefix, cwd string) []string {
	base := prefix
	if !strings.HasPrefix(base, "/") {
		base = cwd + "/" + base
	}
	dir := filepath.Dir(base)
	namePrefix := filepath.Base(base)
	if strings.HasSuffix(prefix, "/") {
		dir = base
		namePrefix = ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var matches []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), namePrefix) {
			rel := filepath.Join(dir, e.Name())
			if !strings.HasPrefix(prefix, "/") {
				rel, _ = filepath.Rel(cwd, rel)
			}
			if e.IsDir() {
				rel += "/"
			}
			matches = append(matches, rel)
		}
	}
	return matches
}

// --- Undo/redo ---

// undoPush saves the current textarea state before a mutation.
// Consecutive single-character edits within undoCoalesceWindow are coalesced
// into a single undo step so that Ctrl+Z undoes a word, not a character.
func (m *model) undoPush(coalesce bool) {
	val := m.ta.Value()
	now := time.Now()
	if coalesce && len(m.undoStack) > 0 && val == m.lastUndoValue &&
		now.Sub(m.lastUndoTime) < undoCoalesceWindow {
		undoDebugLog("undoPush COALESCE-SKIP coalesce=%v val=%q lastUndoValue=%q stack=%d", coalesce, val, m.lastUndoValue, len(m.undoStack))
		return
	}
	if val == m.lastUndoValue {
		undoDebugLog("undoPush SAME-VALUE-SKIP val=%q stack=%d", val, len(m.undoStack))
		return
	}
	entry := undoEntry{value: val, cursor: m.taCursorOffset()}
	m.undoStack = append(m.undoStack, entry)
	if len(m.undoStack) > undoMaxDepth {
		m.undoStack = m.undoStack[1:]
	}
	m.redoStack = m.redoStack[:0]
	m.lastUndoValue = val
	m.lastUndoTime = now
	undoDebugLog("undoPush PUSHED coalesce=%v val=%q cursor=%d stack=%d", coalesce, val, entry.cursor, len(m.undoStack))
}

// undoApply pops from src, pushes current state to dst, restores the entry.
// Cursor is placed at the end of the restored value (textarea default after SetValue).
func (m *model) undoApply(src, dst *[]undoEntry) bool {
	if len(*src) == 0 {
		undoDebugLog("undoApply EMPTY-STACK src=%d dst=%d", len(*src), len(*dst))
		return false
	}
	cur := undoEntry{value: m.ta.Value(), cursor: m.taCursorOffset()}
	entry := (*src)[len(*src)-1]
	*src = (*src)[:len(*src)-1]
	*dst = append(*dst, cur)
	undoDebugLog("undoApply RESTORE cur=%q entry=%q src=%d dst=%d lastUndoValue=%q", cur.value, entry.value, len(*src), len(*dst), m.lastUndoValue)
	m.ta.SetValue(entry.value)
	m.ta.CursorEnd()
	m.lastUndoValue = entry.value
	m.taClearSel()
	return true
}

// --- Textarea helpers ---

func (m *model) updateTA(msg tea.Msg) tea.Cmd {
	// Pre-expand the textarea by one row before the update so that
	// repositionView() inside ta.Update never scrolls when a new wrap row
	// appears. After the update, setViewportContent / syncLayout trims it back
	// to the exact row count via taRows().
	if km, ok := msg.(tea.KeyMsg); ok {
		undoDebugLog("updateTA KEY=%q val_before=%q undoStack=%d lastUndoValue=%q", km.String(), m.ta.Value(), len(m.undoStack), m.lastUndoValue)
	}
	m.ta.SetHeight(m.ta.Height() + 1)
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return cmd
}

// --- Input colorizer ---

func (m *model) colorizeInput(view string) string {
	if !isTTY {
		return view
	}

	// The textarea cursor is rendered as \x1b[7m<char>\x1b[m (reverse-video).
	// Extract it before colorizing so ANSI width math stays correct, then
	// re-inject at the same visual column afterward.
	type cursorSave struct {
		lineIdx   int
		visualCol int // 0-based visual column of the cursor character
		escape    string
	}
	var cursor *cursorSave

	cursorRE := "\x1b[7m"
	if idx := strings.Index(view, cursorRE); idx >= 0 {
		// Find the full cursor sequence: \x1b[7m<char>\x1b[m
		end := strings.Index(view[idx:], "\x1b[m")
		var seq string
		if end >= 0 {
			seq = view[idx : idx+end+3] // include trailing \x1b[m
		} else {
			seq = view[idx:]
		}
		// Determine which line and visual column the cursor sits on.
		before := view[:idx]
		lineIdx := strings.Count(before, "\n")
		lastNL := strings.LastIndex(before, "\n")
		linePrefix := before[lastNL+1:]
		visualCol := ansi.StringWidth(linePrefix)
		cursor = &cursorSave{lineIdx: lineIdx, visualCol: visualCol, escape: seq}
		// Remove cursor from view so subsequent ANSI stripping is clean.
		view = view[:idx] + ansi.Strip(seq) + view[idx+len(seq):]
	}

	lines := strings.Split(view, "\n")
	if len(lines) == 0 {
		return view
	}

	// Strip ANSI from line 0 to find the visual position of "❯ ".
	// line0Plain is used for measurement only; line 0 itself is re-written below.
	line0Plain := ansi.Strip(lines[0])
	promptEnd := strings.Index(line0Plain, "❯ ")
	if promptEnd < 0 {
		return view
	}
	// indentVisual is the number of visible columns up to and including "❯ ".
	indentVisual := promptEnd + 2

	// For line 0: find the byte offset in the raw (ANSI-containing) line that
	// corresponds to indentVisual visible columns, then split there.
	// We advance through the raw line, counting visible chars by stripping ANSI.
	line0ByteSplit := func(raw string, visualCols int) int {
		vis := 0
		i := 0
		for i < len(raw) {
			if raw[i] == '\x1b' {
				// skip ANSI escape sequence
				j := i + 1
				for j < len(raw) && raw[j] != 'm' {
					j++
				}
				if j < len(raw) {
					j++ // consume 'm'
				}
				i = j
				continue
			}
			vis++
			if vis == visualCols {
				_, runeSize := utf8.DecodeRuneInString(raw[i:])
				return i + runeSize
			}
			_, runeSize := utf8.DecodeRuneInString(raw[i:])
			i += runeSize
		}
		return len(raw)
	}

	for i, line := range lines {
		// Strip ANSI from this line to work with visible characters only.
		plain := ansi.Strip(line)

		var prefix, inputPart string
		if i == 0 {
			split := line0ByteSplit(line, indentVisual)
			prefix = line[:split]
			// promptEnd is a byte offset in plain; len("❯ ") is the byte width.
			// Using indentVisual here would slice mid-rune for multi-byte prompt chars.
			inputPart = plain[promptEnd+len("❯ "):]
		} else {
			// Continuation lines have indentVisual plain spaces as indent.
			if len(plain) <= indentVisual {
				continue
			}
			prefix = strings.Repeat(" ", indentVisual)
			inputPart = plain[indentVisual:]
		}

		var colored string
		if m.searching && m.searchQuery.Len() > 0 {
			colored = highlightMatch(inputPart, m.searchQuery.String())
		} else {
			colored = colorizeTokens(inputPart)
		}
		lines[i] = prefix + colored
	}

	// Apply keyboard selection highlight (before cursor re-injection so cursor
	// takes visual precedence over the selection background).
	if m.taSelAnchor >= 0 && m.taSelEnd >= 0 && m.taSelAnchor != m.taSelEnd {
		loRune, hiRune := m.taSelAnchor, m.taSelEnd
		if loRune > hiRune {
			loRune, hiRune = hiRune, loRune
		}
		// Walk display lines, tracking which global rune offset each line starts at.
		// Each display line corresponds to exactly the plain chars in inputPart.
		// We reconstruct the mapping by re-splitting the value.
		valueRunes := []rune(m.ta.Value())
		logicalLines := strings.Split(m.ta.Value(), "\n")
		displayLineOffset := 0 // global rune offset at the start of this logical line
		displayLineIdx := 0    // index into lines[]
		for _, logLine := range logicalLines {
			logRunes := []rune(logLine)
			// memoizedWrap-equivalent: split logLine into display rows using taWrapRows logic.
			wrapW := m.ta.Width()
			if wrapW <= 0 {
				wrapW = 1
			}
			displayRows := wrapLineIntoRows(logRunes, wrapW)
			rowOffset := 0 // rune offset within the logical line for this display row
			for _, row := range displayRows {
				rowLen := len(row)
				rowStart := displayLineOffset + rowOffset
				// Clamp sel range to this row.
				selLo := loRune - rowStart
				selHi := hiRune - rowStart
				if selLo < rowLen && selHi >= 0 {
					if selLo < 0 {
						selLo = 0
					}
					if selHi > rowLen {
						selHi = rowLen
					}
					// Apply highlight to lines[displayLineIdx] between selLo and selHi (rune positions in inputPart).
					if displayLineIdx < len(lines) {
						lines[displayLineIdx] = applyInputHighlight(lines[displayLineIdx], indentVisual, selLo, selHi)
					}
				}
				rowOffset += rowLen
				displayLineIdx++
			}
			// +1 for the '\n' separator between logical lines.
			displayLineOffset += len(logRunes) + 1
		}
		_ = valueRunes // used only for length context; actual work done via logicalLines
	}

	// Re-inject the cursor escape at the saved visual column.
	if cursor != nil && cursor.lineIdx < len(lines) {
		line := lines[cursor.lineIdx]
		targetCol := cursor.visualCol
		byteOff := 0
		vis := 0
		for byteOff < len(line) {
			if line[byteOff] == '\x1b' {
				// skip ANSI escape sequence
				j := byteOff + 1
				for j < len(line) && line[j] != 'm' {
					j++
				}
				if j < len(line) {
					j++
				}
				byteOff = j
				continue
			}
			if vis == targetCol {
				break
			}
			// Advance by one full UTF-8 rune.
			_, runeSize := utf8.DecodeRuneInString(line[byteOff:])
			vis++
			byteOff += runeSize
		}
		// Wrap the rune at byteOff with reverse-video on/off.
		// Use \x1b[27m (reverse off) instead of \x1b[m (full reset) so active
		// color codes (e.g. yellow on a slash command) remain in effect after
		// the cursor character.
		if byteOff < len(line) {
			_, runeSize := utf8.DecodeRuneInString(line[byteOff:])
			ch := line[byteOff : byteOff+runeSize]
			lines[cursor.lineIdx] = line[:byteOff] + "\x1b[7m" + ch + "\x1b[27m" + line[byteOff+runeSize:]
		}
	}

	return strings.Join(lines, "\n")
}

// wrapLineIntoRows splits a logical line (as rune slice) into display rows of
// at most `width` display columns, matching the bubbles/textarea wrap() logic.
// Returns each row as a rune slice.
func wrapLineIntoRows(runes []rune, width int) [][]rune {
	var (
		rows   [][]rune
		curRow []rune
		word   []rune
		curW   int
		spaces int
	)
	flush := func() {
		ww := uniseg.StringWidth(string(word))
		sw := spaces
		if curW+ww+sw > width {
			rows = append(rows, curRow)
			curRow = append(append([]rune{}, word...), []rune(strings.Repeat(" ", spaces))...)
			curW = ww + sw
		} else {
			curRow = append(append(curRow, word...), []rune(strings.Repeat(" ", spaces))...)
			curW += ww + sw
		}
		word = nil
		spaces = 0
	}
	for _, r := range runes {
		if r == ' ' || r == '\t' {
			if len(word) > 0 || spaces > 0 {
				spaces++
			}
		} else {
			if spaces > 0 {
				flush()
			}
			word = append(word, r)
			lastW := rw.RuneWidth(word[len(word)-1])
			if uniseg.StringWidth(string(word))+lastW > width {
				if curW > 0 {
					rows = append(rows, curRow)
					curRow = nil
					curW = 0
				}
				curRow = append(curRow, word...)
				curW += uniseg.StringWidth(string(word))
				word = nil
			}
		}
	}
	// flush remaining
	ww := uniseg.StringWidth(string(word))
	sw := spaces
	if curW+ww+sw >= width {
		rows = append(rows, curRow)
		curRow = append(append([]rune{}, word...), []rune(strings.Repeat(" ", spaces))...)
	} else {
		curRow = append(append(curRow, word...), []rune(strings.Repeat(" ", spaces))...)
	}
	rows = append(rows, curRow)
	return rows
}

// applyInputHighlight applies reverse-video highlight to rune positions [selLo, selHi)
// of the inputPart section of a colorized display line (after indentVisual prefix chars).
func applyInputHighlight(line string, indentVisual, selLo, selHi int) string {
	// Re-split at indentVisual to isolate the input part.
	plainLine := ansi.Strip(line)
	if len([]rune(plainLine)) <= indentVisual {
		return line
	}
	// Find the byte split for the indent in the colorized line.
	inputRunes := []rune(plainLine[indentVisual:])
	if selLo >= len(inputRunes) || selHi <= 0 {
		return line
	}
	if selLo < 0 {
		selLo = 0
	}
	if selHi > len(inputRunes) {
		selHi = len(inputRunes)
	}
	// Build the highlighted version of the plain input part.
	// Use a background-color highlight (not \x1b[7m reverse-video) so it doesn't
	// conflict with the cursor which also uses \x1b[7m.
	var sb strings.Builder
	sb.WriteString(string(inputRunes[:selLo]))
	sb.WriteString("\x1b[48;5;240m") // dark-gray bg for selection
	sb.WriteString(string(inputRunes[selLo:selHi]))
	sb.WriteString("\x1b[49m") // reset background only
	sb.WriteString(string(inputRunes[selHi:]))
	// Reconstruct with original prefix.
	prefix := string([]rune(plainLine)[:indentVisual])
	return prefix + sb.String()
}

// highlightMatch bolds and yellows the first occurrence of query inside s.
func highlightMatch(s, query string) string {
	idx := strings.Index(strings.ToLower(s), strings.ToLower(query))
	if idx < 0 {
		return s
	}
	return s[:idx] + colorize(s[idx:idx+len(query)], "\033[1;33m") + s[idx+len(query):]
}

func colorizeTokens(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = colorizeTokenLine(line)
	}
	return strings.Join(lines, "\n")
}

func colorizeTokenLine(s string) string {
	plain := ansi.Strip(s)
	var out strings.Builder
	i := 0
	for i < len(plain) {
		// Emit whitespace runs as-is.
		j := i
		for j < len(plain) && plain[j] == ' ' {
			j++
		}
		if j > i {
			out.WriteString(plain[i:j])
			i = j
			continue
		}
		// Collect non-whitespace token.
		for j < len(plain) && plain[j] != ' ' {
			j++
		}
		w := plain[i:j]
		switch {
		case isSlashCmdToken(w):
			out.WriteString(yellow(w))
		case strings.HasPrefix(w, "@"):
			out.WriteString(dim(w))
		default:
			out.WriteString(w)
		}
		i = j
	}
	return out.String()
}

// --- ANSI strip ---

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\033' {
			inEsc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// --- Agent dispatch ---

// runTurn routes a prompt to the appropriate agent, writing output to out.
func runTurn(ctx context.Context, st *interactiveState, rtr *router.Router, agents dispatchAgents, input string, out io.Writer, ir ...inputReader) error {
	localAgent := agents.local
	claudeAgent := agents.claude
	localAvail := agents.localAvail
	claudeAvail := agents.claudeAvail

	turnCtx, cancel := context.WithTimeoutCause(ctx, agentTimeout, fmt.Errorf("turn timeout"))
	defer cancel()

	forceEscalate := st.forceEscalate || st.stickyEscalate
	forceLocal := st.forceLocal || st.stickyLocal
	decision, routeErr := rtr.Route(turnCtx, st.sess, input, forceEscalate, forceLocal)
	if routeErr != nil {
		return fmt.Errorf("routing: %w", routeErr)
	}
	st.forceEscalate = false
	st.forceLocal = false
	// stickyEscalate/stickyLocal persist until explicitly cleared.

	target := decision.Target
	if target == router.TargetLocal && !localAvail {
		target = router.TargetClaude
	}
	if target == router.TargetClaude && !claudeAvail {
		target = router.TargetLocal
	}

	var inputR inputReader
	if len(ir) > 0 && ir[0] != nil {
		inputR = ir[0]
	} else {
		inputR = newStdinInputReader()
	}

	switch target {
	case router.TargetLocal:
		return runLocal(turnCtx, st.cfg, st.sess, localAgent, st.mem, input, out)
	case router.TargetClaude:
		// Refresh credentials before each turn so expiring tokens are renewed.
		// The credential-process handles its own cache and returns immediately
		// when the token is still fresh, so this is cheap in the common case.
		claudeAgent = applyAWSCreds(st.cfg, claudeAgent)
		return runClaudeWith(turnCtx, st.sess, claudeAgent, input, inputR, permContext{cs: st.cs, toolFutures: st.toolFutures}, st.mem, out)
	}
	return nil
}

// --- Input history persistence ---

const maxPersistedHistory = 500

func globalHistoryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".milk", "input_history"), nil
}

func sessionHistoryPath(sessID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".milk", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, sessID+".history"), nil
}

func readHistoryFile(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20) // 1 MiB — accommodate long multi-line entries
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines = append(lines, strings.ReplaceAll(line, `\n`, "\n"))
		}
	}
	return lines
}

func writeHistoryFile(path string, history []string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	start := 0
	if len(history) > maxPersistedHistory {
		start = len(history) - maxPersistedHistory
	}
	for _, entry := range history[start:] {
		fmt.Fprintln(w, strings.ReplaceAll(entry, "\n", `\n`))
	}
	w.Flush() //nolint:errcheck
}

// --- runREPL entry point ---

func runREPL(cfg config.Config, cwd string, initialFlagNew bool, initialFlagSession string) error {
	sess, err := loadSession(cwd, initialFlagNew, initialFlagSession)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}

	obsShutdown := initObs(cfg)
	defer func() { obsShutdown(context.Background()) }() //nolint:errcheck

	var mem *memory.Store
	if dir, err := memoryDir(); err == nil {
		if m, err := memory.NewStore(dir, sess.ID); err == nil {
			mem = m
		}
	}

	// Build the local agent without blocking on credential refresh. If
	// aws_auth_refresh is enabled, the agent starts with no/stale credentials
	// and a background goroutine refreshes them after the TUI is running.
	baseAC := cfg.ActiveLocalAgent()
	localAgent := local.NewFromConfig(baseAC)
	if od, err := config.OtelDir(); err == nil {
		localAgent.WithOtelDir(od)
	}
	claudeAgent := claude.NewWithOpts(cfg.ClaudeBin, cfg.DangerouslySkipPermissions, cfg.AllowedTools, cfg.AddDirs)
	claudeAgent = applyAWSCreds(cfg, claudeAgent)
	if dbg, err := openClaudeDebugLog(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: cannot open claude debug log: %v\n", milkTag(), err)
	} else if dbg != nil {
		defer dbg.Close()
		claudeAgent = claudeAgent.WithDebugLog(dbg)
	}

	ctx := context.Background()
	// TUI mode continues even when both agents are unavailable so the user can
	// add providers via /provider commands without re-launching.
	localAvail, claudeAvail, _ := checkAgentAvailability(ctx, localAgent, claudeAgent)

	var routeLocalAgent *local.Agent
	if localAvail {
		routeLocalAgent = localAgent
	}
	rtr := router.New(cfg, routeLocalAgent)

	if cfg.DangerouslySkipPermissions {
		fmt.Fprintf(os.Stderr, "%s\n", red("warning: dangerously_skip_permissions is enabled — Claude will auto-approve all tool uses without prompting"))
	}

	var cs *claudesettings.Store
	if store, err := claudesettings.Open(cwd); err == nil {
		cs = store
	}

	st := &interactiveState{sess: sess, cwd: cwd, cfg: cfg, mem: mem, cs: cs, toolFutures: map[string]chan string{}, skipPermissions: cfg.DangerouslySkipPermissions}
	agents := dispatchAgents{localAgent, claudeAgent, localAvail, claudeAvail}

	m := newModel(ctx, st, rtr, agents, mem)
	m.hasLocalAgentConfig = cfg.HasLocalAgentConfig()
	m.colorizeMode = ParseColorizeMode(cfg.Colorization)
	if needsAWSRefresh(cfg) {
		m.credRefreshing = true
		m.credLabel = "AWS"
	} else if needsTokenCmdRefresh(cfg) {
		m.credRefreshing = true
		m.credLabel = "token"
	}
	if gp, err := globalHistoryPath(); err == nil {
		m.globalHistory = readHistoryFile(gp)
	}
	if sp, err := sessionHistoryPath(sess.ID); err == nil {
		m.sessionHistory = readHistoryFile(sp)
	}

	p := tea.NewProgram(m,
		tea.WithAltScreen(),
	)
	st.program = p

	// Refresh credentials in the background so the TUI starts immediately.
	// A 30-second timeout prevents indefinite blocking on network errors.
	if needsAWSRefresh(cfg) {
		go func() {
			cmd := claudesettings.AWSAuthRefreshCommand()
			refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			creds, err := claude.ResolveAWSCredsContext(refreshCtx, cmd)
			p.Send(credRefreshReadyMsg{label: "AWS", creds: creds, err: err})
		}()
	} else if needsTokenCmdRefresh(cfg) {
		go func() {
			err := localAgent.WarmToken()
			p.Send(credRefreshReadyMsg{label: "token", err: err})
		}()
	}

	// Mode 1002+1006: button-motion + SGR extension.
	// Reports drag coordinates while a button is held, enabling live selection
	// highlight updates. Native terminal selection is replaced by app-managed selection.
	os.Stdout.WriteString("\x1b[?1002h\x1b[?1006h") //nolint:errcheck
	finalModel, err := p.Run()
	os.Stdout.WriteString("\x1b[?1006l\x1b[?1002l") //nolint:errcheck

	if fm, ok := finalModel.(model); ok {
		if gp, err := globalHistoryPath(); err == nil {
			writeHistoryFile(gp, fm.globalHistory)
		}
		if sp, err := sessionHistoryPath(sess.ID); err == nil {
			writeHistoryFile(sp, fm.sessionHistory)
		}
	}
	if mem != nil {
		mem.Consolidate() //nolint:errcheck
	}
	return err
}
