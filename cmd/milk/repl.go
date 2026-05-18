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
const memoryPanelWidth = 34 // chars for the memory panel (includes border)
const memoryPanelInner = 32 // usable chars inside the panel
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
	styleStatusBar = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#555", Dark: "#888"}).
			Background(lipgloss.AdaptiveColor{Light: "#E5E5E5", Dark: "#2B2B2B"})
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
	panelMemory bool
	mem         *memory.Store

	// pending /forget confirmation
	pendingForget *forgetState

	// injected dependencies
	ctx    context.Context
	st     *interactiveState
	rtr    *router.Router
	agents dispatchAgents
}

func newModel(ctx context.Context, st *interactiveState, rtr *router.Router, agents dispatchAgents, mem *memory.Store) model {
	ta := buildTextarea()
	return model{
		histIdx:     -1,
		ctx:         ctx,
		st:          st,
		rtr:         rtr,
		agents:      agents,
		ta:          ta,
		transcript:  &strings.Builder{},
		mem:         mem,
		panelMemory: true,
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

	indent := strings.Repeat(" ", promptWidth)
	m.ta.SetPromptFunc(promptWidth, func(lineIdx int) string {
		if lineIdx == 0 {
			return label
		}
		return indent
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
		m.appendTranscript(dim("[milk] agent is responding — press Ctrl+C to interrupt\n"))
		return m, nil
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	syncHeight(&m.ta)
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
	m.ta, cmd = m.ta.Update(msg)
	syncHeight(&m.ta)
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

func (m model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch tea.MouseEvent(msg).Button {
	case tea.MouseButtonWheelUp:
		m.vp.LineUp(3)
	case tea.MouseButtonWheelDown:
		m.vp.LineDown(3)
	}
	return m, nil
}

// setViewportContent rebuilds the full viewport content:
// transcript + separator + input area. The input area scrolls with the transcript.
func (m *model) setViewportContent() {
	mw := m.mainWidth()
	sep := styleBorder.Width(mw).Render("")
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

// wrappedTranscript returns the transcript word-wrapped to the main area width.
func (m *model) wrappedTranscript() string {
	mw := m.mainWidth()
	if mw <= 0 {
		return m.transcript.String()
	}
	return ansi.Wrap(m.transcript.String(), mw, "")
}

// viewportHeight is the full terminal height minus the status bar line.
func (m *model) viewportHeight() int {
	h := m.height - 1
	if h < 3 {
		h = 3
	}
	return h
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

// syncLayout rebuilds viewport content after textarea size changes.
// Sticky-bottom: scrolls to bottom only when already there.
func (m *model) syncLayout() {
	if !m.ready {
		return
	}
	mw := m.mainWidth()
	vpH := m.viewportHeight()
	atBottom := m.vp.AtBottom()
	if m.vp.Width != mw {
		m.vp.Width = mw
	}
	if m.vp.Height != vpH {
		m.vp.Height = vpH
	}
	m.setViewportContent()
	if atBottom {
		m.vp.GotoBottom()
	}
}

// statusBar renders the one-line status bar.
func (m *model) statusBar() string {
	sessID := m.st.sess.ID
	if len(sessID) > 8 {
		sessID = sessID[:8]
	}
	left := fmt.Sprintf(" session:%s  agent:%s", sessID, m.statusAgent())
	right := m.statusCwd() + " "
	gap := m.width - len(stripANSI(left)) - len(right)
	if gap < 1 {
		gap = 1
	}
	bar := left + strings.Repeat(" ", gap) + right
	if isTTY {
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
	agent := agentLabel(m.st)
	if m.pendingPerm != nil {
		return yellow("?") + " " + agent + yellow(" [allow?]")
	}
	if m.busy {
		frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
		if m.activeToolUse != "" {
			return dim(frame) + " " + agent + dim(" ["+m.activeToolUse+"]")
		}
		return dim(frame) + " " + agent
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
	m.ta, cmd = m.ta.Update(msg)
	syncHeight(&m.ta)
	if _, isMouseMsg := msg.(tea.MouseMsg); !isMouseMsg {
		m.syncLayout()
	}
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m model) handleResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height

	mw := m.mainWidth()
	vpH := m.viewportHeight()
	if !m.ready {
		m.vp = viewport.New(mw, vpH)
		m.ready = true
		m.refreshPrompt()
		m.setViewportContent()
		m.vp.GotoBottom()
	} else {
		atBottom := m.vp.AtBottom()
		m.vp.Width = mw
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
		m.ta, cmd = m.ta.Update(msg)
		syncHeight(&m.ta)
		m.syncLayout()
		return m, cmd
	}

	// ctrl+r search mode: intercept most keys.
	if m.searching {
		return m.handleSearchKey(msg)
	}

	switch msg.String() {
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
		m.vp.HalfViewUp()
		return m, nil
	case "pgdown", "ctrl+f":
		m.vp.HalfViewDown()
		return m, nil
	}

	// Non-Tab key resets tab cycling
	m.tabMatches = nil
	m.tabIdx = -1

	switch msg.String() {
	case "shift+enter", "alt+enter", "ctrl+n":
		m.ta.SetHeight(m.ta.LineCount() + 1)
		m.syncLayout()
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	syncHeight(&m.ta)
	m.syncLayout()
	return m, cmd
}

func (m model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.ta.Value() != "" {
		m.ta.Reset()
		m.ta.SetHeight(1)
		m.tabMatches = nil
		m.tabIdx = -1
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
		return m.handleSlashInput(cmd, rest, input)
	}

	return m.dispatchAgent(input)
}

func (m model) handleSlashInput(cmd, rest, rawInput string) (tea.Model, tea.Cmd) {
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
	m.ta, cmd = m.ta.Update(msg)
	syncHeight(&m.ta)
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
			// Skip tools already allowed in settings.
			if st.cs != nil {
				if ok, _ := st.cs.IsToolAllowed(name); ok {
					return
				}
			}
			// Create a buffered future and ask immediately — non-blocking.
			ch := make(chan string, 1)
			st.toolFutures[name] = ch
			send(permRequestMsg{
				prompt: fmt.Sprintf("%s allow tool %s? [Y/n] ", milkTag(), bold(name)),
				respCh: ch,
			})
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

// --- View ---

func (m model) View() string {
	if !m.ready {
		return ""
	}
	vpH := m.viewportHeight()
	mainArea := m.vp.View()
	if m.panelMemory {
		panel := m.renderMemoryPanel(vpH)
		mainArea = lipgloss.JoinHorizontal(lipgloss.Top, mainArea, panel)
	}
	return mainArea + "\n" + m.statusBar()
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

func (m model) handleTab() model {
	input := m.ta.Value()
	if len(m.tabMatches) == 0 {
		m.tabMatches, m.tabIdx = buildTabMatches(input, m.st.cwd)
		if len(m.tabMatches) == 0 {
			return m
		}
	} else {
		m.tabIdx = (m.tabIdx + 1) % len(m.tabMatches)
	}
	completed := m.tabMatches[m.tabIdx]
	m.ta.SetValue(applyTabCompletion(input, completed))
	m.ta.CursorEnd()
	return m
}

// applyTabCompletion replaces the relevant token in input with completed.
func applyTabCompletion(input, completed string) string {
	if !strings.HasPrefix(completed, "@") {
		return completed
	}
	words := strings.Fields(input)
	for i := len(words) - 1; i >= 0; i-- {
		if strings.HasPrefix(words[i], "@") {
			words[i] = completed
			return strings.Join(words, " ")
		}
	}
	return input
}

func buildTabMatches(input, cwd string) ([]string, int) {
	words := strings.Fields(input)
	for i := len(words) - 1; i >= 0; i-- {
		if strings.HasPrefix(words[i], "@") {
			pathPrefix := words[i][1:]
			matches := expandPath(pathPrefix, cwd)
			atMatches := make([]string, len(matches))
			for j, m := range matches {
				atMatches[j] = "@" + m
			}
			return atMatches, 0
		}
	}
	prefix := ""
	for _, w := range words {
		if strings.HasPrefix(w, "/") {
			prefix = w
			break
		}
	}
	if prefix == "" {
		stripped := strings.TrimLeft(input, " ")
		if strings.HasPrefix(stripped, "/") {
			prefix = stripped
		}
	}
	if prefix == "" {
		return nil, 0
	}
	var matches []string
	for _, cmd := range slashCommands {
		if strings.HasPrefix(cmd, prefix) {
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

// visualRows returns the number of visual rows the textarea content occupies,
// accounting for soft-wrapping of long logical lines. Sizes the textarea so it
// always shows all content without internal scrolling.
func visualRows(ta *textarea.Model) int {
	w := ta.Width()
	if w <= 0 {
		w = 80
	}
	total := 0
	for _, line := range strings.Split(ta.Value(), "\n") {
		cols := len([]rune(line))
		total += cols/w + 1
	}
	if total < 1 {
		total = 1
	}
	return total
}

func syncHeight(ta *textarea.Model) {
	h := visualRows(ta)
	ta.SetHeight(h)
}

// --- Input colorizer ---

func (m *model) colorizeInput(view string) string {
	if !isTTY {
		return view
	}
	lastNL := strings.LastIndex(view, "\n")
	var prefix, line string
	if lastNL >= 0 {
		prefix = view[:lastNL+1]
		line = view[lastNL+1:]
	} else {
		line = view
	}
	promptEnd := strings.LastIndex(line, "> ")
	if promptEnd < 0 {
		return view
	}
	promptPart := line[:promptEnd+2]
	inputPart := line[promptEnd+2:]
	if m.searching && m.searchQuery.Len() > 0 {
		return prefix + promptPart + highlightMatch(inputPart, m.searchQuery.String())
	}
	return prefix + promptPart + colorizeTokens(inputPart)
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
	words := strings.Fields(s)
	if len(words) == 0 {
		return s
	}
	var out strings.Builder
	for i, w := range words {
		if i > 0 {
			out.WriteByte(' ')
		}
		switch {
		case strings.HasPrefix(w, "/"):
			out.WriteString(yellow(w))
		case strings.HasPrefix(w, "@"):
			out.WriteString(dim(w))
		default:
			out.WriteString(w)
		}
	}
	if len(s) > 0 && s[len(s)-1] == ' ' {
		out.WriteByte(' ')
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
		return runClaudeWith(turnCtx, st.sess, claudeAgent, input, inputR, permContext{cs: st.cs, toolFutures: st.toolFutures}, out)
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
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines = append(lines, line)
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
	for _, line := range history[start:] {
		fmt.Fprintln(w, line)
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

	// Prime transcript with welcome line
	welcome := fmt.Sprintf("%s interactive mode — session %s  (type /help for commands)\n",
		milkTag(), sess.ID[:8])
	m.transcript.WriteString(welcome)

	p := tea.NewProgram(m,
		tea.WithAltScreen(),
	)
	st.program = p

	// Mode 1000+1006: X10 basic mouse + SGR extension.
	// Reports scroll wheel and clicks as tea.MouseMsg without capturing drag,
	// so native terminal selection and middle-click paste work without Shift.
	os.Stdout.WriteString("\x1b[?1000h\x1b[?1006h") //nolint:errcheck
	finalModel, err := p.Run()
	os.Stdout.WriteString("\x1b[?1006l\x1b[?1000l") //nolint:errcheck

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
