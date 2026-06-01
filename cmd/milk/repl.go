package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	"github.com/scoutme/milk/internal/obs"
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
// escalationLocal is non-nil when the escalation target is a second local
// provider (cfg.EscalationAgent names a agents entry); in that case
// cliAgent may be nil. When escalation goes to the Claude CLI, escalationLocal
// is nil and claude is non-nil (the default).
type dispatchAgents struct {
	local           *local.Agent
	cliAgent        *claude.Agent
	escalationLocal *local.Agent // non-nil when escalation target is a local provider
	localAvail      bool
	escalationAvail bool
}

// --- TUI message types ---

// chunkMsg carries a chunk of streamed agent output.
type chunkMsg struct{ text string }

// thinkChunkMsg carries a chunk of streamed thinking/reasoning output, kept
// separate from regular content so it can be shown or hidden independently.
type thinkChunkMsg struct{ text string }

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
// remoteInputMsg carries a prompt injected from the remote oversight interface.
type remoteInputMsg struct{ text string }

type permRequestMsg struct {
	prompt string
	respCh chan string
}

// forgetState holds the pending /forget confirmation dialog.
type forgetState struct {
	candidates []memory.Percept // matched percepts shown to the user
}

// switchAgentState tracks state for the /agent switch wizard.
// Both name and role may be supplied inline; missing ones are prompted.
type switchAgentState struct {
	name string // agent name chosen (may be set from inline args)
	role string // "primary" or "escalation" (may be set from inline args)
	step switchAgentStep
}

type switchAgentStep int

const (
	switchStepName switchAgentStep = iota
	switchStepRole
	switchStepDone
)

// addAgentState tracks state for the multi-step /agent add wizard.
// Fields are filled one at a time when the user doesn't supply them inline.
type addAgentState struct {
	ac   config.AgentConfig
	step addAgentStep
}

type addAgentStep int

const (
	addStepName addAgentStep = iota
	addStepProvider
	addStepURL
	addStepModel
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

// makeLocalPermAsk returns the permAsk callback for the local agent.
// It reuses the existing TUI permRequestMsg flow: the goroutine blocks on a
// channel while the TUI displays a yellow permission prompt to the user.
// Grants are persisted to ps (may be nil). Session-level skipPermissions is
// handled by the caller via WithSkipPermissions before this is ever called.
func makeLocalPermAsk(ir *tuiInputReader, ps *local.PermStore) func(tool, summary string) bool {
	return func(tool, summary string) bool {
		prompt := fmt.Sprintf("\n%s permission request — primary agent tool: %s", milkTag(), bold(tool))
		if summary != "" {
			prompt += fmt.Sprintf("  (%s)", dim(summary))
		}
		prompt += fmt.Sprintf("\n%s Allow? [Y/n] ", milkTag())
		yn, _ := ir.readLine(prompt)
		if yn == "" || strings.EqualFold(yn, "y") {
			return true
		}
		return false
	}
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

	// transcript accumulator (pointer — strings.Builder must not be copied by value).
	// Always contains the full content including thinking (dim-wrapped).
	transcript *strings.Builder
	// transcriptNoThink mirrors transcript but replaces thinking blocks with a
	// "[thinking…]" placeholder. Both are maintained in parallel so toggling is
	// instantaneous — no rebuild required.
	transcriptNoThink *strings.Builder
	// thinkingActiveInTurn is true while thinking tokens are arriving for the
	// current turn. The placeholder is flushed to transcriptNoThink when the
	// first regular content chunk or turn-end arrives.
	thinkingActiveInTurn bool
	// showThinking controls whether thinking content is visible in the viewport.
	showThinking bool
	// currentTurnThinking accumulates thinking text for the current in-progress
	// turn so it can be stored in session.Turn.Thinking when the turn completes.
	currentTurnThinking *strings.Builder

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
	searchQuery   *strings.Builder
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

	// active tool use — non-empty while the escalation agent is executing a tool call
	activeToolUse string

	// memory panel
	panelMemory        bool
	panelOffset        int
	mem                *memory.Store
	lastPanelClickID   string
	lastPanelClickTime time.Time

	// pending /forget confirmation
	pendingForget *forgetState

	// pending /agent add wizard
	pendingAdd *addAgentState

	// pending /agent switch wizard
	pendingSwitch *switchAgentState

	// pending /setup telegram wizard
	pendingTelegramSetup *telegramSetupState

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

	// hasInferenceAgent is true when the user has explicitly configured a
	// local-agent backend. Used to show setup hints on the welcome screen.
	hasInferenceAgent bool

	// Per-agent session token totals; updated at turn end from the in-memory accumulator.
	primaryPrompt     int64
	primaryCompletion int64
	escalationPrompt  int64
	escalationComp    int64

	// Live turn output: chars written during the current turn (proxy for completion tokens).
	// Reset at turn start, frozen at turn end as lastTurnChars until next turn starts.
	currentTurnChars int64
	lastTurnChars    int64
	// lastTurnTokens is the real completion token count from the last completed turn
	// (replaces lastTurnChars when available; 0 means use char count).
	lastTurnTokens int64

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

	// credRefreshInit, if non-nil, is returned by Init() to start background
	// credential refresh only after the bubbletea event loop is running.
	credRefreshInit tea.Cmd

	// injected dependencies
	ctx    context.Context
	st     *interactiveState
	rtr    *router.Router
	agents dispatchAgents
}

func newModel(ctx context.Context, st *interactiveState, rtr *router.Router, agents dispatchAgents, mem *memory.Store) model {
	ta := buildTextarea()
	return model{
		histIdx:             -1,
		ctx:                 ctx,
		st:                  st,
		rtr:                 rtr,
		agents:              agents,
		ta:                  ta,
		transcript:          &strings.Builder{},
		transcriptNoThink:   &strings.Builder{},
		currentTurnThinking: &strings.Builder{},
		searchQuery:         &strings.Builder{},
		showThinking:        st.cfg.ShowReasoningDefault(),
		mem:                 mem,
		panelMemory:         true,
		selAnchorLine:       -1,
		selEndLine:          -1,
		taSelAnchor:         -1,
		taSelEnd:            -1,
		lastUndoValue:       "\x00", // sentinel: never equals real textarea value, so first push always succeeds
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
// Enter is blocked (with a one-time hint); ctrl+c cancels; page-up/down scroll
// the viewport (consistent with normal mode); all other keys are forwarded to
// the textarea so the user can pre-compose the next message.
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
	case "ctrl+t":
		m = m.toggleThinking()
		return m, nil
	case "pgup", "ctrl+u":
		m.vp.HalfPageUp()
		return m, nil
	case "pgdown", "ctrl+f":
		m.vp.HalfPageDown()
		return m, nil
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

	// Attach accumulated thinking to the last assistant turn in the session.
	if thinking := m.currentTurnThinking.String(); thinking != "" {
		hist := m.st.sess.History
		for i := len(hist) - 1; i >= 0; i-- {
			if hist[i].Role == session.RoleAssistant {
				hist[i].Thinking = thinking
				break
			}
		}
		m.currentTurnThinking.Reset()
	}

	if m.interrupted {
		m.interrupted = false
		m.appendTranscript(dim("[interrupted]") + "\n")
	} else if msg.err != nil {
		m.appendTranscript(milkTag() + " error: " + msg.err.Error() + "\n")
	}
	obs.IncrementTurnCount()
	m.primaryPrompt, m.primaryCompletion = obs.SessionTokensByRole("primary")
	m.escalationPrompt, m.escalationComp = obs.SessionTokensByRole("escalation")
	m.lastTurnChars = m.currentTurnChars
	m.currentTurnChars = 0
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
			ids := buildPanelLineIDs(m.mem, sessionBricks{
				currentNeed:      m.st.sess.CurrentNeed,
				lastLocalSummary: m.st.sess.LastLocalSummary,
				escalationBrief:  m.st.sess.EscalationBrief,
			})
			if panelLine >= 0 && panelLine < len(ids) {
				id := ids[panelLine]
				if id != "" {
					now := time.Now()
					if id == m.lastPanelClickID && now.Sub(m.lastPanelClickTime) <= 400*time.Millisecond {
						// Double-click: print brick or percept details to transcript.
						bricks := sessionBricks{
							currentNeed:      m.st.sess.CurrentNeed,
							lastLocalSummary: m.st.sess.LastLocalSummary,
							escalationBrief:  m.st.sess.EscalationBrief,
						}
						var result string
						if content := brickContent(id, bricks); content != "" {
							result = milkTag() + " [" + id + "]\n" + content
						} else {
							result = execMemoryShow("#"+id[:min(6, len(id))], m.st)
						}
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
	escalationAvail := m.agents.escalationAvail

	lines := []string{
		pulseColors[8] + "◈" + ansiReset + " " + "\033[1;38;2;255;208;96mmilk\033[0m",
		dim("switch models, not context."),
		"",
	}

	switch {
	case !m.hasInferenceAgent:
		// No provider configured at all — show setup guidance regardless of Claude.
		lines = append(lines,
			yellow("no local agent configured"),
			"",
			dim("quickstart — add a backend with /agent add:"),
			"",
			dim("llama.cpp · Ollama"),
			"› /agent add url=http://localhost:8080 provider=local model=qwen2.5-coder",
			"",
			dim("AWS Bedrock"),
			"› /agent add url=https://bedrock-runtime.<region>.amazonaws.com provider=bedrock model=<arn>",
			"",
			dim("OpenRouter · Together · Groq"),
			"› /agent add url=https://openrouter.ai/api/v1 provider=bearer api_key=<key> model=<id>",
			"",
			dim("Claude Code CLI"),
			"› /agent add name=claude provider=claude-cli",
			"",
		)
		if !escalationAvail {
			escName := m.st.escalationAgentName()
			lines = append(lines,
				dim(escName+" not available — escalation disabled"),
				"",
			)
		}
		lines = append(lines, dim("/help for all commands"))
	case !localAvail && !escalationAvail:
		lines = append(lines,
			yellow("no agents available"),
			"",
			dim("local agent unreachable — check your provider config with /agent"),
			dim(m.st.escalationAgentName()+" not available — escalation disabled"),
			"",
			dim("/help for available commands"),
		)
	case !localAvail:
		lines = append(lines,
			dim("type a message and press Enter to start"),
			dim("local agent unreachable — use /agent to check or switch backends"),
			dim("/help for available commands"),
		)
	case !escalationAvail:
		lines = append(lines,
			dim("type a message and press Enter to start"),
			dim(m.st.escalationAgentName()+" not available — escalation disabled"),
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

// appendTranscript adds text to both transcript variants and refreshes the viewport.
// Sticky-bottom: only auto-scrolls when already at the bottom.
func (m *model) appendTranscript(text string) {
	// If regular content follows thinking, ensure both variants end with a newline
	// so the final content starts on its own line rather than the last thinking row.
	if m.thinkingActiveInTurn {
		if s := m.transcript.String(); len(s) > 0 && s[len(s)-1] != '\n' {
			m.transcript.WriteByte('\n')
		}
		if s := m.transcriptNoThink.String(); len(s) > 0 && s[len(s)-1] != '\n' {
			m.transcriptNoThink.WriteByte('\n')
		}
	}
	m.transcript.WriteString(text)
	m.transcriptNoThink.WriteString(text)
	// If regular content arrives after thinking, mark that the think block ended
	// (the placeholder was already written when the block started).
	m.thinkingActiveInTurn = false
	if m.ready {
		atBottom := m.vp.AtBottom()
		m.setViewportContent()
		if atBottom {
			m.vp.GotoBottom()
		}
	}
}

// appendThinking adds thinking/reasoning text to the full transcript (dim-styled)
// and a single "[thinking…]" placeholder to transcriptNoThink (only on the first
// chunk of a new thinking block, to avoid repeated placeholders per token).
func (m *model) appendThinking(text string) {
	m.transcript.WriteString(dim(text))
	if !m.thinkingActiveInTurn {
		m.transcriptNoThink.WriteString(dim("[thinking… Ctrl+T to show]"))
		m.thinkingActiveInTurn = true
	}
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

// activeTranscript returns the transcript variant to render based on showThinking.
func (m *model) activeTranscript() *strings.Builder {
	if m.showThinking {
		return m.transcript
	}
	return m.transcriptNoThink
}

// wrappedTranscript returns the transcript (or welcome screen) word-wrapped to
// the viewport content width. When a selection range is active, the selected
// text region is highlighted with an inverted background, respecting column
// boundaries on the first and last lines.
func (m *model) wrappedTranscript() string {
	tx := m.activeTranscript()
	if tx.Len() == 0 {
		return m.applySelectionHighlight(m.welcomeScreen())
	}
	vw := m.vpWidth()
	raw := tx.String()
	if vw <= 0 {
		return raw
	}
	if m.colorizeMode == ColorizeOff {
		return m.applySelectionHighlight(ansi.Wrap(raw, vw, ""))
	}

	txLen := tx.Len()
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
	ac := m.st.cfg.ActiveAgent()
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
	tokenStr := m.statusTokens()
	left := fmt.Sprintf(" %s  %s  %s%s", dim("session:"+sessID), dim("role:")+dim(sessionRole(m.st.sess.State)), dim("agent:")+m.statusAgent(), tokenStr)
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

// statusTokens returns the token counter fragment for the status bar.
// While busy: shows live output char count for current turn (↓ proxy).
// While idle: shows session totals for the current agent role (↑↓).
// After the first turn a "last:" suffix shows the previous turn's output count.
func (m *model) statusTokens() string {
	role := agentRoleForStatus(m.st.sess.State)

	var prompt, completion int64
	switch role {
	case "escalation":
		prompt, completion = m.escalationPrompt, m.escalationComp
	default:
		prompt, completion = m.primaryPrompt, m.primaryCompletion
	}

	if m.busy {
		// Live turn: show running output char count.
		live := dim(fmt.Sprintf("  ↓%s", formatTokenCount(m.currentTurnChars)))
		return live
	}

	if prompt+completion == 0 && m.lastTurnChars == 0 {
		return ""
	}

	var parts []string
	if prompt+completion > 0 {
		parts = append(parts, fmt.Sprintf("%s↑%s↓", formatTokenCount(prompt), formatTokenCount(completion)))
	}
	if m.lastTurnChars > 0 {
		parts = append(parts, fmt.Sprintf("last:↓%s", formatTokenCount(m.lastTurnChars)))
	}
	return "  " + dim(strings.Join(parts, " "))
}

// agentRoleForStatus maps session state to an agent role string for token lookup.
func agentRoleForStatus(s session.State) string {
	if s == session.StateEscalation || s == session.StateEscalationWaiting {
		return "escalation"
	}
	return "primary"
}

// formatTokenCount formats a token count compactly: <1000 → exact, ≥1000 → "1.2k".
func formatTokenCount(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// sessionRole maps session state to the human-readable role shown in the status bar.
func sessionRole(s session.State) string {
	switch s {
	case session.StateLocal:
		return "PRIMARY"
	case session.StateEscalation:
		return "ESCALATION"
	case session.StateEscalationWaiting:
		return "ESCALATION_WAITING"
	default:
		return "ROUTING"
	}
}

func agentLabel(st *interactiveState) string {
	localName := st.cfg.ActiveAgent().Name
	if localName == "" {
		localName = "local"
	}
	escalationName := st.cfg.EscalationAgentConfig().Name
	if escalationName == "" {
		escalationName = "escalation"
	}
	switch {
	case st.stickyEscalate:
		return escalationName + " (pinned)"
	case st.forceEscalate:
		return escalationName + " (forced)"
	case st.stickyPrimary:
		return localName + " (pinned)"
	case st.forcePrimary:
		return localName + " (forced)"
	case st.sess.State == session.StateEscalation || st.sess.State == session.StateEscalationWaiting:
		return escalationName
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
	if m.credRefreshInit != nil {
		cmds = append(cmds, m.credRefreshInit)
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
			return m.handleAddAgentKey(msg)
		}
		if m.pendingSwitch != nil {
			return m.handleSwitchAgentKey(msg)
		}
		if m.pendingTelegramSetup != nil {
			return m.handleTelegramSetupKey(msg)
		}
		if m.inputLocked() {
			return m.handleBusyKey(msg)
		}
		return m.handleKey(msg)

	case remoteInputMsg:
		if !m.busy && msg.text != "" {
			m.appendTranscript(dim("[telegram]") + " " + colorizeTokens(msg.text) + "\n")
			return m.dispatchAgent(msg.text)
		}
		return m, nil

	case telegramGetMeMsg:
		if msg.err != nil {
			m.pendingTelegramSetup = nil
			m.appendTranscript(fmt.Sprintf("%s token validation failed: %v\n", milkTag(), msg.err))
			return m, nil
		}
		if m.pendingTelegramSetup != nil {
			m.pendingTelegramSetup.botName = msg.botName
			m.pendingTelegramSetup.step = telegramStepWaitMsg
			m.appendTranscript(fmt.Sprintf("%s bot validated: @%s\n\n"+
				"Now send any message to @%s on Telegram, then press Enter here.\n\n"+
				milkTag()+" (press Enter when done) ",
				milkTag(), msg.botName, msg.botName))
		}
		return m, nil

	case telegramSetupResolvedMsg:
		m.pendingTelegramSetup = nil
		if msg.err != nil {
			m.appendTranscript(fmt.Sprintf("%s %v\n", milkTag(), msg.err))
			return m, nil
		}
		m = m.commitTelegramSetup(msg.token, msg.chatID)
		return m, nil

	case permRequestMsg:
		return m.handlePermRequest(msg)

	case toolUseMsg:
		m.activeToolUse = msg.name
		return m, nil

	case chunkMsg:
		m.currentTurnChars += int64(len(msg.text))
		m.appendTranscript(msg.text)
		return m, nil

	case thinkChunkMsg:
		m.currentTurnThinking.WriteString(msg.text)
		m.appendThinking(msg.text)
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
				ac := activeLocalAgentConfig(m.st.cfg)
				ac.AWSKeyID = msg.creds.AccessKeyID
				ac.AWSSecret = msg.creds.SecretAccessKey
				ac.AWSToken = msg.creds.SessionToken
				newAgent := local.NewFromConfig(ac)
				if od, err := config.OtelDir(); err == nil {
					newAgent.WithOtelDir(od)
				}
				prog := m.st.program
				newAgent.WithOnSigV4Refresh(func(err error) {
					prog.Send(credRefreshReadyMsg{label: "AWS", err: err})
				})
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
	case "ctrl+t":
		m = m.toggleThinking()
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
	if cmd == cmdAgent {
		return m.handleAgentCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdColorize {
		return m.handleColorizeCmd(strings.TrimSpace(rest)), nil
	}
	if cmd == cmdThink {
		return m.handleThinkCmd(strings.TrimSpace(rest)), nil
	}
	if cmd == cmdSetup {
		return m.handleSetupCmd(strings.TrimSpace(rest))
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

// handleThinkCmd handles `/think [on|off]`.
// With no arg: shows the current reasoning visibility. With on/off: toggles it.
// The toggle is retroactive — switching between transcript variants is instantaneous
// because both are maintained in parallel during streaming.
func (m model) handleThinkCmd(arg string) model {
	switch arg {
	case "on":
		if m.showThinking {
			m.appendTranscript(milkTag() + " reasoning visibility: already on\n")
			return m
		}
		m.showThinking = true
		m.colorizeForce = true // switch transcript variant — invalidate cache
		m.colorizeTransLen = 0
		m.appendTranscript(milkTag() + " reasoning visibility: on\n")
	case "off":
		if !m.showThinking {
			m.appendTranscript(milkTag() + " reasoning visibility: already off\n")
			return m
		}
		m.showThinking = false
		m.colorizeForce = true // switch transcript variant — invalidate cache
		m.colorizeTransLen = 0
		m.appendTranscript(milkTag() + " reasoning visibility: off — thinking blocks hidden ([thinking…])\n")
	default:
		state := "off"
		if m.showThinking {
			state = "on"
		}
		m.appendTranscript(fmt.Sprintf("%s reasoning visibility: %s  (use /think on|off)\n", milkTag(), bold(state)))
	}
	return m
}

// toggleThinking flips reasoning visibility and appends a status line.
// Works at any time including while the agent is responding, since it only
// mutates showThinking and the colorize cache — no input submission needed.
func (m model) toggleThinking() model {
	m.showThinking = !m.showThinking
	m.colorizeForce = true
	m.colorizeTransLen = 0
	if m.showThinking {
		m.appendTranscript(milkTag() + " reasoning visibility: on\n")
	} else {
		m.appendTranscript(milkTag() + " reasoning visibility: off\n")
	}
	return m
}

// handleSetupCmd dispatches /setup <subcommand>.
func (m model) handleSetupCmd(arg string) (tea.Model, tea.Cmd) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "telegram":
		m.appendTranscript(milkTag() + " Telegram setup\n\n" +
			"1. Message @BotFather on Telegram\n" +
			"2. Send /newbot and follow the prompts\n" +
			"3. BotFather gives you a token like 123456:ABC-DEF...\n\n" +
			milkTag() + " Bot token: ")
		m.pendingTelegramSetup = &telegramSetupState{step: telegramStepToken}
		m.ta.Reset()
		return m, nil
	case "telegram on":
		m = m.setTelegramEnabled(true)
		return m, nil
	case "telegram off":
		m = m.setTelegramEnabled(false)
		return m, nil
	default:
		m.appendTranscript(milkTag() + " usage: /setup telegram | /setup telegram on | /setup telegram off\n")
		return m, nil
	}
}

// handleTelegramSetupKey handles keypresses during the /setup telegram wizard.
func (m model) handleTelegramSetupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingTelegramSetup = nil
		m.appendTranscript("\n" + milkTag() + " cancelled\n")
		return m, nil
	case "enter":
		answer := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.syncLayout()

		st := m.pendingTelegramSetup
		switch st.step {
		case telegramStepToken:
			// Mask token display — show only last 6 chars.
			display := answer
			if len(answer) > 6 {
				display = strings.Repeat("*", len(answer)-6) + answer[len(answer)-6:]
			}
			m.appendTranscript(display + "\n")
			if answer == "" {
				m.appendTranscript(milkTag() + " token is required\n" + milkTag() + " Bot token: ")
				return m, nil
			}
			st.token = answer
			m.appendTranscript(milkTag() + " validating token…\n")
			token := answer
			return m, func() tea.Msg {
				botName, err := resolveTelegramBotName(token)
				return telegramGetMeMsg{botName: botName, err: err}
			}

		case telegramStepWaitMsg:
			m.appendTranscript("\n" + milkTag() + " looking for your chat ID…\n")
			token := st.token
			return m, func() tea.Msg {
				chatID, err := resolveTelegramChatID(token)
				return telegramSetupResolvedMsg{token: token, chatID: chatID, err: err}
			}
		}
	}
	var cmd tea.Cmd
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// telegramGetMeMsg carries the result of token validation via getMe.
type telegramGetMeMsg struct {
	botName string
	err     error
}

// telegramSetupResolvedMsg carries the result of a chat ID resolution attempt.
type telegramSetupResolvedMsg struct {
	token  string
	chatID int64
	err    error
}

// resolveTelegramBotName calls getMe to validate the token and return the bot username.
func resolveTelegramBotName(token string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.telegram.org/bot"+token+"/getMe", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("invalid token — check the value from @BotFather")
	}
	return result.Result.Username, nil
}

// resolveTelegramChatID calls getUpdates to find the most recent chat ID that
// messaged the bot. Returns an error when no messages are found.
func resolveTelegramChatID(token string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.telegram.org/bot"+token+"/getUpdates?limit=10", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var result struct {
		OK     bool `json:"ok"`
		Result []struct {
			Message *struct {
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	if !result.OK {
		return 0, fmt.Errorf("telegram API error — check your token")
	}
	for i := len(result.Result) - 1; i >= 0; i-- {
		if m := result.Result[i].Message; m != nil && m.Chat.ID != 0 {
			return m.Chat.ID, nil
		}
	}
	return 0, fmt.Errorf("no messages found — send any message to the bot and press Enter again")
}

// commitTelegramSetup writes the Telegram config and reinitialises the notifier.
// setTelegramEnabled enables or disables Telegram oversight without touching
// the stored credentials. Saves config and reinitialises the notifier.
func (m model) setTelegramEnabled(on bool) model {
	ro := m.st.cfg.RemoteOversight
	if ro == nil {
		ro = &config.RemoteOversightConfig{}
		m.st.cfg.RemoteOversight = ro
	}
	if on {
		if ro.Telegram == nil || ro.Telegram.Token == "" || ro.Telegram.ChatID == 0 {
			m.appendTranscript(milkTag() + " no Telegram credentials configured — run /setup telegram first\n")
			return m
		}
		ro.Backend = "telegram"
	} else {
		ro.Backend = ""
	}
	if err := config.Save(m.st.cfg); err != nil {
		m.appendTranscript(fmt.Sprintf("%s error saving config: %v\n", milkTag(), err))
		return m
	}
	m.st.notifier = newNotifier(m.st.cfg)
	if tn, ok := m.st.notifier.(interface {
		SetOnInput(func(string))
		StartPolling(context.Context)
	}); ok && m.st.program != nil {
		p := m.st.program
		tn.SetOnInput(func(text string) { p.Send(remoteInputMsg{text: text}) })
		tn.StartPolling(m.ctx)
	}
	if on {
		m.appendTranscript(milkTag() + " Telegram oversight enabled\n")
	} else {
		m.appendTranscript(milkTag() + " Telegram oversight disabled\n")
	}
	return m
}

func (m model) commitTelegramSetup(token string, chatID int64) model {
	ro := m.st.cfg.RemoteOversight
	if ro == nil {
		ro = &config.RemoteOversightConfig{}
	}
	ro.Backend = "telegram"
	if ro.Telegram == nil {
		ro.Telegram = &config.TelegramConfig{}
	}
	ro.Telegram.Token = token
	ro.Telegram.ChatID = chatID
	m.st.cfg.RemoteOversight = ro

	if err := config.Save(m.st.cfg); err != nil {
		m.appendTranscript(fmt.Sprintf("%s error saving config: %v\n", milkTag(), err))
		return m
	}

	// Re-init notifier with the new credentials.
	m.st.notifier = newNotifier(m.st.cfg)
	if tn, ok := m.st.notifier.(interface {
		SetOnInput(func(string))
		StartPolling(context.Context)
	}); ok && m.st.program != nil {
		p := m.st.program
		tn.SetOnInput(func(text string) { p.Send(remoteInputMsg{text: text}) })
		tn.StartPolling(m.ctx)
	}

	m.appendTranscript(fmt.Sprintf("%s Telegram configured (chat_id: %d) — sending test message…\n", milkTag(), chatID))
	m.st.notifier.NotifyTurnStart(context.Background(), "milk", "setup", "Telegram oversight configured successfully ✓")
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

	// Strip optional '#' prefix so both "#abc123" and "abc123" work as ID lookups.
	idPat := strings.TrimPrefix(pat, "#")

	var candidates []memory.Percept
	if strings.HasPrefix(pat, "#") {
		candidates = m.mem.FindByIDPrefix(idPat)
	} else {
		// Try ID prefix first (hex-looking input without '#'), fall back to text search.
		if isHexPrefix(idPat) {
			candidates = m.mem.FindByIDPrefix(idPat)
		}
		if len(candidates) == 0 {
			candidates = m.mem.List(memory.ListOpts{Pattern: pat})
		}
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

// handleAgentCmd handles `/agent [list|switch <name>|add [key=val ...]]`.
func (m model) handleAgentCmd(arg string) (model, tea.Cmd) {
	// Re-read config so externally added providers are visible.
	if fresh, err := config.Load(); err == nil {
		// Preserve the in-session active selection if the user hasn't changed it.
		if m.st.cfg.Agent != "" {
			fresh.Agent = m.st.cfg.Agent
		}
		m.st.cfg = fresh
	}

	switch {
	case arg == "" || arg == "status":
		m.appendTranscript(execAgent(m.st) + "\n")

	case arg == "list":
		m.appendTranscript(execAgentList(m.st) + "\n")

	case arg == "switch", strings.HasPrefix(arg, "switch "):
		inline := strings.TrimSpace(strings.TrimPrefix(arg, "switch"))
		return m.startSwitchAgent(inline)

	case strings.HasPrefix(arg, "add"):
		inline := strings.TrimSpace(arg[len("add"):])
		return m.startAddAgent(inline), nil

	default:
		m.appendTranscript(milkTag() + " usage: /agent [list|switch <name>|add [name=... url=... model=... provider=...]]\n")
	}
	return m, nil
}

// execAgentList formats all configured agent backends, marking the active
// primary agent with "P" and the active escalation agent with "E".
func execAgentList(st *interactiveState) string {
	agents := st.cfg.Agents
	if len(agents) == 0 {
		agents = []config.AgentConfig{st.cfg.ActiveAgent()}
	}
	primaryName := strings.ToLower(strings.TrimSpace(st.cfg.ActiveAgent().Name))
	escalationName := strings.ToLower(strings.TrimSpace(st.cfg.EscalationAgentConfig().Name))
	var b strings.Builder
	fmt.Fprintf(&b, "%s agents (%d):\n", milkTag(), len(agents))
	for i, a := range agents {
		nameLower := strings.ToLower(a.Name)
		isPrimary := strings.EqualFold(nameLower, primaryName) && !a.IsCLI()
		isEscalation := strings.EqualFold(nameLower, escalationName)
		var marker string
		switch {
		case isPrimary && isEscalation:
			marker = bold("PE")
		case isPrimary:
			marker = bold("P ")
		case isEscalation:
			marker = bold(" E")
		default:
			marker = "  "
		}
		provider := a.Provider
		if provider == "" {
			provider = "local"
		}
		fmt.Fprintf(&b, "[%s] %s  %s  %s  [%s]", marker, bold(a.Name), dim(a.URL), dim(a.Model), provider)
		if i < len(agents)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// startAddAgent handles `/agent add [key=val ...]`.
// Known keys: name, url, model, provider, api_key, aws_region.
// Missing required fields (name, url, model) are prompted interactively.
func (m model) startAddAgent(inline string) model {
	ac := parseAgentInlineArgs(inline)

	// If all required fields are present, add immediately.
	isCLI := strings.ToLower(ac.Provider) == "claude-cli"
	if ac.Name != "" && (isCLI || (ac.URL != "" && ac.Model != "")) {
		return m.commitAddAgent(ac)
	}

	// Otherwise start the wizard from the first missing required field.
	st := &addAgentState{ac: ac}
	st.step = firstMissingStep(ac)
	m.pendingAdd = st
	m.appendTranscript(addAgentPrompt(st.step) + " ")
	m.ta.Reset()
	return m
}

// parseAgentInlineArgs parses "key=val key2=val2 ..." into a LocalAgentConfig.
func parseAgentInlineArgs(s string) config.AgentConfig {
	var ac config.AgentConfig
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
		case "bin":
			ac.Bin = v
		}
	}
	return ac
}

// firstMissingStep returns the first wizard step that still needs input.
func firstMissingStep(ac config.AgentConfig) addAgentStep {
	if ac.Name == "" {
		return addStepName
	}
	if ac.Provider == "" {
		return addStepProvider
	}
	p := strings.ToLower(ac.Provider)
	isCLI := p == "claude-cli"
	if !isCLI && ac.URL == "" {
		return addStepURL
	}
	if !isCLI && ac.Model == "" {
		return addStepModel
	}
	if !isCLI && p != "local" && p != "bedrock" && ac.APIKey == "" {
		return addStepAPIKey
	}
	if p == "bedrock" && ac.AWSRegion == "" {
		return addStepAWSRegion
	}
	return addStepDone
}

// addAgentPrompt returns the prompt string for a wizard step.
func addAgentPrompt(step addAgentStep) string {
	switch step {
	case addStepName:
		return milkTag() + " name:"
	case addStepURL:
		return milkTag() + " url:"
	case addStepModel:
		return milkTag() + " model:"
	case addStepProvider:
		return milkTag() + " provider [local/bedrock/claude-cli/<bearer-name>, enter to skip]:"
	case addStepAPIKey:
		return milkTag() + " api_key:"
	case addStepAWSRegion:
		return milkTag() + " aws_region:"
	default:
		return ""
	}
}

// handleAddAgentKey handles keypresses during the /agent add wizard.
func (m model) handleAddAgentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
				m.appendTranscript(milkTag() + " name is required\n" + addAgentPrompt(addStepName) + " ")
				return m, nil
			}
			st.ac.Name = answer
		case addStepURL:
			if answer == "" {
				m.appendTranscript(milkTag() + " url is required\n" + addAgentPrompt(addStepURL) + " ")
				return m, nil
			}
			st.ac.URL = answer
		case addStepModel:
			if answer == "" {
				m.appendTranscript(milkTag() + " model is required\n" + addAgentPrompt(addStepModel) + " ")
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
			m = m.commitAddAgent(st.ac)
		} else {
			m.appendTranscript(addAgentPrompt(st.step) + " ")
		}
		return m, nil
	}
	var cmd tea.Cmd
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// commitAddAgent appends the new agent to config, saves, and confirms.
func (m model) commitAddAgent(ac config.AgentConfig) model {
	// Check for name collision.
	for _, existing := range m.st.cfg.Agents {
		if strings.EqualFold(existing.Name, ac.Name) {
			m.appendTranscript(fmt.Sprintf("%s agent %q already exists — use /agent switch %s to activate it\n",
				milkTag(), ac.Name, ac.Name))
			return m
		}
	}
	isFirst := len(m.st.cfg.Agents) == 0
	m.st.cfg.Agents = append(m.st.cfg.Agents, ac)
	if isFirst {
		m.st.cfg.Agent = ac.Name
	}
	if err := config.Save(m.st.cfg); err != nil {
		m.appendTranscript(fmt.Sprintf("%s error saving config: %v\n", milkTag(), err))
		return m
	}
	m.hasInferenceAgent = true
	if isFirst {
		freshAC := applyFreshAWSCreds(m.st.cfg, activeLocalAgentConfig(m.st.cfg))
		newAgent := local.NewFromConfig(freshAC)
		if od, err := config.OtelDir(); err == nil {
			newAgent.WithOtelDir(od)
		}
		prog := m.st.program
		newAgent.WithOnSigV4Refresh(func(err error) {
			prog.Send(credRefreshReadyMsg{label: "AWS", err: err})
		})
		m.agents.local = newAgent
		m.agents.localAvail = newAgent.Ping(m.ctx) == nil
		m.rtr = router.New(m.st.cfg, newAgent)
	}
	provider := ac.Provider
	if provider == "" {
		provider = "local"
	}
	var agentDetail string
	if strings.ToLower(provider) == "claude-cli" {
		bin := ac.Bin
		if bin == "" {
			bin = "claude"
		}
		agentDetail = fmt.Sprintf("%s added agent %s  (%s | bin=%s)\n",
			milkTag(), bold(ac.Name), provider, bin)
	} else {
		agentDetail = fmt.Sprintf("%s added agent %s  (%s | %s | %s)\n",
			milkTag(), bold(ac.Name), ac.URL, ac.Model, provider)
	}
	m.appendTranscript(agentDetail)
	if isFirst {
		m.appendTranscript(milkTag() + " activated as the default provider\n")
	} else {
		m.appendTranscript(milkTag() + " use /agent switch " + ac.Name + " to activate it\n")
	}
	return m
}

// parseSwitchInlineArgs parses "name [as role]" or "name role" inline args.
// Returns name and role (either may be empty).
func parseSwitchInlineArgs(s string) (name, role string) {
	// Accept "name as role" or just "name" or "name role"
	fields := strings.Fields(s)
	switch len(fields) {
	case 0:
	case 1:
		name = fields[0]
	case 2:
		name = fields[0]
		role = strings.ToLower(fields[1])
	case 3:
		name = fields[0]
		// "name as role"
		if strings.ToLower(fields[1]) == "as" {
			role = strings.ToLower(fields[2])
		}
	}
	return
}

// switchAgentPrompt returns the prompt string for the given switch wizard step.
func switchAgentPrompt(step switchAgentStep, names []string) string {
	switch step {
	case switchStepName:
		return milkTag() + " agent name [" + strings.Join(names, ", ") + "]:"
	case switchStepRole:
		return milkTag() + " role [primary/escalation]:"
	}
	return ""
}

// startSwitchAgent starts or immediately executes /agent switch.
// inline is everything after "switch" — may contain name and/or "as role".
func (m model) startSwitchAgent(inline string) (model, tea.Cmd) {
	name, role := parseSwitchInlineArgs(inline)

	// Validate name if provided.
	if name != "" {
		found := false
		for _, a := range m.st.cfg.Agents {
			if strings.EqualFold(a.Name, name) {
				found = true
				break
			}
		}
		if !found {
			var names []string
			for _, a := range m.st.cfg.Agents {
				names = append(names, a.Name)
			}
			m.appendTranscript(fmt.Sprintf("%s unknown agent %q — available: %s\n",
				milkTag(), name, strings.Join(names, ", ")))
			return m, nil
		}
	}
	// Validate role if provided.
	if role != "" && role != "primary" && role != "escalation" {
		m.appendTranscript(fmt.Sprintf("%s unknown role %q — use primary or escalation\n", milkTag(), role))
		return m, nil
	}

	st := &switchAgentState{name: name, role: role}
	// Determine which step to start from.
	if name == "" {
		st.step = switchStepName
	} else if role == "" {
		st.step = switchStepRole
	} else {
		st.step = switchStepDone
		return m.commitSwitchAgent(st)
	}

	var names []string
	for _, a := range m.st.cfg.Agents {
		names = append(names, a.Name)
	}
	m.pendingSwitch = st
	m.appendTranscript(switchAgentPrompt(st.step, names) + " ")
	m.ta.Reset()
	m.syncLayout()
	return m, nil
}

// handleSwitchAgentKey handles keypresses during the /agent switch wizard.
func (m model) handleSwitchAgentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingSwitch = nil
		m.appendTranscript("\n" + milkTag() + " cancelled\n")
		return m, nil
	case "enter":
		answer := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript(answer + "\n")

		st := m.pendingSwitch
		switch st.step {
		case switchStepName:
			if answer == "" {
				var names []string
				for _, a := range m.st.cfg.Agents {
					names = append(names, a.Name)
				}
				m.appendTranscript(switchAgentPrompt(switchStepName, names) + " ")
				return m, nil
			}
			found := false
			for _, a := range m.st.cfg.Agents {
				if strings.EqualFold(a.Name, answer) {
					found = true
					break
				}
			}
			if !found {
				var names []string
				for _, a := range m.st.cfg.Agents {
					names = append(names, a.Name)
				}
				m.appendTranscript(fmt.Sprintf("%s unknown agent %q — available: %s\n", milkTag(), answer, strings.Join(names, ", ")))
				m.appendTranscript(switchAgentPrompt(switchStepName, names) + " ")
				return m, nil
			}
			st.name = answer
			st.step = switchStepRole
			m.appendTranscript(switchAgentPrompt(switchStepRole, nil) + " ")
			return m, nil

		case switchStepRole:
			role := strings.ToLower(answer)
			if role != "primary" && role != "escalation" {
				m.appendTranscript(fmt.Sprintf("%s unknown role %q — use primary or escalation\n", milkTag(), answer))
				m.appendTranscript(switchAgentPrompt(switchStepRole, nil) + " ")
				return m, nil
			}
			st.role = role
			st.step = switchStepDone
			m.pendingSwitch = nil
			return m.commitSwitchAgent(st)
		}
		return m, nil
	}
	var cmd tea.Cmd
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// commitSwitchAgent applies the name+role switch: updates config and rebuilds the live agent.
func (m model) commitSwitchAgent(st *switchAgentState) (model, tea.Cmd) {
	m.pendingSwitch = nil
	name := st.name
	role := st.role

	switch role {
	case "primary":
		m.st.cfg.Agent = name
		if err := config.Save(m.st.cfg); err != nil {
			m.appendTranscript(fmt.Sprintf("%s warning: could not persist switch: %v\n", milkTag(), err))
		}
		newAgent := local.NewFromConfig(activeLocalAgentConfig(m.st.cfg))
		if od, err := config.OtelDir(); err == nil {
			newAgent.WithOtelDir(od)
		}
		prog := m.st.program
		newAgent.WithOnSigV4Refresh(func(err error) {
			prog.Send(credRefreshReadyMsg{label: "AWS", err: err})
		})
		m.agents.local = newAgent
		m.agents.localAvail = newAgent.Ping(m.ctx) == nil
		m.rtr = router.New(m.st.cfg, newAgent)
		m.credStatus = ""
		m.credLabel = ""
		m.credOK = false
		m.appendTranscript(fmt.Sprintf("%s primary agent → %s\n", milkTag(), bold(name)))
		m.appendTranscript(execAgent(m.st) + "\n")
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

	case "escalation":
		m.st.cfg.EscalationAgent = name
		if err := config.Save(m.st.cfg); err != nil {
			m.appendTranscript(fmt.Sprintf("%s warning: could not persist switch: %v\n", milkTag(), err))
		}
		escAC := applyFreshAWSCreds(m.st.cfg, m.st.cfg.EscalationAgentConfig())
		if escAC.IsCLI() {
			// claude-cli: no local.Agent needed, just update config.
			m.agents.escalationLocal = nil
			m.agents.escalationAvail = true
		} else if escAC.URL != "" {
			newEsc := local.NewFromConfig(escAC).AsEscalationTarget(escAC.Name)
			if od, err := config.OtelDir(); err == nil {
				newEsc.WithOtelDir(od)
			}
			m.agents.escalationLocal = newEsc
			m.agents.escalationAvail = newEsc.Ping(m.ctx) == nil
		}
		m.appendTranscript(fmt.Sprintf("%s escalation agent → %s\n", milkTag(), bold(name)))
		m.appendTranscript(execAgent(m.st) + "\n")
	}

	return m, nil
}

// isHexPrefix returns true when s looks like a percept ID prefix (4-64 hex chars).
func isHexPrefix(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
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
	case strings.HasPrefix(answer, "#") || isHexPrefix(answer):
		prefix := strings.TrimPrefix(answer, "#")
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
	m.currentTurnChars = 0
	m.currentTurnThinking.Reset()
	m.thinkingActiveInTurn = false

	turnCtx, cancel := context.WithCancel(m.ctx)
	m.cancelTurn = cancel

	st := m.st
	rtr := m.rtr
	agents := m.agents

	send := func(msg tea.Msg) { st.program.Send(msg) }
	st.toolFutures = map[string]chan string{}

	tuiAgents := agents
	ir0 := &tuiInputReader{send: send}
	tuiAgents.cliAgent = agents.cliAgent.
		WithSkipPermissions(st.skipPermissions).
		WithOnToolUse(func(name string) {
			send(toolUseMsg{name: name})
		}).
		WithOnToolUseReady(func(name string, input map[string]any) {
			var hint string
			if name == "AskUserQuestion" {
				if rendered := formatAskUserQuestion(input); rendered != "" {
					hint = fmt.Sprintf("\n\033[2m⚙ %s\033[0m\n%s\n", name, rendered)
				} else {
					hint = fmt.Sprintf("\n\033[2m⚙ %s\033[0m\n", name)
				}
			} else {
				summary := cliToolArgSummary(input)
				if summary != "" {
					hint = fmt.Sprintf("\n\033[2m⚙ %s: %s\033[0m\n", name, summary)
				} else {
					hint = fmt.Sprintf("\n\033[2m⚙ %s\033[0m\n", name)
				}
				if st.cfg.RemoteOversight.NotifyToolsEnabled() {
					st.notifier.NotifyToolUse(context.Background(), name, cliToolArgSummary(input))
				}
			}
			send(chunkMsg{text: hint})
		}).
		WithOnThinking(func(text string) { send(thinkChunkMsg{text: text}) }).
		WithPermissionHandler(makeTUIPermissionHandler(ir0, st.cs, st.notifier))

	// Wire local-agent permissions: persistent store + TUI ask callback.
	// Both the primary and escalation-local agents share the same store and ask
	// callback — they operate in the same cwd and grants should be shared.
	localPermStore := st.localPerms
	localPermAsk := makeLocalPermAsk(ir0, localPermStore)
	tuiAgents.local = agents.local.
		WithSkipPermissions(st.skipPermissions).
		WithPermissions(localPermStore, localPermAsk)
	if agents.escalationLocal != nil {
		tuiAgents.escalationLocal = agents.escalationLocal.
			WithSkipPermissions(st.skipPermissions).
			WithPermissions(localPermStore, localPermAsk)
	}
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
	if !filepath.IsAbs(base) {
		base = filepath.Join(cwd, base)
	}
	dir := filepath.Dir(base)
	namePrefix := filepath.Base(base)
	if strings.HasSuffix(prefix, "/") || strings.HasSuffix(prefix, string(filepath.Separator)) {
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
	// Work exclusively in runes: indentVisual is a visual column count, not a
	// byte offset. Using plainLine[indentVisual:] would slice mid-rune for
	// multi-byte prompt characters (e.g. ❯ is 3 bytes but 1 column/rune).
	plainRunes := []rune(ansi.Strip(line))
	if len(plainRunes) <= indentVisual {
		return line
	}
	inputRunes := plainRunes[indentVisual:]
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
	// Reconstruct with original prefix (rune-safe).
	return string(plainRunes[:indentVisual]) + sb.String()
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
	cliAgent := agents.cliAgent
	escalationLocalAgent := agents.escalationLocal
	localAvail := agents.localAvail
	escalationAvail := agents.escalationAvail

	turnCtx, cancel := context.WithTimeoutCause(ctx, agentTimeout, fmt.Errorf("turn timeout"))
	defer cancel()

	forceEscalate := st.forceEscalate || st.stickyEscalate
	forcePrimary := st.forcePrimary || st.stickyPrimary
	decision, routeErr := rtr.Route(turnCtx, st.sess, input, forceEscalate, forcePrimary)
	if routeErr != nil {
		return fmt.Errorf("routing: %w", routeErr)
	}
	st.forceEscalate = false
	st.forcePrimary = false
	// stickyEscalate/stickyPrimary persist until explicitly cleared.

	target := decision.Target
	if target == router.TargetLocal && !localAvail {
		target = router.TargetEscalation
	}
	if target == router.TargetEscalation && !escalationAvail {
		target = router.TargetLocal
	}

	var inputR inputReader
	if len(ir) > 0 && ir[0] != nil {
		inputR = ir[0]
	} else {
		inputR = newStdinInputReader()
	}

	targetName := "local"
	agentName := st.cfg.ActiveAgent().Name
	if target == router.TargetEscalation {
		targetName = "escalation"
		agentName = st.cfg.EscalationAgentConfig().Name
	}
	st.notifier.NotifyTurnStart(turnCtx, agentName, targetName, input)

	var turnErr error
	switch target {
	case router.TargetLocal:
		turnErr = runLocal(turnCtx, st.cfg, st.sess, localAgent, st.mem, input, out)
	case router.TargetEscalation:
		if escalationLocalAgent != nil {
			// Escalation is routed to a second local provider, not the Claude CLI.
			turnErr = runEscalationLocal(turnCtx, st.cfg, st.sess, escalationLocalAgent, st.mem, input, out)
		} else {
			// Refresh credentials before each turn so expiring tokens are renewed.
			// The credential-process handles its own cache and returns immediately
			// when the token is still fresh, so this is cheap in the common case.
			cliAgent = applyAWSCreds(st.cfg, cliAgent)
			turnErr = runCLIEscalationWith(turnCtx, st.cfg, st.sess, cliAgent, input, inputR, permContext{cs: st.cs, toolFutures: st.toolFutures}, st.mem, "", out)
		}
	}
	st.notifier.NotifyTurnDone(turnCtx, agentName, turnErr)
	if turnErr == nil {
		// Find the last assistant turn added by this agent and forward its text.
		hist := st.sess.History
		for i := len(hist) - 1; i >= 0; i-- {
			t := hist[i]
			if t.Role == session.RoleAssistant && t.Content != "" {
				st.notifier.NotifyResponse(turnCtx, agentName, t.Content)
				break
			}
		}
	}
	return turnErr
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
	baseAC := activeLocalAgentConfig(cfg)
	localAgent := local.NewFromConfig(baseAC)
	if od, err := config.OtelDir(); err == nil {
		localAgent.WithOtelDir(od)
	}
	localAgent.WithLogContext(cfg.Otel.LogContext)

	// Build the escalation-local agent when the escalation target is a second
	// local provider rather than the Claude CLI.
	var escalationLocalAgent *local.Agent
	if !cfg.EscalationAgentConfig().IsCLI() {
		escAC := applyFreshAWSCreds(cfg, cfg.EscalationAgentConfig())
		if escAC.URL != "" {
			escalationLocalAgent = local.NewFromConfig(escAC).AsEscalationTarget(escAC.Name)
			if od, err := config.OtelDir(); err == nil {
				escalationLocalAgent.WithOtelDir(od)
			}
			escalationLocalAgent.WithLogContext(cfg.Otel.LogContext)
			escalationLocalAgent.WithSkipPermissions(cliAgentConfig(cfg).DangerouslySkipPermissions)
			if lp, err := local.OpenPermStore(cwd); err == nil {
				escalationLocalAgent.WithPermissions(lp, nil)
			}
		} else {
			fmt.Fprintf(os.Stderr, "%s warning: escalation_agent %q not found in agents — falling back to claude-cli\n", milkTag(), cfg.EscalationAgent)
		}
	}

	cliAgent := newCLIAgent(cliAgentConfig(cfg))
	cliAgent = applyAWSCreds(cfg, cliAgent)
	if dbg, err := openCLIDebugLog(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: cannot open claude debug log: %v\n", milkTag(), err)
	} else if dbg != nil {
		defer dbg.Close()
		cliAgent = cliAgent.WithDebugLog(dbg)
	}

	ctx := context.Background()
	// TUI mode continues even when both agents are unavailable so the user can
	// add providers via /agent commands without re-launching.
	//
	// When escalation is routed to a second local provider, "escalationAvail" really
	// means "escalation agent available" — ping that instead of the Claude CLI.
	var localAvail, escalationAvail bool
	if escalationLocalAgent != nil {
		localAvail = localAgent.Ping(ctx) == nil
		escalationAvail = escalationLocalAgent.Ping(ctx) == nil
		if !localAvail {
			fmt.Fprintln(os.Stderr, milkTag()+" warning: primary agent unreachable — routing all to escalation agent")
		}
		if !escalationAvail {
			fmt.Fprintln(os.Stderr, milkTag()+" warning: escalation agent unreachable — primary only")
		}
	} else {
		localAvail, escalationAvail, _ = checkAgentAvailability(ctx, localAgent, cliAgent)
	}

	var routeLocalAgent *local.Agent
	if localAvail {
		routeLocalAgent = localAgent
	}
	rtr := router.New(cfg, routeLocalAgent)

	if cliAgentConfig(cfg).DangerouslySkipPermissions {
		fmt.Fprintf(os.Stderr, "%s\n", red("warning: dangerously_skip_permissions is enabled — all agents will auto-approve tool uses without prompting"))
	}

	var cs *claudesettings.Store
	if store, err := claudesettings.Open(cwd); err == nil {
		cs = store
	}

	var localPerms *local.PermStore
	if lp, err := local.OpenPermStore(cwd); err == nil {
		localPerms = lp
	}

	st := &interactiveState{sess: sess, cwd: cwd, cfg: cfg, mem: mem, cs: cs, localPerms: localPerms, toolFutures: map[string]chan string{}, skipPermissions: cliAgentConfig(cfg).DangerouslySkipPermissions, notifier: newNotifier(cfg)}
	agents := dispatchAgents{
		local:           localAgent,
		cliAgent:        cliAgent,
		escalationLocal: escalationLocalAgent,
		localAvail:      localAvail,
		escalationAvail: escalationAvail,
	}

	m := newModel(ctx, st, rtr, agents, mem)
	m.hasInferenceAgent = cfg.HasInferenceAgent()
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

	// Credential refresh is deferred to Init() via credRefreshInit so that
	// the tea.Cmd runs only after the bubbletea event loop is fully started.
	// Goroutines that call p.Send() before p.Run() race with TUI init and
	// can corrupt the layout (duplicate prompts / status bars).
	if needsAWSRefresh(cfg) {
		awsCmd := claudesettings.AWSAuthRefreshCommand()
		m.credRefreshInit = func() tea.Msg {
			refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			creds, err := claude.ResolveAWSCredsContext(refreshCtx, awsCmd)
			return credRefreshReadyMsg{label: "AWS", creds: creds, err: err}
		}
	} else if needsTokenCmdRefresh(cfg) {
		m.credRefreshInit = func() tea.Msg {
			err := localAgent.WarmToken()
			return credRefreshReadyMsg{label: "token", err: err}
		}
	}

	p := tea.NewProgram(m,
		tea.WithAltScreen(),
	)
	st.program = p
	localAgent.WithOnSigV4Refresh(func(err error) {
		p.Send(credRefreshReadyMsg{label: "AWS", err: err})
	})

	// Wire remote input: messages from the oversight backend are injected as turns.
	if tn, ok := st.notifier.(interface {
		SetOnInput(func(string))
		StartPolling(context.Context)
	}); ok {
		tn.SetOnInput(func(text string) {
			p.Send(remoteInputMsg{text: text})
		})
		tn.StartPolling(ctx)
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
		_ = mem.Consolidate()
		_ = mem.PruneGlobal(cfg.PerceptStoreSizeLimit())
	}
	return err
}
