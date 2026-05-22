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

// memoryRefreshMsg fires on a periodic tick to redraw the memory panel.
type memoryRefreshMsg struct{}

// toolUseMsg carries the name of a tool Claude just started calling.
type toolUseMsg struct{ name string }

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

	// click-to-select state (content-space coordinates; -1 = none)
	selAnchorLine int
	selAnchorCol  int
	selEndLine    int
	selEndCol     int
	selText       string // plain text of the selected range (populated after release)
	copyFeedback  string // transient "[copied N chars]" shown in status bar
	busyHint      string // transient "agent is responding" shown in status bar

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

	km := textarea.DefaultKeyMap
	km.InsertNewline.SetKeys("shift+enter", "alt+enter", "ctrl+n")
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
		label = yellow("("+dir+"-search)") + " > "
	} else {
		label = promptLabel(m.st)
	}
	plain := stripANSI(label)
	promptWidth := len(plain)

	m.ta.SetPromptFunc(promptWidth, func(lineIdx int) string {
		if lineIdx == 0 {
			return label
		}
		return ""
	})
	if m.width > 0 {
		m.ta.SetWidth(m.mainWidth() - promptWidth)
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
		m.ta.SetHeight(1)
		m.syncLayout()
		m.appendTranscript(answer + "\n")
		m.pendingPerm.respCh <- answer
		m.pendingPerm = nil
		m.dequeueNextPerm()
		return m, nil
	}
	var cmd tea.Cmd
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
	m.ta.SetHeight(1)
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
	m.ta.SetHeight(1)
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
			m.selText = ""
			m.setViewportContent()
		case tea.MouseActionMotion:
			if m.selAnchorLine >= 0 {
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
			if m.selText != "" {
				copyToClipboard(m.selText)
				m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(m.selText)))
				m.clearSelection()
				m.setViewportContent()
				return m, copyFeedbackClearCmd()
			}
			// No selection: paste clipboard content into the textarea.
			text, err := clipboard.ReadAll()
			if err == nil && text != "" {
				m.ta.InsertString(text)
			}
		}
	}
	return m, nil
}

// selectionText extracts the plain text between the selection anchor and end,
// respecting column boundaries on the first and last lines.
func (m *model) selectionText() string {
	lines := strings.Split(m.wrappedTranscript(), "\n")
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

// clearSelection resets selection state.
func (m *model) clearSelection() {
	m.selAnchorLine = -1
	m.selAnchorCol = 0
	m.selEndLine = -1
	m.selEndCol = 0
	m.selText = ""
}

// setViewportContent rebuilds the full viewport content:
// transcript + separator + input area. The input area scrolls with the transcript.
func (m *model) setViewportContent() {
	vw := m.vpWidth()
	sep := styleBorder.Width(vw).Render("")
	content := m.wrappedTranscript() + "\n" + sep + "\n" + m.colorizeInput(m.ta.View())
	m.vp.SetContent(content)
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

// wrappedTranscript returns the transcript word-wrapped to the viewport content width.
// When a selection range is active, the selected text region is highlighted with an
// inverted background, respecting column boundaries on the first and last lines.
func (m *model) wrappedTranscript() string {
	vw := m.vpWidth()
	raw := m.transcript.String()
	if vw <= 0 {
		return raw
	}
	wrapped := ansi.Wrap(raw, vw, "")
	if m.selAnchorLine < 0 || m.selEndLine < 0 {
		return wrapped
	}
	loLine, loCol := m.selAnchorLine, m.selAnchorCol
	hiLine, hiCol := m.selEndLine, m.selEndCol
	if hiLine < loLine || (hiLine == loLine && hiCol < loCol) {
		loLine, loCol, hiLine, hiCol = hiLine, hiCol, loLine, loCol
	}
	lines := strings.Split(wrapped, "\n")
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

// viewportHeight is the full terminal height minus the header bar (content + border), status bar, and hint lines.
func (m *model) viewportHeight() int {
	h := m.height - 3 - len(m.tabHints)
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
	tagline := dim("local-first agentic orchestrator")
	taglinePlain := "local-first agentic orchestrator"

	sessID := m.st.sess.ID
	if len(sessID) > 8 {
		sessID = sessID[:8]
	}
	model := m.st.cfg.LlamaModel
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
	left := fmt.Sprintf(" %s  %s", dim("session:"+sessID), dim("agent:")+m.statusAgent())
	right := dim(m.statusCwd() + " ")
	if m.busyHint != "" {
		left += yellow(" [" + m.busyHint + "]")
	} else if m.copyFeedback != "" {
		left += green(" [" + m.copyFeedback + "]")
	} else if m.selAnchorLine >= 0 {
		var selStatus string
		if m.selText != "" {
			selStatus = yellow(fmt.Sprintf(" [%d chars — right-click to copy]", len([]rune(m.selText))))
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
	switch {
	case st.stickyEscalate:
		return "claude (pinned)"
	case st.forceEscalate:
		return "claude (forced)"
	case st.stickyLocal:
		return "local (pinned)"
	case st.forceLocal:
		return "local (forced)"
	case st.sess.State == session.StateClaude || st.sess.State == session.StateClaudeWaiting:
		return "claude"
	default:
		return "local"
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
	if msg.Paste {
		var cmd tea.Cmd
		cmd = m.updateTA(msg)
		m.syncLayout()
		return m, cmd
	}

	// ctrl+r search mode: intercept most keys.
	if m.searching {
		return m.handleSearchKey(msg)
	}

	switch msg.String() {
	case "esc":
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
		if m.ta.LineCount() == 1 {
			m = m.historyBack()
			m.syncLayout()
			return m, nil
		}
	case "down":
		if m.ta.LineCount() == 1 {
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

	switch msg.String() {
	case "shift+enter", "alt+enter", "ctrl+n":
		m.ta.SetHeight(m.ta.LineCount() + 1)
		m.syncLayout()
	}
	var cmd tea.Cmd
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

func (m model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.ta.Value() != "" {
		m.ta.Reset()
		m.ta.SetHeight(1)
		m.tabMatches = nil
		m.tabIdx = -1
		m.tabPrefix = ""
		m.tabHints = nil
		m.syncLayout()
		return m, nil
	}
	if m.st.forceEscalate || m.st.forceLocal || m.st.stickyEscalate || m.st.stickyLocal {
		m.st.forceEscalate = false
		m.st.forceLocal = false
		m.st.stickyEscalate = false
		m.st.stickyLocal = false
		m.refreshPrompt()
		m.appendTranscript(dim("[milk]") + " mode cleared\n")
		return m, nil
	}
	return m, tea.Quit
}

func (m model) handleEnter() (tea.Model, tea.Cmd) {
	input := strings.TrimSpace(m.ta.Value())
	m.ta.Reset()
	m.ta.SetHeight(1)
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
	exit, dispatch, output := handleSlashCommand(cmd, rest, m.st)
	m.refreshPrompt()
	if exit {
		return m, tea.Quit
	}
	if output != "" {
		m.appendTranscript(output + "\n")
	}
	if dispatch != "" {
		return m.dispatchAgent(dispatch)
	}
	return m, nil
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
		m.ta.SetHeight(1)
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
		m.ta.SetHeight(m.ta.LineCount())
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
	sw0 := &sendWriter{send: send}
	ir0 := &tuiInputReader{send: send}
	tuiAgents.claude = agents.claude.
		WithOnToolUse(func(name string) {
			send(toolUseMsg{name: name})
		}).
		WithOnToolUseReady(func(name string, input map[string]any) {
			// Skip tools already allowed in settings.
			if st.cs != nil {
				if ok, _ := st.cs.IsToolAllowed(name); ok {
					return
				}
			}
			// Create a buffered future and ask — non-blocking from this goroutine.
			ch := make(chan string, 1)
			st.toolFutures[name] = ch
			detail := formatToolInput(input)
			var prompt string
			if detail != "" {
				prompt = fmt.Sprintf("%s allow tool %s %s? [Y/n] ", milkTag(), bold(name), dim(detail))
			} else {
				prompt = fmt.Sprintf("%s allow tool %s? [Y/n] ", milkTag(), bold(name))
			}
			send(permRequestMsg{prompt: prompt, respCh: ch})
		}).
		WithOnThinking(func(text string) { send(chunkMsg{text: dim(text)}) }).
		WithPermissionHandler(makeTUIPermissionHandler(sw0, st.cs))
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
	m.ta.SetHeight(m.ta.LineCount())
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
	m.ta.SetHeight(m.ta.LineCount())
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
		m.ta.SetHeight(m.ta.LineCount())
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
		m.ta.SetHeight(m.ta.LineCount())
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

// --- Textarea helpers ---

// syncHeight sets the textarea height to exactly the number of display rows
// the textarea itself renders. We derive this from ta.View() so the count
// matches the textarea's own word-wrap logic (uniseg grapheme widths, prompt
// width, etc.) rather than our own approximation which drifts for long lines.
func syncHeight(ta *textarea.Model) {
	view := ta.View()
	// ta.View() includes a trailing \n on each row (including the last one
	// added by the "always show m.Height lines" padding loop). Count newlines
	// and that gives us the display row count the textarea wants.
	rows := strings.Count(view, "\n")
	if rows < 1 {
		rows = 1
	}
	ta.SetHeight(rows)
}

// updateTA pre-grows the textarea by one row before passing the message to
// ta.Update. Without this, when text first wraps to a second line the textarea
// scrolls its internal viewport (offset +1) to keep the cursor visible, hiding
// line 1. The extra row gives headroom so no internal scroll happens; syncHeight
// then resets the height to the exact visual row count.
func (m *model) updateTA(msg tea.Msg) tea.Cmd {
	m.ta.SetHeight(m.ta.Height() + 1)
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	syncHeight(&m.ta)
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

	// Strip ANSI from line 0 to find the visual position of "> ".
	// line0Plain is used for measurement only; line 0 itself is re-written below.
	line0Plain := ansi.Strip(lines[0])
	promptEnd := strings.Index(line0Plain, "> ")
	if promptEnd < 0 {
		return view
	}
	// indentVisual is the number of visible columns up to and including "> ".
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
			inputPart = plain[indentVisual:]
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

	localAgent := local.New(cfg.LlamaURL, cfg.LlamaModel)
	if od, err := config.OtelDir(); err == nil {
		localAgent.WithOtelDir(od)
	}
	claudeAgent := claude.NewWithOpts(cfg.ClaudeBin, cfg.DangerouslySkipPermissions, cfg.AllowedTools, cfg.AddDirs, cfg.EffectivePermissionPhrases(), cfg.EffectiveDirRestrictionPhrases())
	claudeAgent = applyAWSCreds(cfg, claudeAgent)
	if dbg, err := openClaudeDebugLog(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: cannot open claude debug log: %v\n", milkTag(), err)
	} else if dbg != nil {
		defer dbg.Close()
		claudeAgent = claudeAgent.WithDebugLog(dbg)
	}

	ctx := context.Background()
	localAvail, claudeAvail, err := checkAgentAvailability(ctx, localAgent, claudeAgent)
	if err != nil {
		return err
	}

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

	st := &interactiveState{sess: sess, cwd: cwd, cfg: cfg, mem: mem, cs: cs, toolFutures: map[string]chan string{}}
	agents := dispatchAgents{localAgent, claudeAgent, localAvail, claudeAvail}

	m := newModel(ctx, st, rtr, agents, mem)
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
