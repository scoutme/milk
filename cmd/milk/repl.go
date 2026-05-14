package main

import (
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
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
)

const agentTimeout = 10 * time.Minute

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

// permRequestMsg is sent by the agent goroutine when it needs a y/n answer.
// The agent blocks on respCh until the TUI sends a permResponseMsg back.
type permRequestMsg struct {
	prompt string
	respCh chan string
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
	history []string
	histIdx int
	saved   string

	// tab completion
	tabMatches []string
	tabIdx     int

	// pending permission request (non-nil while waiting for user y/n)
	pendingPerm *permRequestMsg

	// injected dependencies
	ctx    context.Context
	st     *interactiveState
	rtr    *router.Router
	agents dispatchAgents
}

func newModel(ctx context.Context, st *interactiveState, rtr *router.Router, agents dispatchAgents) model {
	ta := buildTextarea()
	return model{
		histIdx:    -1,
		ctx:        ctx,
		st:         st,
		rtr:        rtr,
		agents:     agents,
		ta:         ta,
		transcript: &strings.Builder{},
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
	label := promptLabel(m.st.sess, m.st.forceEscalate, m.st.forceLocal)
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
		m.ta.SetWidth(m.width - promptWidth)
	}
}

// inputLocked returns true when agent is running.
func (m *model) inputLocked() bool { return m.busy }

// handlePermKey routes key events while a permission prompt is pending.
// Only enter submits; anything else is passed to the textarea normally.
func (m model) handlePermKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		// deny and unblock the agent goroutine
		m.pendingPerm.respCh <- "n"
		m.pendingPerm = nil
		m.appendTranscript("n\n")
		return m, nil
	case "enter":
		answer := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.ta.SetHeight(1)
		m.appendTranscript(answer + "\n")
		m.pendingPerm.respCh <- answer
		m.pendingPerm = nil
		return m, nil
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	syncHeight(&m.ta)
	return m, cmd
}

// appendTranscript adds text to the viewport transcript.
func (m *model) appendTranscript(text string) {
	m.transcript.WriteString(text)
	if m.ready {
		m.vp.SetContent(m.wrappedTranscript())
		m.vp.GotoBottom()
	}
}

// wrappedTranscript returns the transcript word-wrapped to the viewport width.
func (m *model) wrappedTranscript() string {
	if m.width <= 0 {
		return m.transcript.String()
	}
	return ansi.Wrap(m.transcript.String(), m.width, "")
}

// viewportHeight calculates the available height for the viewport.
// Layout: viewport | status bar (1 line) | separator (1 line) | textarea (dynamic)
func (m *model) viewportHeight() int {
	taLines := m.ta.LineCount()
	if taLines < 1 {
		taLines = 1
	}
	// status bar (1) + separator border (1) + textarea lines + padding (1)
	reserved := 1 + 1 + taLines + 1
	h := m.height - reserved
	if h < 3 {
		h = 3
	}
	return h
}

// statusBar renders the one-line status bar.
func (m *model) statusBar() string {
	agent := "local"
	switch {
	case m.st.forceEscalate:
		agent = "claude (forced)"
	case m.st.forceLocal:
		agent = "local (forced)"
	case m.st.sess.State == session.StateClaude || m.st.sess.State == session.StateClaudeWaiting:
		agent = "claude"
	}
	sessID := m.st.sess.ID
	if len(sessID) > 8 {
		sessID = sessID[:8]
	}
	cwd := m.st.cwd
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, cwd); err == nil && !strings.HasPrefix(rel, "..") {
			cwd = "~/" + rel
		}
	}
	if m.busy {
		frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
		agent = dim(frame) + " " + agent
	}
	left := fmt.Sprintf(" session:%s  agent:%s", sessID, agent)
	right := cwd + " "
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

// --- Init ---

func (m model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		tea.EnableBracketedPaste,
		tea.EnterAltScreen,
	)
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
		if m.inputLocked() {
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		}
		return m.handleKey(msg)

	case permRequestMsg:
		m.pendingPerm = &msg
		// Print prompt into transcript so user sees what they're answering
		m.appendTranscript(milkTag() + " " + msg.prompt)
		m.ta.Reset()
		m.ta.SetHeight(1)
		return m, nil

	case chunkMsg:
		m.appendTranscript(msg.text)
		return m, nil

	case agentDoneMsg:
		m.busy = false
		if msg.err != nil {
			m.appendTranscript(milkTag() + " error: " + msg.err.Error() + "\n")
		}
		m.appendTranscript("\n")
		m.refreshPrompt()
		return m, nil

	case spinnerTickMsg:
		if m.busy {
			m.spinnerFrame++
			return m, spinnerTick()
		}
		return m, nil

	}

	// Pass remaining messages to viewport and textarea
	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)
	m.ta, cmd = m.ta.Update(msg)
	syncHeight(&m.ta)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m model) handleResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height

	label := promptLabel(m.st.sess, m.st.forceEscalate, m.st.forceLocal)
	promptWidth := len(stripANSI(label))
	m.ta.SetWidth(m.width - promptWidth)

	vpH := m.viewportHeight()
	if !m.ready {
		m.vp = viewport.New(m.width, vpH)
		m.vp.SetContent(m.wrappedTranscript())
		m.vp.GotoBottom()
		m.ready = true
		m.refreshPrompt()
	} else {
		m.vp.Width = m.width
		m.vp.Height = vpH
		m.vp.SetContent(m.wrappedTranscript())
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Bracketed paste — let the textarea handle it directly.
	if msg.Paste {
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		syncHeight(&m.ta)
		return m, cmd
	}

	switch msg.String() {
	case "ctrl+c":
		return m.handleCtrlC()
	case "ctrl+d":
		if m.ta.Value() == "" {
			return m, tea.Quit
		}
	case "enter":
		return m.handleEnter()
	case "up":
		if m.ta.LineCount() == 1 {
			m = m.historyBack()
			return m, nil
		}
	case "down":
		if m.ta.LineCount() == 1 {
			m = m.historyForward()
			return m, nil
		}
	case "tab":
		m = m.handleTab()
		return m, nil
	// Viewport scrolling (works any time input is empty or these are scroll keys)
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
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	syncHeight(&m.ta)
	return m, cmd
}

func (m model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.ta.Value() != "" {
		m.ta.Reset()
		m.ta.SetHeight(1)
		m.tabMatches = nil
		m.tabIdx = -1
		return m, nil
	}
	if m.st.forceEscalate || m.st.forceLocal {
		m.st.forceEscalate = false
		m.st.forceLocal = false
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
	m.histIdx = -1
	m.saved = ""
	if input == "" {
		return m, nil
	}

	// Append echo to transcript
	label := promptLabel(m.st.sess, m.st.forceEscalate, m.st.forceLocal)
	m.appendTranscript(label + colorizeTokens(input) + "\n")

	// Dedupe history
	if len(m.history) == 0 || m.history[len(m.history)-1] != input {
		m.history = append(m.history, input)
		if len(m.history) > 500 {
			m.history = m.history[1:]
		}
	}

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

func (m model) dispatchAgent(input string) (tea.Model, tea.Cmd) {
	m.busy = true
	m.spinnerFrame = 0

	ctx := m.ctx
	st := m.st
	rtr := m.rtr
	agents := m.agents

	send := func(msg tea.Msg) { st.program.Send(msg) }
	return m, tea.Batch(
		spinnerTick(),
		func() tea.Msg {
			sw := &sendWriter{send: send}
			ir := &tuiInputReader{send: send}
			err := runTurn(ctx, st, rtr, agents, input, sw, ir)
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
	if m.vp.Height != vpH {
		m.vp.Height = vpH
	}

	sep := styleBorder.Width(m.width).Render("")
	status := m.statusBar()
	return m.vp.View() + "\n" + status + "\n" + sep + "\n" + colorizeInput(m.ta.View())
}

// --- Spinner ---

func spinnerTick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// --- History ---

func (m model) historyBack() model {
	if len(m.history) == 0 {
		return m
	}
	if m.histIdx == -1 {
		m.saved = m.ta.Value()
		m.histIdx = len(m.history) - 1
	} else if m.histIdx > 0 {
		m.histIdx--
	}
	m.ta.SetValue(m.history[m.histIdx])
	m.ta.SetHeight(m.ta.LineCount())
	return m
}

func (m model) historyForward() model {
	if m.histIdx == -1 {
		return m
	}
	m.histIdx++
	if m.histIdx >= len(m.history) {
		m.histIdx = -1
		m.ta.SetValue(m.saved)
	} else {
		m.ta.SetValue(m.history[m.histIdx])
	}
	m.ta.SetHeight(m.ta.LineCount())
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

func syncHeight(ta *textarea.Model) {
	lines := ta.LineCount()
	if lines < 1 {
		lines = 1
	}
	ta.SetHeight(lines)
}

// --- Input colorizer ---

func colorizeInput(view string) string {
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
	return prefix + promptPart + colorizeTokens(inputPart)
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

	decision, routeErr := rtr.Route(turnCtx, st.sess, input, st.forceEscalate, st.forceLocal)
	if routeErr != nil {
		return fmt.Errorf("routing: %w", routeErr)
	}
	st.forceEscalate = false
	st.forceLocal = false

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
		return runLocal(turnCtx, st.cfg, st.sess, localAgent, input, out)
	case router.TargetClaude:
		return runClaudeWith(turnCtx, st.sess, claudeAgent, input, inputR, out)
	}
	return nil
}

// --- runREPL entry point ---

func runREPL(cfg config.Config, cwd string, initialFlagNew bool, initialFlagSession string) error {
	sess, err := loadSession(cwd, initialFlagNew, initialFlagSession)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}

	localAgent := local.New(cfg.LlamaURL, cfg.LlamaModel)
	claudeAgent := claude.NewWithOpts(cfg.ClaudeBin, cfg.DangerouslySkipPermissions, cfg.AllowedTools, cfg.AddDirs, cfg.EffectivePermissionPhrases(), cfg.EffectiveDirRestrictionPhrases())

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

	st := &interactiveState{sess: sess, cwd: cwd, cfg: cfg}
	agents := dispatchAgents{localAgent, claudeAgent, localAvail, claudeAvail}

	m := newModel(ctx, st, rtr, agents)

	// Prime transcript with welcome line
	welcome := fmt.Sprintf("%s interactive mode — session %s  (type /help for commands)\n",
		milkTag(), sess.ID[:8])
	m.transcript.WriteString(welcome)

	p := tea.NewProgram(m,
		tea.WithAltScreen(),
	)
	st.program = p
	_, err = p.Run()
	return err
}
