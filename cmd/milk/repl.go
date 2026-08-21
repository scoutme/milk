package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	rw "github.com/mattn/go-runewidth"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"go.opentelemetry.io/otel/attribute"

	"github.com/scoutme/milk/internal/agent/aider"
	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/agent/smolagent"
	"github.com/scoutme/milk/internal/agent/subprocess"
	"github.com/scoutme/milk/internal/claudesettings"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/loop"
	"github.com/scoutme/milk/internal/mcp"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/shelldetect"
	"github.com/scoutme/milk/internal/tasks"
	"github.com/scoutme/milk/internal/updater"
	"github.com/scoutme/milk/internal/workflow"
	wfdev "github.com/scoutme/milk/internal/workflow/dev"
)

const agentTimeout = 10 * time.Minute

const memoryPanelWidth = 33 // chars for the memory panel (32 inner + 1 right scrollbar)
const memoryPanelInner = 32 // usable inner chars; scrollbar is a separate column in View()
const memoryPollInterval = 5 * time.Second

// dispatchAgents holds the agents and their availability for a turn.
// primary and escalation are the TurnRunner instances used for dispatch.
// local is kept for router classification and in-place credential refresh.
// cliAgent, escalationLocal, subprocessAgent, subprocessPrimary are kept for
// TUI callback wiring in dispatchAgent and live-rebuild in commitSwitchAgent.
type dispatchAgents struct {
	// TurnRunner dispatch targets (set from runREPL / commitSwitchAgent)
	primary    TurnRunner
	escalation TurnRunner
	// Underlying typed agents (needed for TUI callback wiring and live-rebuild)
	local             *local.Agent
	cliAgent          *claude.Agent
	escalationLocal   *local.Agent      // non-nil when escalation target is a local provider
	subprocessAgent   *subprocess.Agent // non-nil when escalation target is a subprocess provider
	subprocessPrimary *subprocess.Agent // non-nil when primary is a subprocess provider
	localAvail        bool
	escalationAvail   bool
	toolRunners       map[string]TurnRunner // lazily built tool-agent runners, keyed by agent name
	// mcpToolSets holds the live MCP ToolSet for each agent that has MCP servers
	// configured, keyed by agent name. Used by /mcp list and /mcp reconnect.
	mcpToolSets map[string]*mcp.ToolSet
}

// --- TUI message types ---

// chunkMsg carries a chunk of streamed agent output.
type chunkMsg struct{ text string }

// prefixChunkMsg carries the agent-name prefix printed before streaming begins.
// It is appended to the transcript but excluded from the live token estimate.
type prefixChunkMsg struct{ text string }

// thinkChunkMsg carries a chunk of streamed thinking/reasoning output, kept
// separate from regular content so it can be shown or hidden independently.
type thinkChunkMsg struct{ text string }

// agentDoneMsg signals the agent goroutine finished.
type agentDoneMsg struct{ err error }

// directBashDoneMsg is sent when a direct-bash command exits (PTY or ExecProcess path).
type directBashDoneMsg struct {
	err     error
	logPath string // path to script(1) typescript; empty when using PTY pane
}

// ptyOutputMsg is sent by the PTY read goroutine whenever new bytes have been
// written to the VT emulator. Triggers a View() re-render of the PTY pane.
type ptyOutputMsg struct{}

// turnTimeoutWarningMsg is sent when a turn exceeds its configured timeout but
// the turn is still running. The turn is not cancelled — this is a soft warning.
type turnTimeoutWarningMsg struct{ agentName string }

type spinnerTickMsg struct{}

// workflowIdleCheckMsg is sent periodically while a workflow is running. It
// checks whether the current role's turn has exceeded that agent's configured
// turn timeout since the last streamed activity, and warns once per idle
// stretch — the workflow equivalent of turnTimeoutWarningMsg, needed because a
// workflow's overall run spans many turns rather than the single turn a
// one-shot timer could cover.
type workflowIdleCheckMsg struct{}

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

// configReloadMsg is sent when the config file changes on disk (via Watcher)
// or when the user runs /reload. cfg is the freshly parsed config; err is
// non-nil when the file could not be parsed (cfg is then the zero value and
// the existing in-memory config should be kept).
type configReloadMsg struct {
	cfg config.Config
	err error
}
type errMsg struct{ err error }

// updateAvailableMsg is sent when the background update check finds a newer release.
type updateAvailableMsg struct{ release *updater.Release }

// workflowResumeCheckMsg is sent at startup when a saved workflow state file
// was found for the current session. The TUI prints a one-line resume offer.
type workflowResumeCheckMsg struct{ state *workflow.State }

// updateProgressMsg carries download progress during /update install.
type updateProgressMsg struct{ done, total int64 }

// updateDoneMsg signals a completed self-update attempt.
type updateDoneMsg struct{ err error }

type serverStartDoneMsg struct {
	agentName string
	url       string
	pid       int
	err       error
}

type serverStopDoneMsg struct {
	agentName string
	stopped   bool
	err       error
}

// openFileMsg is sent by the agent goroutine (or /open command) to request that
// the TUI open a file in the editor. The goroutine blocks on respCh until the
// editor exits. path is the resolved file path to open.
type openFileMsg struct {
	path   string
	respCh chan error // nil when sent from /open (no goroutine waiting)
}

type permRequestMsg struct {
	prompt string
	label  string // status-bar label; defaults to "[allow?]" when empty
	respCh chan string
}

// oauthRequiredMsg is sent by the claude agent when stderr indicates an MCP
// server requires OAuth authorization. serverName may be empty when not
// detectable; authURL may be empty when no URL appeared in the error.
type oauthRequiredMsg struct {
	serverName string
	authURL    string
}

// mcpAuthDoneMsg is sent after a /mcp auth interactive subprocess exits
// successfully. serverName is the MCP server that was authorized.
type mcpAuthDoneMsg struct{ serverName string }

// forgetState holds the pending /forget confirmation dialog.
type forgetState struct {
	candidates []memory.Percept // matched percepts shown to the user
}

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

// prefixWriter is an io.Writer that forwards writes as prefixChunkMsg,
// excluded from the live output-token estimate.
type prefixWriter struct {
	send func(msg tea.Msg)
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.send(prefixChunkMsg{text: string(p)})
	}
	return len(p), nil
}

// tuiInputReader implements inputReader for the TUI: sends a permRequestMsg
// and blocks until the user responds via the TUI input area.
type tuiInputReader struct {
	send func(msg tea.Msg)
}

func (r *tuiInputReader) readLine(prompt string) (string, error) {
	return r.readLineLabeled(prompt, "")
}

func (r *tuiInputReader) readLineLabeled(prompt, label string) (string, error) {
	respCh := make(chan string, 1)
	r.send(permRequestMsg{prompt: prompt, label: label, respCh: respCh})
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
	tabMatches      []string // flat list of matching commands / @-paths
	tabIdx          int      // index into tabMatches (for @-path and non-slash completions)
	tabCmdIdx       int      // index of current command within tabMatches (slash completions)
	tabVarIdx       int      // index of current variant within the current command's variants
	tabLine         int      // line index the current tabMatches were built for
	tabPrefix       string   // what the user had typed when Tab was first pressed
	tabBeforeCursor string   // beforeCursor snapshot at session start; used for clean cycling
	tabAfterCursor  string   // afterCursor snapshot at session start
	tabSubcmdMode   bool     // true when tabMatches holds full sigs (subcommand/trailing-space mode)
	tabHints        []string // hint lines shown below viewport (may have one entry highlighted)
	tabHintsBase    []string // same lines without any highlight; source of truth for highlightHint
	hintIdx         int      // selected inline hint (-1 = none)

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

	// tasks panel
	panelTasks  bool
	tasksOffset int
	taskStore   *tasks.Store

	// pending /forget confirmation
	pendingForget *forgetState

	// pendingAttachments holds files staged for the next agent turn via /attach
	// or path-in-paste detection. Cleared after each successful submission.
	pendingAttachments []PendingAttachment

	// pendingPathPaste is set when the user pastes what looks like a file path.
	// The TUI asks "Attach as file? [y/N]" and this holds the path until answered.
	pendingPathPaste string

	// pending /agent add wizard
	pendingAdd *addAgentState

	// pending /mcp add wizard
	pendingMCPAdd *addMCPState

	// pending /agent switch wizard
	pendingSwitch *switchAgentState

	// pending /setup telegram wizard
	pendingTelegramSetup *telegramSetupState

	// pending /init wizard
	pendingInit *initWizardState

	// prompt width (visual columns) set by the most recent refreshPrompt call;
	// used by taRows() to compute the exact content wrap width.
	promptWidth int

	// click-to-select state (content-space coordinates; -1 = none)
	selAnchorLine int
	selAnchorCol  int
	selEndLine    int
	selEndCol     int
	selDragging   bool   // true once the mouse has moved after the initial press
	selText       string // plain text of the selected range (populated after release)

	// click-to-select state for the memory/workflow side panels (panel-local
	// coordinates; -1 = none). Kept separate from the transcript selection above
	// so a click in one region never bleeds into the other's highlight.
	panelSelRegion     panelRegion
	panelSelAnchorLine int
	panelSelAnchorCol  int
	panelSelEndLine    int
	panelSelEndCol     int
	panelSelDragging   bool
	panelSelText       string

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

	// pendingForceFresh is set to true in dispatchAgent when the turn begins with
	// ForceFreshEscalation so that handleAgentDone can zero lastEscalationContextHash
	// after the turn completes, preventing a stale hash from suppressing the fresh
	// session's first context block.
	pendingForceFresh bool

	// pendingDirectBash is non-nil while waiting for y/N confirmation to run a
	// shell command directly. The string holds the command to run on approval.
	pendingDirectBash *string

	// ptyPane is non-nil while a shell command is running inside an embedded PTY.
	ptyPane *ptyPaneState

	// hasInferenceAgent is true when the user has explicitly configured a
	// local-agent backend. Used to show setup hints on the welcome screen.
	hasInferenceAgent bool

	// startupWarnings holds config validation warnings to print once the TUI is ready.
	startupWarnings []string

	// Per-agent session token totals; updated at turn end from the in-memory accumulator.
	primaryPrompt     int64
	primaryCompletion int64
	escalationPrompt  int64
	escalationComp    int64
	// Cumulative cache tokens for the escalation role (Claude CLI only).
	escalationCacheRead     int64
	escalationCacheCreation int64

	// Live turn output: chars written during the current turn, used as a streaming proxy.
	// Reset at turn start.
	currentTurnChars int64
	// currentTurnInputChars holds the input prompt char count for the current turn,
	// used as a streaming proxy for estimated input tokens.
	currentTurnInputChars int64
	// lastTurnPrompt/Completion are per-role deltas from the last completed turn
	// for each agent, captured at agentDoneMsg.
	lastTurnPrompt     map[string]int64
	lastTurnCompletion map[string]int64
	// lastTokenRole tracks which role's counters were last displayed; used to detect
	// role changes and clear stale last-turn counters between turns.
	lastTokenRole string

	colorizeMode ColorizeMode

	// colorize cache: avoid re-running chroma/glamour on every streamed token.
	// The cache is invalidated when the transcript grows by ≥ colorizeLineThresh
	// new lines, or when the viewport width changes, or when the caller
	// explicitly sets colorizeForce = true (e.g. after agentDoneMsg, resize).
	colorizeCached    string // last colorized output
	colorizeTransLen  int    // transcript byte length when cache was built
	colorizeVPWidth   int    // vpWidth when cache was built
	colorizeForce     bool   // if true, bypass cache on next render
	colorizeLinesSeen int    // new lines since last full re-colorize

	// hintDebounceGen is incremented on every keystroke that triggers a hint
	// rebuild. hintDebounceMsg carries the gen value at dispatch time; any
	// message whose gen no longer matches is a stale firing and is dropped.
	hintDebounceGen int

	// credRefreshInit, if non-nil, is returned by Init() to start background
	// credential refresh only after the bubbletea event loop is running.
	credRefreshInit tea.Cmd

	// workflowResumeInit, if non-nil, is returned by Init() to check for a
	// saved workflow state file and emit workflowResumeCheckMsg when found.
	workflowResumeInit tea.Cmd

	// pendingUpdate is non-nil when an update is available but not yet installed.
	pendingUpdate *updater.Release
	// updateInstalling is true while /update install is downloading+applying.
	updateInstalling bool
	// updateProgress tracks download bytes for the progress indicator.
	updateProgress int64
	updateTotal    int64

	// workflow panel
	workflowPanelOpen     bool
	workflowPanelOffset   int
	workflowState         *workflow.State
	pendingWorkflowWizard *workflowWizardState
	pendingWorkflowExtend *workflowExtendState

	// designer disambiguation
	pendingWorkflowQuestions string        // raw questions from designer (non-nil while collecting answers)
	workflowAnswersCh        chan<- string // channel to send user answers back to workflow goroutine

	// workflow idle watchdog: mirrors the turnTimeoutWarningMsg mechanism used by
	// normal turns, but reset on every workflow role transition/streamed chunk
	// rather than a single one-shot timer, since a workflow runs many turns.
	lastWorkflowActivity  time.Time
	workflowTimeoutWarned bool

	// loop detection
	loopDetector  *loop.Detector
	loopInterrupt bool   // true when a high-confidence loop signal fired
	loopWarning   string // non-empty when a medium-confidence signal fired

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
		hintIdx:             -1,
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
		panelSelRegion:      regionNone,
		panelSelAnchorLine:  -1,
		panelSelEndLine:     -1,
		taSelAnchor:         -1,
		taSelEnd:            -1,
		lastUndoValue:       "\x00", // sentinel: never equals real textarea value, so first push always succeeds
		lastTurnPrompt:      map[string]int64{"primary": 0, "escalation": 0},
		lastTurnCompletion:  map[string]int64{"primary": 0, "escalation": 0},
		loopDetector:        loop.New(st.cfg.LoopDetectionCfg()),
	}
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
		// Match syncLayout's width: the textarea renders as content inside the
		// transcript viewport, whose width is vpWidth() (mainWidth() minus the
		// scrollbar column), not mainWidth() itself.
		m.ta.SetWidth(m.vpWidth())
	}
}

// inputLocked returns true when agent is running.
func (m *model) inputLocked() bool { return m.busy }

// handleBusyKey handles key events while an agent turn is running.
// It intercepts the three busy-specific cases, then delegates to handleKey
// for all navigation, editing, history, undo/redo, and viewport scroll.
// Safe slash commands (display/read-only) are executed immediately even during a turn.
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
		input := strings.TrimSpace(stripCompletionPlaceholders(m.ta.Value()))
		if cmd, rest, found := extractSlashCommand(input); found {
			if busySafeCommands[cmd] {
				// Safe command: execute immediately without clearing busy state.
				m.ta.Reset()
				m.tabMatches = nil
				m.tabIdx = -1
				m.tabHints = nil
				m.tabHintsBase = nil
				m.syncLayout()
				label := promptLabel(m.st)
				m.appendTranscript(label + colorizeTokens(input) + "\n")
				return m.handleSlashInput(cmd, rest)
			}
			m.busyHint = cmd + " unavailable while agent is responding"
			return m, busyHintClearCmd()
		}
		m.busyHint = "agent is responding — Ctrl+C to interrupt"
		return m, busyHintClearCmd()
	case "tab":
		// Tab completion not available while busy — ignore silently.
		return m, nil
	}
	return m.handleKey(msg)
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

	if m.pendingForceFresh {
		m.st.lastEscalationContextHash = ""
		m.pendingForceFresh = false
	}

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
		errText := msg.err.Error()
		switch {
		case isContextCanceled(msg.err):
			// Turn was cancelled by the user — already handled by m.interrupted;
			// this branch catches any late-arriving cancellation that slipped through.
			m.appendTranscript(dim("[interrupted]") + "\n")
		case isContextDeadlineExceeded(msg.err):
			m.appendTranscript(milkTag() + " turn ended: context deadline exceeded\n")
		default:
			m.appendTranscript(milkTag() + " error: " + errText + "\n")
		}
	} else if m.currentTurnChars == 0 {
		// No text or thinking was streamed and no error was reported — the agent
		// produced no visible output. Show a placeholder so the turn is not silent.
		m.appendTranscript(dim("[no response]") + "\n")
	}
	obs.IncrementTurnCount()
	newPrimaryPrompt, newPrimaryCompletion := obs.SessionTokensByRole("primary")
	newEscPrompt, newEscComp := obs.SessionTokensByRolePrefix("escalation")
	newEscCacheRead, newEscCacheCreation := obs.SessionCacheByRolePrefix("escalation")
	// Compute per-role per-turn deltas from the accumulators.
	m.lastTurnPrompt["escalation"] = newEscPrompt - m.escalationPrompt
	m.lastTurnCompletion["escalation"] = newEscComp - m.escalationComp
	m.lastTurnPrompt["primary"] = newPrimaryPrompt - m.primaryPrompt
	m.lastTurnCompletion["primary"] = newPrimaryCompletion - m.primaryCompletion
	m.primaryPrompt, m.primaryCompletion = newPrimaryPrompt, newPrimaryCompletion
	m.escalationPrompt, m.escalationComp = newEscPrompt, newEscComp
	m.escalationCacheRead, m.escalationCacheCreation = newEscCacheRead, newEscCacheCreation
	m.lastTokenRole = m.activeTokenRole()
	m.currentTurnChars = 0
	m.currentTurnInputChars = 0

	// Loop detection: feed turn summary and check for signals.
	if m.loopDetector != nil {
		turnText := ""
		if hist := m.st.sess.History; len(hist) > 0 {
			turnText = hist[len(hist)-1].Content
		}
		turnDelta := m.lastTurnPrompt[m.activeTokenRole()] + m.lastTurnCompletion[m.activeTokenRole()]
		verdicts := m.loopDetector.Feed(loop.TurnSummary{
			Text:         turnText,
			InputTokens:  m.lastTurnPrompt[m.activeTokenRole()],
			OutputTokens: m.lastTurnCompletion[m.activeTokenRole()],
			Timestamp:    time.Now(),
			IsUserTurn:   false,
		})
		for _, v := range verdicts {
			if v.Confidence >= 0.8 {
				m.appendTranscript(yellow(fmt.Sprintf("[⚠ loop detected: %s (confidence %.0f%%)]\n", v.Message, v.Confidence*100)))
				if m.loopDetector != nil && v.ShouldInterrupt {
					m.loopInterrupt = true
				}
			} else if v.Confidence >= 0.5 && m.loopWarning == "" {
				m.loopWarning = fmt.Sprintf("⚠ %s", v.Message)
			}
		}
		_ = turnDelta // used for future velocity display
	}

	m.appendTranscript("\n")
	m.colorizeForce = true // turn finished — force a clean full re-colorize
	m.refreshPrompt()
	m.syncLayout()
	return m, nil
}

func isContextCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

func isContextDeadlineExceeded(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

func cleanupCLIImageFiles(st *interactiveState) {
	for _, p := range st.pendingCLIImageFiles {
		os.Remove(p) //nolint:errcheck
	}
	st.pendingCLIImageFiles = nil
}

// isEOFOrClosed reports whether err is a normal PTY/pipe EOF or "file already closed"
// that occurs when the PTY master is closed after the child exits.
func isEOFOrClosed(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	// os.ErrClosed is wrapped by the PTY read when the master fd is closed.
	return errors.Is(err, os.ErrClosed)
}

// setMouseDragMode switches the terminal's mouse-tracking level between the
// at-rest mode (1000, X10 basic — reliable wheel-scroll reporting) and the
// drag mode (1002, button-motion — needed to receive MouseActionMotion events
// for live selection-highlight updates). Call with true on drag start and
// false on drag end; safe to call redundantly.
func setMouseDragMode(dragging bool) {
	if dragging {
		os.Stdout.WriteString("\x1b[?1000l\x1b[?1002h") //nolint:errcheck
	} else {
		os.Stdout.WriteString("\x1b[?1002l\x1b[?1000h") //nolint:errcheck
	}
}

func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	ev := tea.MouseEvent(msg)
	region, regionX := m.regionAt(ev.X)
	switch ev.Button {
	case tea.MouseButtonWheelUp:
		switch region {
		case regionMemory:
			if m.panelOffset > 0 {
				m.panelOffset--
			}
		case regionTasks:
			if m.tasksOffset > 0 {
				m.tasksOffset--
			}
		case regionWorkflow:
			if m.workflowPanelOffset > 0 {
				m.workflowPanelOffset--
			}
		default:
			m.vp.ScrollUp(3)
		}
	case tea.MouseButtonWheelDown:
		h := m.viewportHeight()
		switch region {
		case regionMemory:
			if m.panelOffset < m.panelMaxOffset(regionMemory, h) {
				m.panelOffset++
			}
		case regionTasks:
			if m.tasksOffset < m.panelMaxOffset(regionTasks, h) {
				m.tasksOffset++
			}
		case regionWorkflow:
			if m.workflowPanelOffset < m.panelMaxOffset(regionWorkflow, h) {
				m.workflowPanelOffset++
			}
		default:
			m.vp.ScrollDown(3)
		}
	case tea.MouseButtonLeft:
		if region == regionMemory || region == regionWorkflow {
			return m.handlePanelMouse(region, regionX, ev)
		}
		// Only handle events inside the viewport area (rows 2..height-2).
		const vpRowStart = 2
		vpRowEnd := m.height - 2
		if ev.Y < vpRowStart || ev.Y >= vpRowEnd || region != regionNone {
			break
		}
		contentLine := m.vp.YOffset + (ev.Y - vpRowStart)
		switch ev.Action {
		case tea.MouseActionPress:
			if ev.Ctrl && m.selAnchorLine >= 0 {
				// Extend existing selection to clicked position.
				m.selEndLine = contentLine
				m.selEndCol = ev.X
				m.selDragging = true
				m.selText = m.selectionText()
				m.setViewportContent()
				setMouseDragMode(true)
				break
			}
			m.clearPanelSelection()
			m.selAnchorLine = contentLine
			m.selAnchorCol = ev.X
			m.selEndLine = -1
			m.selEndCol = 0
			m.selDragging = false
			m.selText = ""
			m.setViewportContent()
			setMouseDragMode(true)
		case tea.MouseActionMotion:
			if m.selAnchorLine >= 0 {
				m.selDragging = true
				m.selEndLine = contentLine
				m.selEndCol = ev.X
				m.setViewportContent()
			}
		case tea.MouseActionRelease:
			setMouseDragMode(false)
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
			// A drag whose release was dropped can leave mouse mode stuck at 1002;
			// any subsequent click reliably arrives, so reset it defensively here.
			setMouseDragMode(false)
			// Finalize any in-progress drag selection that lost its release event
			// (release can be dropped when pointer drifts outside viewport bounds).
			if m.selText == "" && m.selAnchorLine >= 0 && m.selDragging {
				m.selText = m.selectionText()
				m.setViewportContent()
			}
			if m.panelSelText == "" && m.panelSelAnchorLine >= 0 && m.panelSelDragging {
				m.panelSelText = panelSelectionText(m.panelSelLines(), m.panelSelAnchorLine, m.panelSelAnchorCol, m.panelSelEndLine, m.panelSelEndCol)
			}
			// Transcript selection takes priority; then panel selection; then keyboard input selection.
			if m.selText != "" {
				copyToClipboard(m.selText)
				m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(m.selText)))
				m.clearSelection()
				m.setViewportContent()
				return m, copyFeedbackClearCmd()
			}
			if m.panelSelText != "" {
				copyToClipboard(m.panelSelText)
				m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(m.panelSelText)))
				m.clearPanelSelection()
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

	primaryName := m.st.primaryAgentName()
	escName := m.st.escalationAgentName()

	switch {
	case !m.hasInferenceAgent:
		// No provider configured at all — show setup guidance.
		lines = append(lines,
			yellow("no primary agent configured"),
			"",
			dim("run the setup wizard to get started:"),
			"› /config init",
			"",
			dim("or add a backend directly:"),
			"",
			dim("llama.cpp · Ollama · vLLM"),
			"› /agent add url=http://localhost:8080 provider=local model=qwen2.5-coder",
			"",
			dim("AWS Bedrock"),
			"› /agent add url=https://bedrock-runtime.<region>.amazonaws.com provider=bedrock model=<arn>",
			"",
			dim("OpenRouter · Together · Groq"),
			"› /agent add url=https://openrouter.ai/api/v1 provider=bearer api_key=<key> model=<id>",
			"",
			dim("GitHub Copilot"),
			"› /agent add provider=copilot model=gpt-4o",
			"",
			dim("Claude CLI"),
			"› /agent add provider=claude-cli model=claude-sonnet-4-5",
			"",
			dim("Aider / custom subprocess"),
			"› /agent add provider=aider-cli model=<model>",
			"",
		)
		if !escalationAvail {
			lines = append(lines,
				dim("no escalation agent configured — run /config init to add one"),
				"",
			)
		}
		lines = append(lines, dim("/help for all commands"))
	case !localAvail && !escalationAvail:
		lines = append(lines,
			yellow("no agents available"),
			"",
			dim(primaryName+" unreachable — check your provider config with /agent"),
			dim(escName+" not available — escalation disabled"),
			"",
			dim("/help for available commands"),
		)
	case !localAvail:
		lines = append(lines,
			dim("type a message and press Enter to start"),
			dim(primaryName+" unreachable — use /agent to check or switch backends"),
			dim("/help for available commands"),
		)
	case !escalationAvail:
		lines = append(lines,
			dim("type a message and press Enter to start"),
			"",
			dim("routing: "+primaryName+"  ·  escalation disabled"),
			dim("run /config init to add an escalation agent"),
			"",
			dim("/help for all commands  ·  /config init to reconfigure"),
		)
	default:
		lines = append(lines,
			dim("type a message and press Enter to start"),
			"",
			dim("routing: "+primaryName+" ↔ "+escName+"  ·  /escalate to pin  ·  /primary to unpin"),
			dim("/need — set current goal  ·  /panel memory — memory panel  ·  /think on — reasoning tokens"),
			dim("/config — view config  ·  /config init — reconfigure  ·  /config open — edit in $EDITOR"),
			dim("--new — fresh session  ·  --resume — resume last session  ·  /help for all commands"),
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
	if m.workflowResumeInit != nil {
		cmds = append(cmds, m.workflowResumeInit)
	}
	if m.st.cfg.ShouldCheckUpdate() {
		cfg := m.st.cfg
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			rel, err := updater.CheckLatest(ctx, version, cfg.UpdateCheckIncludePrerelease())
			if err != nil || rel == nil {
				return nil
			}
			return updateAvailableMsg{release: rel}
		})
	}
	return tea.Batch(cmds...)
}

// --- Update ---

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleResize(msg)

	case tea.KeyMsg:
		if m.ptyPane != nil {
			return m.handlePTYKey(msg)
		}
		if m.pendingDirectBash != nil {
			return m.handleDirectBashKey(msg)
		}
		if m.pendingPerm != nil {
			return m.handlePermKey(msg)
		}
		if m.pendingPathPaste != "" {
			return m.handlePathPasteKey(msg)
		}
		if m.pendingForget != nil {
			return m.handleForgetKey(msg)
		}
		if m.pendingAdd != nil {
			return m.handleAddAgentKey(msg)
		}
		if m.pendingMCPAdd != nil {
			return m.handleAddMCPKey(msg)
		}
		if m.pendingSwitch != nil {
			return m.handleSwitchAgentKey(msg)
		}
		if m.pendingTelegramSetup != nil {
			return m.handleTelegramSetupKey(msg)
		}
		if m.pendingInit != nil {
			return m.handleInitWizardKey(msg)
		}
		if m.pendingWorkflowWizard != nil {
			return m.handleWorkflowWizardKey(msg)
		}
		if m.pendingWorkflowExtend != nil {
			return m.handleWorkflowExtendKey(msg)
		}
		if m.inputLocked() {
			return m.handleBusyKey(msg)
		}
		return m.handleKey(msg)

	case remoteInputMsg:
		if !m.busy && msg.text != "" {
			return m.submitInput(msg.text, dim("[telegram]")+" ")
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

	case oauthRequiredMsg:
		notice := milkTag() + " MCP OAuth authorization required"
		if msg.authURL != "" {
			notice += "\n" + milkTag() + " authorization URL: " + msg.authURL
		}
		notice += "\n" + milkTag() + " run " + bold("/mcp auth <server-name>") + " to authorize"
		m.appendTranscript(notice + "\n")
		m.syncLayout()
		return m, nil

	case mcpAuthDoneMsg:
		m.appendTranscript(fmt.Sprintf("%s OAuth authorization for MCP server %q completed — use /mcp reconnect to connect\n", milkTag(), msg.serverName))
		m.syncLayout()
		return m, nil

	case toolUseMsg:
		m.activeToolUse = msg.name
		return m, nil

	case clipboardAttachMsg:
		defer os.Remove(msg.path)
		m = m.handleAttachCmd(msg.path)
		return m, nil

	case clipboardNoToolMsg:
		m.appendTranscript(milkTag() + " clipboard paste: no non-text content found — on WSL2 powershell.exe is used automatically; on X11 install " + bold("xclip") + "; on Wayland install " + bold("wl-paste") + "\n")
		return m, nil

	case prefixChunkMsg:
		m.currentTurnChars += int64(len(msg.text))
		m.appendTranscript(msg.text)
		return m, nil

	case chunkMsg:
		m.currentTurnChars += int64(len(msg.text))
		m.appendTranscript(msg.text)
		// Intra-turn loop detection: feed chunk and check for repetition.
		if m.loopDetector != nil {
			for _, v := range m.loopDetector.FeedChunk(msg.text) {
				m.appendTranscript(yellow(fmt.Sprintf("\n[⚠ loop detected: %s (confidence %.0f%%)]\n", v.Message, v.Confidence*100)))
				if v.ShouldInterrupt {
					m.loopInterrupt = true
					if m.cancelTurn != nil {
						m.cancelTurn()
					}
				}
			}
		}
		return m, nil

	case thinkChunkMsg:
		m.currentTurnChars += int64(len(msg.text))
		m.currentTurnThinking.WriteString(msg.text)
		m.appendThinking(msg.text)
		return m, nil

	case agentDoneMsg:
		return m.handleAgentDone(msg)

	case ptyOutputMsg:
		// Clear the pending flag so the read goroutine can enqueue the next refresh.
		if m.ptyPane != nil {
			atomic.StoreInt32(&m.ptyPane.pending, 0)
		}
		// Returning m triggers View() which redraws the PTY pane.
		return m, nil

	case directBashDoneMsg:
		m.busy = false
		m.cancelTurn = nil
		m.busyHint = ""
		if m.ptyPane != nil {
			// Snapshot the VT screen and append cleaned output to the transcript.
			out := m.ptySnapshot()
			if out != "" {
				m.appendTranscript(dimLines(out))
				m.currentTurnChars += int64(len(out))
			}
			_ = m.ptyPane.ptm.Close()
			m.ptyPane = nil
		}
		if msg.err != nil {
			if exitErr, ok := msg.err.(*exec.ExitError); ok {
				m.appendTranscript(fmt.Sprintf("%s exit %d\n", dim("[sh]"), exitErr.ExitCode()))
			} else if !isEOFOrClosed(msg.err) {
				m.appendTranscript(fmt.Sprintf("%s direct-bash error: %v\n", milkTag(), msg.err))
			}
		}
		m.appendTranscript("\n")
		m.refreshPrompt()
		m.syncLayout()
		return m, nil

	case turnTimeoutWarningMsg:
		if m.busy {
			m.appendTranscript(dim(fmt.Sprintf("[%s is taking longer than expected — still waiting]", msg.agentName)) + "\n")
			m.syncLayout()
		}
		return m, nil

	case wfdev.WorkflowProgressMsg:
		if msg.State.WorkflowName != "" {
			st := msg.State
			m.workflowState = &st
		}
		m.workflowPanelOpen = true
		m.lastWorkflowActivity = time.Now()
		m.workflowTimeoutWarned = false
		m.syncLayout()
		return m, nil

	case workflow.WorkflowChunkMsg:
		m.currentTurnChars += int64(len(msg.Text))
		m.appendTranscript(msg.Text)
		m.lastWorkflowActivity = time.Now()
		m.workflowTimeoutWarned = false
		m.syncLayout()
		return m, nil

	case workflowIdleCheckMsg:
		if !m.busy || m.workflowState == nil {
			return m, nil // workflow finished or was cancelled — stop rescheduling
		}
		if !m.workflowTimeoutWarned {
			agentName := m.workflowState.AgentMap[m.workflowState.Role]
			if agentCfg, ok := findAgentByName(m.st.cfg, agentName); ok {
				timeout := m.st.cfg.AgentTurnTimeout(agentCfg)
				if timeout > 0 && time.Since(m.lastWorkflowActivity) >= timeout {
					m.workflowTimeoutWarned = true
					m.appendTranscript(dim(fmt.Sprintf("[%s (%s) is taking longer than expected — still waiting; Ctrl+C to interrupt]", agentName, m.workflowState.Role)) + "\n")
					m.syncLayout()
				}
			}
		}
		return m, workflowIdleCheck()

	case workflow.WorkflowQuestionsMsg:
		// Designer identified ambiguities — present questions to user.
		// Unblock input so the user can type answers. The workflow goroutine
		// is blocked on cfg.AnswersCh; we forward that exact channel here so
		// the user's reply reaches it.
		m.pendingWorkflowQuestions = msg.Questions
		m.workflowAnswersCh = msg.AnswersCh // use the workflow goroutine's channel
		m.busy = false                      // allow user input
		m.busyHint = ""
		if m.workflowState != nil {
			// Distinguish "waiting on you" from "still generating" in the panel —
			// otherwise it keeps showing "role: designer" throughout the pause too.
			m.workflowState.Role = "waiting for your answers"
		}
		m.appendTranscript("\n" + milkTag() + " Designer has questions:\n")
		m.appendTranscript(msg.Questions + "\n\n")
		m.appendTranscript(milkTag() + " Please provide your answers (or press Enter to use defaults):\n")
		m.refreshPrompt()
		m.syncLayout()
		return m, nil

	case wfdev.WorkflowDoneMsg:
		m.busy = false
		m.cancelTurn = nil
		m.busyHint = ""
		obs.IncrementTurnCount()
		m.currentTurnChars = 0
		m.currentTurnInputChars = 0
		m.workflowPanelOpen = true
		if m.workflowState != nil {
			m.workflowState.Role = "done"
		}
		if m.interrupted {
			m.interrupted = false
			m.appendTranscript(dim("[interrupted]") + "\n")
		} else if isContextCanceled(msg.Err) {
			m.appendTranscript(dim("[interrupted]") + "\n")
		} else if msg.Err != nil {
			var exhausted *wfdev.ErrPassesExhausted
			if errors.As(msg.Err, &exhausted) && m.workflowState != nil && m.pendingWorkflowWizard == nil {
				// Offer to continue with doubled pass limit.
				w := &workflowWizardState{
					name:       m.workflowState.WorkflowName,
					task:       m.workflowState.Task,
					designer:   m.workflowState.AgentMap["designer"],
					generator:  m.workflowState.AgentMap["generator"],
					evaluator:  m.workflowState.AgentMap["evaluator"],
					workflowID: m.workflowState.WorkflowID,
				}
				if w.designer == "" {
					w.designer = workflow.AliasEscalation
				}
				if w.generator == "" {
					w.generator = workflow.AliasEscalation
				}
				if w.evaluator == "" {
					w.evaluator = workflow.AliasEscalation
				}
				m.pendingWorkflowExtend = &workflowExtendState{
					wizard:    w,
					sprint:    exhausted.Sprint,
					maxPasses: exhausted.MaxPasses,
				}
				m.appendTranscript(fmt.Sprintf(
					"%s workflow: sprint %d exhausted %d passes — continue with %d passes? [y/n] ",
					milkTag(), exhausted.Sprint, exhausted.MaxPasses, exhausted.MaxPasses*2,
				))
			} else {
				m.appendTranscript(milkTag() + " workflow error: " + msg.Err.Error() + "\n")
			}
		} else {
			m.appendTranscript(milkTag() + " workflow complete\n")
		}
		m.colorizeForce = true
		m.refreshPrompt()
		m.syncLayout()
		return m, nil

	case workflowResumeCheckMsg:
		if msg.state != nil {
			st := msg.state
			m.workflowState = st
			m.workflowPanelOpen = true
			if st.Role != "done" {
				m.appendTranscript(fmt.Sprintf(
					"%s workflow %s in progress (sprint %d pass %d) — /workflow resume to continue, or ignore\n",
					milkTag(), st.WorkflowName, st.Sprint, st.Pass,
				))
			}
			m.syncLayout()
		}
		return m, nil

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
				newAgent.WithLogContext(m.st.cfg.Otel.LogContext)
				ist := m.st
				newAgent.WithOnTokens(func(model, role string, prompt, completion, cacheRead, cacheCreation int64) {
					ist.sess.AddTokensFull(model, role, prompt, completion, cacheRead, cacheCreation)
				})
				newAgent = attachMCPToolSet(m.ctx, m.st.cfg, activeLocalAgentConfig(m.st.cfg).Name, newAgent)
				m.agents.local = newAgent
				m.agents.localAvail = newAgent.Ping(m.ctx) == nil
				m.agents.primary = newLocalRunner(newAgent, activeLocalAgentConfig(m.st.cfg).Name)
				m.rtr = router.New(m.st.cfg, newAgent)
			}
			// For token_cmd providers the transport already holds the token
			// internally; no agent rebuild is needed.
		}
		return m, nil

	case quitPendingClearMsg:
		m.quitPending = false
		return m, nil

	case hintDebounceMsg:
		if msg.gen == m.hintDebounceGen {
			m.rebuildInlineHints()
			m.syncLayout()
		}
		return m, nil

	case memoryRefreshMsg:
		if m.panelMemory {
			return m, memoryPollTick()
		}
		return m, nil

	case configReloadMsg:
		if msg.err != nil {
			m.appendTranscript(fmt.Sprintf("%s config reload error: %v\n", milkTag(), msg.err))
			return m, nil
		}
		// Detect MCP server changes — connections are NOT restarted on reload.
		hasMCPChange := len(msg.cfg.MCPServers) != len(m.st.cfg.MCPServers)
		// Detect whether anything user-visible actually changed. Metadata-only
		// writes (e.g. update_last_check saved on startup) must not show a banner
		// that hides the splash screen.
		oldCfg, newCfg := m.st.cfg, msg.cfg
		oldCfg.UpdateLastCheck, newCfg.UpdateLastCheck = "", ""
		oldCfg.UpdateSkippedVersion, newCfg.UpdateSkippedVersion = "", ""
		contentChanged := !reflect.DeepEqual(oldCfg, newCfg)
		m.st.cfg = msg.cfg
		if contentChanged {
			m.appendTranscript(milkTag() + " config reloaded\n")
		}
		if hasMCPChange {
			m.appendTranscript(milkTag() + " note: mcp_servers changed — restart milk to apply MCP connection changes\n")
		}
		return m, nil

	case openFileMsg:
		return m.handleOpenFileMsg(msg)

	case updateAvailableMsg:
		m.pendingUpdate = msg.release
		// Record last-check time so we don't spam on every startup.
		cfg := m.st.cfg
		cfg.UpdateLastCheck = time.Now().UTC().Format(time.RFC3339)
		_ = config.Save(cfg)
		m.st.cfg = cfg
		return m, nil

	case updateProgressMsg:
		m.updateProgress = msg.done
		m.updateTotal = msg.total
		return m, nil

	case updateDoneMsg:
		m.updateInstalling = false
		m.updateProgress = 0
		m.updateTotal = 0
		if msg.err != nil {
			m.appendTranscript(fmt.Sprintf("%s update failed: %v\n", milkTag(), msg.err))
		} else {
			m.pendingUpdate = nil
			m.appendTranscript(milkTag() + " update applied — restart milk to use the new version\n")
		}
		return m, nil

	case serverStartDoneMsg:
		if msg.err != nil {
			m.appendTranscript(fmt.Sprintf("%s server start failed: %v\n", milkTag(), msg.err))
		} else if msg.pid != 0 {
			m.appendTranscript(fmt.Sprintf("%s server for %q started  pid=%d  url=%s\n", milkTag(), msg.agentName, msg.pid, msg.url))
		} else {
			m.appendTranscript(fmt.Sprintf("%s server for %q started  url=%s\n", milkTag(), msg.agentName, msg.url))
		}
		return m, nil

	case serverStopDoneMsg:
		if msg.err != nil {
			m.appendTranscript(fmt.Sprintf("%s server stop failed: %v\n", milkTag(), msg.err))
		} else if msg.stopped {
			m.appendTranscript(fmt.Sprintf("%s server for %q stopped\n", milkTag(), msg.agentName))
		} else {
			m.appendTranscript(fmt.Sprintf("%s no tracked server process for %q (not started by milk)\n", milkTag(), msg.agentName))
		}
		return m, nil

	case errMsg:
		m.appendTranscript(fmt.Sprintf("%s error: %v\n", milkTag(), msg.err))
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

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Bracketed paste — let the textarea handle it directly.
	// Pre-expand to terminal height so repositionView() inside ta.Update never
	// scrolls on a large paste (updateTA's +1 is insufficient for multi-line pastes).
	if msg.Paste {
		// Check whether the pasted content looks like a file path that exists.
		// If so, ask the user whether to attach it rather than insert it as text.
		pasted := strings.TrimSpace(string(msg.Runes))
		if isLikelyFilePath(pasted) {
			clean := unquoteFilePath(pasted)
			m.pendingPathPaste = clean
			m.appendTranscript(fmt.Sprintf("%s pasted path %q — attach as file? [Y/n] ", milkTag(), clean))
			m.syncLayout()
			return m, nil
		}
		// Path-like string that doesn't exist on disk (e.g. Windows UNC path
		// or a path to a remote file): insert as @path so the agent sees it.
		if looksLikeFilePath(pasted) {
			m.ta.InsertString("@" + unquoteFilePath(pasted))
			m.syncLayout()
			return m, nil
		}
		// Empty paste: the clipboard may hold binary/non-text content that the
		// terminal couldn't relay. Probe via xclip/wl-paste asynchronously.
		if pasted == "" {
			return m, m.probeClipboardCmd()
		}
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
		if m.panelSelAnchorLine >= 0 {
			m.clearPanelSelection()
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
		if len(m.tabMatches) > 0 {
			// Tab cycling is active: accept the already-inserted completion.
			// If the inserted sig has a parameter placeholder, position the cursor
			// there and clear the placeholder so the user can type the value directly.
			current := m.ta.Value()
			m.tabMatches = nil
			m.tabIdx = -1
			m.tabCmdIdx = 0
			m.tabVarIdx = 0
			m.tabBeforeCursor = ""
			m.tabAfterCursor = ""
			m.tabPrefix = ""
			m.tabSubcmdMode = false
			m.tabHints = nil
			m.tabHintsBase = nil
			m.hintIdx = -1
			if cleared, pos := clearFirstPlaceholder(current); pos >= 0 {
				m.ta.SetValue(cleared)
				m.ta.SetCursor(pos)
				m.rebuildInlineHints()
			}
			m.syncLayout()
			return m, nil
		}
		if m.hintIdx >= 0 {
			if m.commitHintSelection() {
				// Same: position cursor at first placeholder if present.
				if cleared, pos := clearFirstPlaceholder(m.ta.Value()); pos >= 0 {
					m.ta.SetValue(cleared)
					m.ta.SetCursor(pos)
					m.rebuildInlineHints()
				}
				m.syncLayout()
				return m, nil
			}
			m.hintIdx = -1
		}
		return m.handleEnter()
	case "up":
		if len(m.tabHints) > 0 {
			m.hintIdx--
			if m.hintIdx < 0 {
				m.hintIdx = len(m.tabHints) - 1
			}
			if len(m.tabMatches) > 0 {
				m.syncTabIdxFromHint()
				m = m.insertActiveCompletion()
			}
			m.highlightHint()
			m.syncLayout()
			return m, nil
		}
		li := m.ta.LineInfo()
		if m.ta.Line() == 0 && li.RowOffset == 0 {
			m = m.historyBack()
			m.syncLayout()
			return m, nil
		}
	case "down":
		if len(m.tabHints) > 0 {
			m.hintIdx++
			if m.hintIdx >= len(m.tabHints) {
				m.hintIdx = 0
			}
			if len(m.tabMatches) > 0 {
				m.syncTabIdxFromHint()
				m = m.insertActiveCompletion()
			}
			m.highlightHint()
			m.syncLayout()
			return m, nil
		}
		li := m.ta.LineInfo()
		if m.ta.Line() == m.ta.LineCount()-1 && li.RowOffset == li.Height-1 {
			m = m.historyForward()
			m.syncLayout()
			return m, nil
		}
	case "ctrl+up":
		if m.selAnchorLine >= 0 {
			return m.extendTranscriptSel(msg)
		}
		m = m.historyBack()
		m.syncLayout()
		return m, nil
	case "ctrl+down":
		if m.selAnchorLine >= 0 {
			return m.extendTranscriptSel(msg)
		}
		m = m.historyForward()
		m.syncLayout()
		return m, nil
	case "ctrl+left", "ctrl+right":
		if m.selAnchorLine >= 0 {
			return m.extendTranscriptSel(msg)
		}
	case "shift+left", "shift+right", "shift+up", "shift+down", "shift+home", "shift+end",
		"shift+ctrl+left", "shift+ctrl+right", "shift+alt+left", "shift+alt+right",
		"ctrl+shift+left", "ctrl+shift+right":
		if m.selAnchorLine >= 0 {
			return m.extendTranscriptSel(msg)
		}
		return m.handleShiftArrow(msg)
	case "tab":
		if m.hintIdx >= 0 && len(m.tabMatches) == 0 {
			m.commitHintSelection()
			m.syncLayout()
			return m, nil
		}
		m = m.handleTab(1)
		m.syncLayout()
		return m, nil
	case "shift+tab":
		m = m.handleTab(-1)
		m.syncLayout()
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
	m.tabCmdIdx = 0
	m.tabVarIdx = 0
	m.tabBeforeCursor = ""
	m.tabAfterCursor = ""
	m.tabPrefix = ""
	m.tabSubcmdMode = false
	m.tabHints = nil
	m.tabHintsBase = nil

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
	return m, tea.Batch(cmd, m.scheduleHintRebuild())
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
	case "shift+ctrl+left", "shift+alt+left", "ctrl+shift+left":
		bareKey = tea.KeyMsg{Type: tea.KeyLeft, Alt: true}
	case "shift+ctrl+right", "shift+alt+right", "ctrl+shift+right":
		bareKey = tea.KeyMsg{Type: tea.KeyRight, Alt: true}
	default:
		bareKey = tea.KeyMsg{Type: tea.KeyRight}
	}

	cmd := m.updateTA(bareKey)
	m.taSelEnd = m.taCursorOffset()
	m.syncLayout()
	return m, cmd
}

// extendTranscriptSel extends or reduces the current transcript selection using
// Shift+Arrow keys. The anchor stays fixed; only selEndLine/selEndCol moves.
func (m model) extendTranscriptSel(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	lines := m.transcriptPlainLines()
	maxLine := len(lines) - 1

	endLine := m.selEndLine
	endCol := m.selEndCol
	if endLine < 0 {
		// Selection was set via mouse-press only (no drag yet): start end at anchor.
		endLine = m.selAnchorLine
		endCol = m.selAnchorCol
	}

	lineLen := func(l int) int {
		if l < 0 || l > maxLine {
			return 0
		}
		return len([]rune(lines[l]))
	}

	switch msg.String() {
	case "shift+up", "ctrl+up":
		endLine--
	case "shift+down", "ctrl+down":
		endLine++
	case "shift+left", "ctrl+left":
		if endCol > 0 {
			endCol--
		} else if endLine > 0 {
			endLine--
			endCol = lineLen(endLine)
		}
	case "shift+right", "ctrl+right":
		ll := lineLen(endLine)
		if endCol < ll {
			endCol++
		} else if endLine < maxLine {
			endLine++
			endCol = 0
		}
	case "shift+home":
		endCol = 0
	case "shift+end":
		endCol = lineLen(endLine)
	}

	if endLine < 0 {
		endLine = 0
	}
	if endLine > maxLine {
		endLine = maxLine
	}
	ll := lineLen(endLine)
	if endCol < 0 {
		endCol = 0
	}
	if endCol > ll {
		endCol = ll
	}

	m.selEndLine = endLine
	m.selEndCol = endCol
	m.selText = m.selectionText()

	if m.ready {
		// Scroll the viewport to keep the moving end of the selection visible.
		visible0 := m.vp.YOffset
		visible1 := m.vp.YOffset + m.vp.Height - 1
		if endLine < visible0 {
			m.vp.SetYOffset(endLine)
		} else if endLine > visible1 {
			m.vp.SetYOffset(endLine - m.vp.Height + 1)
		}
		m.setViewportContent()
	}
	return m, nil
}

func (m model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.selText != "" {
		copyToClipboard(m.selText)
		m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(m.selText)))
		m.clearSelection()
		m.setViewportContent()
		return m, copyFeedbackClearCmd()
	}
	if m.panelSelText != "" {
		copyToClipboard(m.panelSelText)
		m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(m.panelSelText)))
		m.clearPanelSelection()
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
		m.tabCmdIdx = 0
		m.tabVarIdx = 0
		m.tabBeforeCursor = ""
		m.tabAfterCursor = ""
		m.tabPrefix = ""
		m.tabSubcmdMode = false
		m.tabHints = nil
		m.tabHintsBase = nil
		m.syncLayout()
		return m, nil
	}
	// If designer questions are pending, Ctrl+C cancels the workflow.
	if m.pendingWorkflowQuestions != "" {
		m.pendingWorkflowQuestions = ""
		m.workflowAnswersCh = nil
		if m.cancelTurn != nil {
			m.cancelTurn()
			m.cancelTurn = nil
		}
		m.appendTranscript("\n" + milkTag() + " workflow cancelled\n")
		m.refreshPrompt()
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
	input := strings.TrimSpace(stripCompletionPlaceholders(m.ta.Value()))
	m.ta.Reset()

	m.tabMatches = nil
	m.tabIdx = -1
	m.tabCmdIdx = 0
	m.tabVarIdx = 0
	m.tabBeforeCursor = ""
	m.tabAfterCursor = ""
	m.tabPrefix = ""
	m.tabSubcmdMode = false
	m.tabHints = nil
	m.tabHintsBase = nil
	m.syncLayout()
	m.histIdx = -1
	m.saved = ""

	// If we're collecting designer disambiguation answers, send them to the workflow.
	if m.pendingWorkflowQuestions != "" {
		m.appendTranscript(promptLabel(m.st) + input + "\n")
		if m.workflowAnswersCh != nil {
			m.workflowAnswersCh <- input
			close(m.workflowAnswersCh)
			m.workflowAnswersCh = nil
		}
		m.pendingWorkflowQuestions = ""
		// Re-lock input while the designer finalizes the plan.
		m.busy = true
		m.spinnerFrame = 0
		m.lastWorkflowActivity = time.Now()
		m.workflowTimeoutWarned = false
		if m.workflowState != nil {
			// Keep Role as the canonical "designer" (matches AgentMap's key) so
			// the idle watchdog can still resolve the agent's turn timeout.
			m.workflowState.Role = "designer"
		}
		m.appendTranscript(milkTag() + " Answers submitted. Designer finalizing plan...\n")
		m.syncLayout()
		return m, tea.Batch(spinnerTick(), workflowIdleCheck())
	}

	return m.submitInput(input, promptLabel(m.st))
}

// submitInput handles a finalised user input string from any source (keyboard,
// Telegram, etc.). label is the transcript echo prefix.
func (m model) submitInput(input, label string) (tea.Model, tea.Cmd) {
	if input == "" {
		return m, nil
	}

	// Feed user turn to loop detector (resets turn-flood streak).
	if m.loopDetector != nil {
		m.loopDetector.Feed(loop.TurnSummary{
			Text:       input,
			Timestamp:  time.Now(),
			IsUserTurn: true,
		})
		m.loopInterrupt = false
		m.loopWarning = ""
	}

	m.appendTranscript(label + colorizeTokens(input) + "\n")

	// Append to both histories (deduped)
	m.sessionHistory = appendDeduped(m.sessionHistory, input, maxPersistedHistory)
	m.globalHistory = appendDeduped(m.globalHistory, input, maxPersistedHistory)

	if input == cmdPaste {
		// /paste is also the manual trigger for clipboard binary attachment:
		// terminals (especially Windows Terminal / WSL2) never send a bracketed-paste
		// event when the clipboard holds only binary data (image, PDF, etc.), so the
		// automatic empty-paste hook cannot fire. /paste covers that gap.
		return m, m.probeClipboardCmd()
	}

	if cmd, rest, found := extractSlashCommand(input); found {
		return m.handleSlashInput(cmd, rest)
	}

	// Direct-bash shortcut: if enabled and input looks like a shell command,
	// ask for confirmation before running it locally (or run immediately if
	// the first token is in the allow-list).
	if m.st.cfg.DirectBash {
		if shellCmd, ok := shelldetect.IsShellCommand(input); ok {
			if shelldetect.HasAllowedPrefix(shellCmd, m.st.cfg.DirectBashAllow) {
				return m.launchPTYPane(shellCmd)
			}
			// Show confirmation prompt.
			cmd := shellCmd
			m.pendingDirectBash = &cmd
			m.appendTranscript(milkTag() + " run: " + bold(shellCmd) + "  [Y/n] ")
			m.refreshPrompt()
			return m, nil
		}
	}

	return m.dispatchAgent(input)
}

func (m model) dispatchAgent(input string) (tea.Model, tea.Cmd) {
	m.busy = true
	m.spinnerFrame = 0
	m.currentTurnChars = 0
	m.currentTurnInputChars = 0
	m.currentTurnThinking.Reset()
	m.thinkingActiveInTurn = false
	m.loopInterrupt = false
	m.loopWarning = ""
	if m.loopDetector != nil {
		m.loopDetector.ResetTurn()
	}

	// Apply pending attachments.
	// Text attachments: prepend context blocks to the agent prompt.
	// Image attachments: set ContentParts on the local agent for multipart vision payload;
	//   also inject data-URI blocks into the prompt for the CLI escalation path.
	// Session history uses the original input plus compact [attached: name] placeholders.
	attachments := m.pendingAttachments
	m.pendingAttachments = nil // clear before dispatch

	// agentInput: what gets sent to the agent (may include full attachment content).
	// st.pendingSessionContent: compact version stored in session history (placeholders).
	agentInput := input
	var imageParts []local.ContentPart
	if len(attachments) > 0 {
		var textBlocks strings.Builder
		var imgBlocks strings.Builder
		var ph strings.Builder
		for _, a := range attachments {
			fmt.Fprintf(&ph, " %s", attachmentPlaceholder(a))
			if a.isImage() {
				// Local agent: multipart image_url content part (base64 data URI).
				imageParts = append(imageParts, local.ContentPart{
					Type:     "image_url",
					ImageURL: &local.ImageURLPart{URL: attachmentDataURI(a)},
				})
				if m.agents.escalation != nil && m.agents.escalation.IsCLI() {
					// CLI escalation path: write image to temp file, inject @path reference.
					// The claude binary reads @path natively — no base64 in the prompt needed.
					ext := mimeExtension(a.MIMEType)
					written := false
					if f, err := os.CreateTemp("", "milk-img-*"+ext); err == nil {
						if _, werr := f.Write(a.Data); werr == nil {
							f.Close()
							m.st.pendingCLIImageFiles = append(m.st.pendingCLIImageFiles, f.Name())
							fmt.Fprintf(&imgBlocks, "@%s\n", f.Name())
							written = true
						} else {
							f.Close()
							os.Remove(f.Name())
						}
					}
					if !written {
						fmt.Fprintf(&imgBlocks, "[attached image: %s]\n%s\n\n", a.Name, attachmentDataURI(a))
					}
				} else {
					// Non-CLI escalation (local HTTP, subprocess): inline base64 data URI.
					fmt.Fprintf(&imgBlocks, "[attached image: %s]\n%s\n\n", a.Name, attachmentDataURI(a))
				}
			} else {
				textBlocks.WriteString(attachmentContextBlock(a))
			}
		}
		var sb strings.Builder
		if textBlocks.Len() > 0 {
			sb.WriteString(textBlocks.String())
			sb.WriteByte('\n')
		}
		if imgBlocks.Len() > 0 {
			sb.WriteString(imgBlocks.String())
		}
		sb.WriteString(input)
		agentInput = sb.String()
		obs.Debug("dispatch: agentInput prefix", "agentInput[:100]", func() string {
			if len(agentInput) > 100 {
				return agentInput[:100]
			}
			return agentInput
		}())
		// Record compact form in session history (no full file data).
		m.st.pendingSessionContent = input + ph.String()
	}

	m.pendingForceFresh = m.st.sess.ForceFreshEscalation

	turnCtx, cancel := context.WithCancel(m.ctx)
	m.cancelTurn = cancel

	st := m.st
	rtr := m.rtr
	send := func(msg tea.Msg) { st.program.Send(msg) }
	st.toolFutures = map[string]chan string{}

	ir0 := &tuiInputReader{send: send}
	tuiAgents, _ := m.buildTUIAgents(send, ir0)

	// Wire image parts into the primary local agent so it sends a multipart payload.
	if len(imageParts) > 0 && tuiAgents.local != nil {
		tuiAgents.local.SetPendingImageParts(imageParts)
	}

	// Capture input chars for the live token estimate in the status bar.
	m.currentTurnInputChars = int64(len(agentInput))

	return m, tea.Batch(
		spinnerTick(),
		func() tea.Msg {
			defer cancel()
			sw := &sendWriter{send: send}
			err := runTurn(turnCtx, st, rtr, &tuiAgents, agentInput, sw, ir0)
			return agentDoneMsg{err: err}
		},
	)
}

// buildTUIAgents wires the live TUI callbacks (permission handlers, tool-use
// hints, thinking, skip-permissions) into a fresh copy of m.agents and returns
// it alongside the permContext built for the CLI escalation runner.
// Called by both dispatchAgent (for normal turns) and launchWorkflow (so that
// workflow roles get the same tool access and permission flow as regular turns).
func (m model) buildTUIAgents(send func(tea.Msg), ir0 *tuiInputReader) (dispatchAgents, permContext) {
	st := m.st
	agents := m.agents
	termWidth := m.width

	tuiAgents := agents
	var cliPC permContext
	if agents.cliAgent != nil {
		tuiCliAgent := agents.cliAgent.
			WithSkipPermissions(st.skipPermissions).
			WithOAuthRequiredHandler(func(serverName, authURL string) {
				send(oauthRequiredMsg{serverName: serverName, authURL: authURL})
			}).
			WithOnToolUse(func(name string) {
				send(toolUseMsg{name: name})
			}).
			WithOnToolUseReady(func(name string, input map[string]any) {
				// AskUserQuestion is handled entirely by milk's selection prompt —
				// suppress the ⚙ hint here to keep the transcript clean.
				if name == "AskUserQuestion" {
					return
				}
				var hint string
				summary := truncateToolSummary(name, cliToolArgSummary(input), termWidth)
				if summary != "" {
					hint = "\n" + dimWrap("⚙ "+name+": "+summary) + "\n"
				} else {
					hint = "\n" + dimWrap("⚙ "+name) + "\n"
				}
				if st.cfg.RemoteOversight.NotifyToolsEnabled() {
					st.notifier.NotifyToolUse(context.Background(), name, cliToolArgSummary(input))
				}
				if d := cliToolDiff(name, input); d != "" {
					hint += d
				}
				send(chunkMsg{text: hint})
			}).
			WithOnThinking(func(text string) { send(thinkChunkMsg{text: text}) }).
			WithPermissionHandler(makeTUIPermissionHandler(ir0, st.cs, st.notifier))
		tuiAgents.cliAgent = tuiCliAgent
		cliPC = permContext{cs: st.cs, cwd: st.cwd, toolFutures: st.toolFutures, contextHash: &st.lastEscalationContextHash}
		// Only rebuild escalation runner as cliRunner when cliAgent IS the escalation target.
		if agents.escalationLocal == nil && agents.subprocessAgent == nil {
			escName := agents.escalation.Name()
			cr := newCLIRunner(tuiCliAgent, escName, cliPC, func() inputReader { return ir0 })
			if servers := st.cfg.EffectiveMCPServers(escName); len(servers) > 0 {
				cr = cr.withMCPServers(servers)
			}
			tuiAgents.escalation = cr
		}
	}

	// Wire local-agent permissions: persistent store + TUI ask callback.
	// Both the primary and escalation-local agents share the same store and ask
	// callback — they operate in the same cwd and grants should be shared.
	localPermStore := st.localPerms
	localPermAsk := makeLocalPermAsk(ir0, localPermStore)
	localOpenFile := func(path string) error {
		respCh := make(chan error, 1)
		send(openFileMsg{path: path, respCh: respCh})
		return <-respCh
	}
	localOnToolUse := func(name, summary string) {
		if st.cfg.RemoteOversight.NotifyToolsEnabled() {
			st.notifier.NotifyToolUse(context.Background(), name, summary)
		}
	}
	if agents.local != nil {
		tuiLocalAgent := agents.local.
			WithSkipPermissions(st.skipPermissions).
			WithPermissions(localPermStore, localPermAsk).
			WithOnOpenFile(localOpenFile).
			WithOnToolUse(localOnToolUse).
			WithOnThinking(func(text string) { send(thinkChunkMsg{text: text}) })
		tuiAgents.local = tuiLocalAgent
		tuiAgents.primary = newLocalRunner(tuiLocalAgent, agents.primary.Name())
	}
	if agents.escalationLocal != nil {
		tuiEscLocal := agents.escalationLocal.
			WithSkipPermissions(st.skipPermissions).
			WithPermissions(localPermStore, localPermAsk).
			WithOnOpenFile(localOpenFile).
			WithOnToolUse(localOnToolUse).
			WithOnThinking(func(text string) { send(thinkChunkMsg{text: text}) })
		tuiAgents.escalationLocal = tuiEscLocal
		tuiAgents.escalation = newLocalRunner(tuiEscLocal, agents.escalation.Name())
	}
	return tuiAgents, cliPC
}

// renderScrollbar returns a single-column string of h lines showing a dim │
// track with a bright ▌ thumb proportional to scroll position.
// --- Memory panel poll ---

func memoryPollTick() tea.Cmd {
	return tea.Tick(memoryPollInterval, func(time.Time) tea.Msg {
		return memoryRefreshMsg{}
	})
}

// workflowIdleCheckInterval is how often the workflow idle watchdog checks
// elapsed time against the current role's agent turn timeout.
const workflowIdleCheckInterval = 15 * time.Second

func workflowIdleCheck() tea.Cmd {
	return tea.Tick(workflowIdleCheckInterval, func(time.Time) tea.Msg {
		return workflowIdleCheckMsg{}
	})
}

// --- Spinner ---

func spinnerTick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// --- Agent dispatch ---

// replTurnSourceLabel returns the "source" label for milk.turns.total based on
// TUI routing state: user (sticky/force), auto_sticky, or auto (router-decided).
func replTurnSourceLabel(st *interactiveState) string {
	if st.stickyEscalate || st.stickyPrimary {
		return "user"
	}
	if st.autoStickyEscalate {
		return "auto_sticky"
	}
	return "auto"
}

// runTurn routes a prompt to the appropriate agent, writing output to out.
func runTurn(ctx context.Context, st *interactiveState, rtr *router.Router, agents *dispatchAgents, input string, out io.Writer, ir ...inputReader) error {
	localAvail := agents.localAvail
	escalationAvail := agents.escalationAvail

	// Route first (fast), then apply the per-agent timeout so long-running
	// escalation agents can have a higher limit than the local default.
	forceEscalate := st.forceEscalate || st.stickyEscalate || st.autoStickyEscalate
	forcePrimary := st.forcePrimary || st.stickyPrimary
	routeCtx, routeCancel := context.WithTimeoutCause(ctx, agentTimeout, fmt.Errorf("turn timeout"))
	decision, routeErr := rtr.Route(routeCtx, st.sess, input, forceEscalate, forcePrimary)
	routeCancel()
	if routeErr != nil {
		return fmt.Errorf("routing: %w", routeErr)
	}
	st.forceEscalate = false
	// A forcePrimary turn (single-turn /primary <prompt>) breaks auto-sticky so
	// the next turn is re-evaluated by the router rather than staying on escalation.
	if st.forcePrimary {
		st.autoStickyEscalate = false
	}
	st.forcePrimary = false
	// stickyEscalate/stickyPrimary/autoStickyEscalate persist until explicitly cleared.

	target := decision.Target
	if target == router.TargetLocal && !localAvail {
		target = router.TargetEscalation
		st.activeFallbackTarget = "escalation"
	} else if target == router.TargetEscalation && !escalationAvail {
		target = router.TargetLocal
		st.activeFallbackTarget = "primary"
	} else {
		st.activeFallbackTarget = ""
	}
	defer func() { st.activeFallbackTarget = "" }()

	targetName := "local"
	agentName := st.cfg.ActiveAgent().Name
	agentCfg := st.cfg.ActiveAgent()
	if target == router.TargetEscalation {
		targetName = "escalation"
		agentCfg = st.cfg.EscalationAgentConfig()
		agentName = agentCfg.Name
	}

	// Apply the per-agent turn timeout now that we know the target.
	// The turn context has no hard deadline — instead, a warning is sent after the
	// timeout period if the turn is still running, and the agent continues.
	// The turn only terminates on explicit user cancellation (Ctrl+C) or completion.
	timeout := st.cfg.AgentTurnTimeout(agentCfg)
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if timeout > 0 {
		go func() {
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				if st.program != nil {
					st.program.Send(turnTimeoutWarningMsg{agentName: agentName})
				}
			case <-turnCtx.Done():
			}
		}()
	}
	st.notifier.NotifyTurnStart(turnCtx, agentName, targetName, input)

	sourceLabel := replTurnSourceLabel(st)
	turnStart := time.Now()
	var turnErr error
	var pw io.Writer
	if sw, ok := out.(*sendWriter); ok {
		pw = &prefixWriter{send: sw.send}
	} else {
		pw = out
	}
	// onResponse is called by the dispatch layer with the agent's final text,
	// giving us the exact text from this turn without re-scanning history.
	var lastResponseText string
	onResponse := func(text string) { lastResponseText = text }

	// sessionContent is the compact version stored in history (with attachment
	// placeholders). Falls back to input when no attachment override is set.
	sessionContent := input
	if st.pendingSessionContent != "" {
		sessionContent = st.pendingSessionContent
		st.pendingSessionContent = ""
	}

	switch target {
	case router.TargetLocal:
		if mem := st.mem; mem != nil {
			defer func() {
				_ = mem.Consolidate()
				_ = mem.PruneGlobal(st.cfg.PerceptStoreSizeLimit())
			}()
		}
		// Local path: CLI image temp files not needed; clean up.
		cleanupCLIImageFiles(st)
		turnErr = runPrimaryWithSession(turnCtx, st.cfg, st.sess, agents.primary, agents.escalation, st.mem, input, sessionContent, out, agents, onResponse, pw)
	case router.TargetEscalation:
		imageCtxFile := st.pendingImageContextFile
		st.pendingImageContextFile = ""
		turnErr = runEscalationWithSession(turnCtx, st.cfg, st.sess, agents.escalation, "", st.mem, input, sessionContent, imageCtxFile, out, onResponse, pw)
		// CLI image temp files are no longer needed after the turn.
		cleanupCLIImageFiles(st)
	}
	targetLabel := string(target)
	obs.Inc(turnCtx, milkScope, "milk.turns.total",
		attribute.String("target", targetLabel),
		attribute.String("source", sourceLabel),
	)
	obs.RecordDuration(turnCtx, milkScope, "milk.turns.latency_ms", time.Since(turnStart),
		attribute.String("target", targetLabel),
	)
	if turnErr != nil {
		obs.Inc(turnCtx, milkScope, "milk.turns.errors",
			attribute.String("target", targetLabel),
			attribute.String("kind", "inference"),
		)
	}
	st.notifier.NotifyTurnDone(turnCtx, agentName, turnErr)
	if turnErr == nil {
		if lastResponseText != "" {
			st.notifier.NotifyResponse(turnCtx, agentName, lastResponseText)
		}
		// Auto-sticky: if the router decided to escalate (not user-pinned) and the
		// turn succeeded, keep subsequent turns on the escalation agent.
		// Explicit /escalate uses stickyEscalate (pinned) and is unaffected by this.
		if target == router.TargetEscalation &&
			!st.stickyEscalate && !st.forceEscalate &&
			st.cfg.StickyEscalationEnabled() {
			st.autoStickyEscalate = true
		}
	}
	return turnErr
}

// --- runREPL entry point ---

func runREPL(cfg config.Config, cwd string, initialFlagNew bool, initialFlagSession string) error {
	sess, err := loadSession(cwd, initialFlagNew, initialFlagSession)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}

	sessionStart := time.Now()
	obsShutdown := initObs(cfg)
	defer func() {
		obs.SetGauge(context.Background(), milkScope, "milk.session.duration_ms",
			time.Since(sessionStart).Milliseconds(),
		)
		obsShutdown(context.Background()) //nolint:errcheck
	}()

	var mem *memory.Store
	if dir, err := memoryDir(); err == nil {
		if m, err := memory.NewStore(dir, sess.ID); err == nil {
			mem = m
		}
	}

	var taskStore *tasks.Store
	if dir, err := config.Dir(); err == nil {
		tasksDir := dir + "/tasks"
		if ts, err := tasks.New(tasksDir, sess.ID); err == nil {
			taskStore = ts
		}
	}

	// Build the primary agent. When the active agent is a subprocess provider
	// (subprocess, aider-cli), bypass the HTTP local agent.
	tuiPrimaryAC := cfg.ActiveAgent()
	var localAgent *local.Agent
	var tuiSubprocessPrimaryAgent *subprocess.Agent
	if tuiPrimaryAC.IsExternalProcess() && !tuiPrimaryAC.IsCLI() {
		switch {
		case tuiPrimaryAC.IsSubprocess():
			if tuiPrimaryAC.Bin == "" {
				if scriptPath, scriptErr := ensureSmolagentScript(); scriptErr != nil {
					return fmt.Errorf("building subprocess primary agent: %w", scriptErr)
				} else {
					tuiPrimaryAC.Bin = scriptPath
				}
			}
			tuiSubprocessPrimaryAgent = smolagent.New(tuiPrimaryAC)
		case tuiPrimaryAC.IsAiderCLI():
			tuiSubprocessPrimaryAgent = aider.New(tuiPrimaryAC)
		}
		if tuiSubprocessPrimaryAgent != nil {
			tuiSubprocessPrimaryAgent = tuiSubprocessPrimaryAgent.WithLogContext(cfg.Otel.LogContext)
			if dbg, err := openSubprocessDebugLog(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "%s warning: cannot open subprocess debug log: %v\n", milkTag(), err)
			} else if dbg != nil {
				defer dbg.Close()
				tuiSubprocessPrimaryAgent = tuiSubprocessPrimaryAgent.WithDebugLog(dbg)
			}
		}
	} else {
		// Build the local agent without blocking on credential refresh. If
		// aws_auth_refresh is enabled, the agent starts with no/stale credentials
		// and a background goroutine refreshes them after the TUI is running.
		baseAC := activeLocalAgentConfig(cfg)
		localAgent = local.NewFromConfig(baseAC)
		if od, err := config.OtelDir(); err == nil {
			localAgent.WithOtelDir(od)
		}
		localAgent.WithLogContext(cfg.Otel.LogContext)
		if taskStore != nil {
			localAgent = localAgent.WithTaskStore(tasks.NewAdapter(taskStore))
		}
		if dbg, err := openLocalDebugLog(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "%s warning: cannot open local debug log: %v\n", milkTag(), err)
		} else if dbg != nil {
			defer dbg.Close()
			localAgent = localAgent.WithDebugLog(dbg)
		}
	}

	// Build the escalation agent: local provider, subprocess (subprocess, aider-cli), or claude-cli (default).
	tuiEscAC := cfg.EscalationAgentConfig()
	var escalationLocalAgent *local.Agent
	var tuiSubprocessAgent *subprocess.Agent
	switch {
	case tuiEscAC.IsSubprocess():
		if tuiEscAC.Bin == "" {
			if scriptPath, scriptErr := ensureSmolagentScript(); scriptErr != nil {
				return fmt.Errorf("building subprocess escalation agent: %w", scriptErr)
			} else {
				tuiEscAC.Bin = scriptPath
			}
		}
		tuiSubprocessAgent = smolagent.New(tuiEscAC)
	case tuiEscAC.IsAiderCLI():
		tuiSubprocessAgent = aider.New(tuiEscAC)
	default:
	}
	if tuiSubprocessAgent != nil {
		tuiSubprocessAgent = tuiSubprocessAgent.WithLogContext(cfg.Otel.LogContext)
		if dbg, err := openSubprocessDebugLog(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "%s warning: cannot open subprocess debug log: %v\n", milkTag(), err)
		} else if dbg != nil {
			defer dbg.Close()
			tuiSubprocessAgent = tuiSubprocessAgent.WithDebugLog(dbg)
		}
	}
	if !tuiEscAC.IsExternalProcess() {
		escAC := applyFreshAWSCreds(cfg, tuiEscAC)
		if escAC.URL != "" {
			escalationLocalAgent = local.NewFromConfig(escAC).AsEscalationTarget(escAC.Name)
			if od, err := config.OtelDir(); err == nil {
				escalationLocalAgent.WithOtelDir(od)
			}
			escalationLocalAgent.WithLogContext(cfg.Otel.LogContext)
			escalationLocalAgent = escalationLocalAgent.WithSkipPermissions(cliAgentConfig(cfg).DangerouslySkipPermissions)
			if lp, err := local.OpenPermStore(cwd); err == nil {
				escalationLocalAgent.WithPermissions(lp, nil)
			}
		} else {
			fmt.Fprintf(os.Stderr, "%s warning: escalation_agent %q not found in agents — falling back to claude-cli\n", milkTag(), cfg.EscalationAgent)
		}
	}

	cliAgent := newCLIAgent(cliAgentConfig(cfg))
	cliAgent = applyAWSCreds(cfg, cliAgent)
	cliAgent = cliAgent.WithLogContext(cfg.Otel.LogContext)
	if dbg, err := openCLIDebugLog(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: cannot open claude debug log: %v\n", milkTag(), err)
	} else if dbg != nil {
		defer dbg.Close()
		cliAgent = cliAgent.WithDebugLog(dbg)
	}

	ctx := context.Background()

	// If the primary local agent has a run_cmd, launch it when the server is
	// not yet reachable. This happens synchronously so the Ping below reflects
	// the started server. A failure here is non-fatal — milk continues with the
	// server unavailable and the user can start it manually.
	if localAgent != nil {
		primaryAC := activeLocalAgentConfig(cfg)
		if primaryAC.RunCmd != "" && !isReachable(primaryAC.URL) {
			fmt.Fprintln(os.Stderr, milkTag()+" starting local inference server…")
			if err := ensureServerRunning(ctx, primaryAC.URL, primaryAC.RunCmd, primaryAC.Name); err != nil {
				fmt.Fprintln(os.Stderr, milkTag()+" warning: run_cmd failed: "+err.Error())
			}
		}
	}

	// TUI mode continues even when both agents are unavailable so the user can
	// add providers via /agent commands without re-launching.
	var localAvail, escalationAvail bool
	if tuiSubprocessPrimaryAgent != nil {
		localAvail = tuiSubprocessPrimaryAgent.Ping() == nil
		escalationAvail = true // CLI/local escalation checked lazily
		if !localAvail {
			fmt.Fprintln(os.Stderr, milkTag()+" warning: "+tuiPrimaryAC.Name+" primary agent unreachable")
		}
	} else if escalationLocalAgent != nil {
		localAvail = localAgent.Ping(ctx) == nil
		escalationAvail = escalationLocalAgent.Ping(ctx) == nil
		if !localAvail {
			fmt.Fprintln(os.Stderr, milkTag()+" warning: primary agent unreachable — routing all to escalation agent")
		}
		if !escalationAvail {
			fmt.Fprintln(os.Stderr, milkTag()+" warning: escalation agent unreachable — primary only")
		}
	} else if tuiSubprocessAgent != nil {
		localAvail = localAgent.Ping(ctx) == nil
		escalationAvail = tuiSubprocessAgent.Ping() == nil
		if !localAvail {
			fmt.Fprintln(os.Stderr, milkTag()+" warning: primary agent unreachable — routing all to escalation agent")
		}
		if !escalationAvail {
			fmt.Fprintln(os.Stderr, milkTag()+" warning: "+tuiEscAC.Name+" escalation agent unreachable — primary only")
		}
	} else {
		localAvail, escalationAvail, _ = checkAgentAvailability(ctx, localAgent, cliAgent)
	}

	// Pass nil routeLocalAgent when primary is a subprocess (no classifier available).
	var routeLocalAgent *local.Agent
	if localAvail && localAgent != nil {
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

	// Wire token persistence callbacks now that st is available; closures reference
	// st.sess so they always write to the current session even after /new.
	if localAgent != nil {
		localAgent.WithOnTokens(func(model, role string, prompt, completion, cacheRead, cacheCreation int64) {
			st.sess.AddTokensFull(model, role, prompt, completion, cacheRead, cacheCreation)
		})
	}
	if escalationLocalAgent != nil {
		escalationLocalAgent.WithOnTokens(func(model, role string, prompt, completion, cacheRead, cacheCreation int64) {
			st.sess.AddTokensFull(model, role, prompt, completion, cacheRead, cacheCreation)
		})
	}

	// Build TurnRunner instances for dispatch.
	mcpToolSets := map[string]*mcp.ToolSet{}
	var primaryRunner TurnRunner
	switch {
	case tuiSubprocessPrimaryAgent != nil:
		r := newSubprocessRunner(tuiSubprocessPrimaryAgent, tuiPrimaryAC.Name)
		if servers, ts := buildMCPToolSet(context.Background(), cfg, tuiPrimaryAC.Name); ts != nil {
			r = r.withMCPToolSet(servers, ts)
			mcpToolSets[tuiPrimaryAC.Name] = ts
			defer ts.Close(context.Background())
		}
		primaryRunner = r
	case localAgent != nil:
		if _, ts := buildMCPToolSet(context.Background(), cfg, tuiPrimaryAC.Name); ts != nil {
			localAgent = localAgent.WithMCPToolSet(ts)
			mcpToolSets[tuiPrimaryAC.Name] = ts
			defer ts.Close(context.Background())
		}
		primaryRunner = newLocalRunner(localAgent, tuiPrimaryAC.Name)
	}
	var escalationRunner TurnRunner
	switch {
	case tuiSubprocessAgent != nil:
		r := newSubprocessRunner(tuiSubprocessAgent, tuiEscAC.Name)
		if servers, ts := buildMCPToolSet(context.Background(), cfg, tuiEscAC.Name); ts != nil {
			r = r.withMCPToolSet(servers, ts)
			mcpToolSets[tuiEscAC.Name] = ts
			defer ts.Close(context.Background())
		}
		escalationRunner = r
	case escalationLocalAgent != nil:
		if _, ts := buildMCPToolSet(context.Background(), cfg, tuiEscAC.Name); ts != nil {
			escalationLocalAgent = escalationLocalAgent.WithMCPToolSet(ts)
			mcpToolSets[tuiEscAC.Name] = ts
			defer ts.Close(context.Background())
		}
		escalationRunner = newLocalRunner(escalationLocalAgent, tuiEscAC.Name)
	default:
		cliAC := cliAgentConfig(cfg)
		escName := cliAC.Name
		if escName == "" {
			escName = "claude"
		}
		cr := newCLIRunner(cliAgent, escName,
			permContext{cs: cs, cwd: cwd}, func() inputReader { return newStdinInputReader() })
		if servers := cfg.EffectiveMCPServers(cliAC.Name); len(servers) > 0 {
			cr = cr.withMCPServers(servers)
		}
		escalationRunner = cr
	}

	agents := dispatchAgents{
		primary:           primaryRunner,
		escalation:        escalationRunner,
		local:             localAgent,
		cliAgent:          cliAgent,
		escalationLocal:   escalationLocalAgent,
		subprocessAgent:   tuiSubprocessAgent,
		subprocessPrimary: tuiSubprocessPrimaryAgent,
		localAvail:        localAvail,
		escalationAvail:   escalationAvail,
		mcpToolSets:       mcpToolSets,
	}

	m := newModel(ctx, st, rtr, agents, mem)
	m.taskStore = taskStore
	m.hasInferenceAgent = cfg.HasInferenceAgent()
	for _, w := range config.Validate(cfg) {
		m.startupWarnings = append(m.startupWarnings, w.String())
	}
	m.colorizeMode = ParseColorizeMode(cfg.Colorization)
	if needsAWSRefresh(cfg) {
		m.credRefreshing = true
		m.credLabel = "AWS"
	} else if localAgent != nil && needsTokenCmdRefresh(cfg) {
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
	} else if localAgent != nil && needsTokenCmdRefresh(cfg) {
		m.credRefreshInit = func() tea.Msg {
			err := localAgent.WarmToken()
			return credRefreshReadyMsg{label: "token", err: err}
		}
	}

	// Check for a saved workflow state file for the current session.
	// Deferred to Init() for the same reason as credRefreshInit.
	{
		sessID := sess.ID
		m.workflowResumeInit = func() tea.Msg {
			stateDir, err := session.Dir()
			if err != nil {
				return workflowResumeCheckMsg{}
			}
			id, ok, err := workflow.CurrentWorkflowID(stateDir, sessID)
			if err != nil || !ok {
				return workflowResumeCheckMsg{}
			}
			st, err := workflow.LoadState(workflow.StatePath(stateDir, sessID, id))
			if err != nil || st == nil {
				return workflowResumeCheckMsg{}
			}
			return workflowResumeCheckMsg{state: st}
		}
	}

	p := tea.NewProgram(m,
		tea.WithAltScreen(),
	)
	st.program = p

	// Wire task store redraw: when tasks change, send a tick to trigger View().
	if taskStore != nil {
		taskStore.SetOnChange(func() {
			p.Send(memoryRefreshMsg{}) // reuse existing refresh msg to trigger a redraw
		})
	}

	// Start a config watcher so the TUI updates automatically when config.json
	// changes on disk (e.g. the user edits it in another terminal).
	if cfgDir, cfgDirErr := config.Dir(); cfgDirErr == nil {
		cfgPath := cfgDir + "/config.json"
		watcher, watchErr := config.NewWatcher(cfgPath, func(newCfg config.Config, err error) {
			p.Send(configReloadMsg{cfg: newCfg, err: err})
		})
		if watchErr == nil {
			defer watcher.Close()
		}
	}
	if localAgent != nil {
		localAgent.WithOnSigV4Refresh(func(err error) {
			p.Send(credRefreshReadyMsg{label: "AWS", err: err})
		})
	}

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

	// Mode 1000+1006 at rest: X10 basic mouse tracking + SGR extension. Wheel
	// scroll is reliably reported as tea.MouseMsg under 1000; some terminals
	// fall back to arrow-key emulation for wheel scroll when button-motion
	// mode (1002) is left enabled at rest instead. setMouseDragMode switches
	// to 1002 only for the duration of an active drag, when motion events are
	// actually needed for live selection-highlight updates.
	os.Stdout.WriteString("\x1b[?1000h\x1b[?1006h") //nolint:errcheck
	finalModel, err := p.Run()
	os.Stdout.WriteString("\x1b[?1006l\x1b[?1002l\x1b[?1000l") //nolint:errcheck

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
