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
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

// submitMsg is sent when the user submits input.
type submitMsg struct{ text string }

// replResumeMsg is sent after an agent exec finishes.
type replResumeMsg struct{}

// replModel is the bubbletea model for the interactive REPL.
type replModel struct {
	ta         textarea.Model
	promptText string   // plain-text label for width calculation and echo
	history    []string // submitted prompts, oldest first
	histIdx    int      // -1 = live buffer; 0..len-1 = browsing history
	saved      string   // saved live buffer while browsing history
	width      int

	// tab completion state
	tabMatches []string // current candidate list
	tabIdx     int      // index into tabMatches (-1 = not cycling)

	// injected dependencies
	ctx    context.Context
	st     *interactiveState
	rtr    *router.Router
	agents dispatchAgents
}

func newReplModel(ctx context.Context, st *interactiveState, rtr *router.Router, agents dispatchAgents) replModel {
	m := replModel{
		histIdx: -1,
		ctx:     ctx,
		st:      st,
		rtr:     rtr,
		agents:  agents,
	}
	m.ta = m.buildTextarea(st.sess, st.forceEscalate, st.forceLocal)
	return m
}

// buildTextarea constructs a configured textarea using promptLabel for the
// current mode. The label is rendered via SetPromptFunc so it appears on
// line 0; continuation lines get a blank indent of matching width.
func (m *replModel) buildTextarea(sess *session.Session, forceEscalate, forceLocal bool) textarea.Model {
	label := promptLabel(sess, forceEscalate, forceLocal)
	plain := stripANSI(label)
	m.promptText = label

	ta := textarea.New()
	ta.Placeholder = ""
	ta.ShowLineNumbers = false
	ta.SetWidth(120)
	ta.SetHeight(1)
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.BlurredStyle.Base = lipgloss.NewStyle()
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = lipgloss.NewStyle()
	ta.BlurredStyle.Prompt = lipgloss.NewStyle()
	ta.CharLimit = 0

	indent := strings.Repeat(" ", len(plain))
	ta.SetPromptFunc(len(plain), func(lineIdx int) string {
		if lineIdx == 0 {
			return label
		}
		return indent
	})

	// Enter submits; Shift+Enter and Alt+Enter insert a newline.
	km := textarea.DefaultKeyMap
	km.InsertNewline.SetKeys("shift+enter", "alt+enter")
	ta.KeyMap = km

	ta.Focus() //nolint:errcheck
	return ta
}

// updatePrompt rebuilds the textarea prompt when mode changes, preserving content.
func (m *replModel) updatePrompt() {
	content := m.ta.Value()
	m.ta = m.buildTextarea(m.st.sess, m.st.forceEscalate, m.st.forceLocal)
	if content != "" {
		m.ta.SetValue(content)
		m.ta.SetHeight(m.ta.LineCount())
	}
	if m.width > 0 {
		m.ta.SetWidth(m.width - len(stripANSI(m.promptText)))
	}
}

func (m replModel) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		tea.EnableBracketedPaste,
	)
}

func (m replModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.ta.SetWidth(msg.Width - len(stripANSI(m.promptText)))
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case submitMsg:
		return m.handleSubmit(msg.text)
	case replResumeMsg:
		m.updatePrompt()
		return m, nil
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.syncHeight()
	return m, cmd
}

func (m replModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Bracketed paste: insert whole block, then tick textarea for redraw.
	if msg.Paste {
		m.ta.InsertString(string(msg.Runes))
		m.syncHeight()
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		m.syncHeight()
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
			return m.historyBack(), nil
		}
	case "down":
		if m.ta.LineCount() == 1 {
			return m.historyForward(), nil
		}
	case "tab":
		return m.handleTab(), nil
	}
	// Any non-Tab key resets tab cycling.
	m.tabMatches = nil
	m.tabIdx = -1
	// Pre-grow height before inserting a newline so the viewport never scrolls.
	if msg.String() == "shift+enter" || msg.String() == "alt+enter" {
		m.ta.SetHeight(m.ta.LineCount() + 1)
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.syncHeight()
	return m, cmd
}

func (m replModel) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.st.forceEscalate || m.st.forceLocal {
		m.st.forceEscalate = false
		m.st.forceLocal = false
		m.updatePrompt()
		return m, tea.Println(milkTag() + " mode cleared")
	}
	return m, tea.Quit
}

func (m replModel) handleEnter() (tea.Model, tea.Cmd) {
	input := strings.TrimSpace(m.ta.Value())
	m.ta.Reset()
	m.ta.SetHeight(1)
	m.histIdx = -1
	m.saved = ""
	if input == "" {
		return m, nil
	}
	return m, func() tea.Msg { return submitMsg{text: input} }
}

func (m replModel) handleTab() replModel {
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
// For @path completions it swaps the last @token; for slash commands it replaces the whole value.
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
	// Check for trailing @token (path completion)
	for i := len(words) - 1; i >= 0; i-- {
		if strings.HasPrefix(words[i], "@") {
			pathPrefix := words[i][1:] // strip @
			matches := expandPath(pathPrefix, cwd)
			atMatches := make([]string, len(matches))
			for j, m := range matches {
				atMatches[j] = "@" + m
			}
			return atMatches, 0
		}
	}
	// Slash command completion: find slash-prefixed token
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

func (m *replModel) syncHeight() {
	lines := m.ta.LineCount()
	if lines < 1 {
		lines = 1
	}
	m.ta.SetHeight(lines)
}

func (m replModel) historyBack() replModel {
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

func (m replModel) historyForward() replModel {
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

func (m replModel) handleSubmit(input string) (tea.Model, tea.Cmd) {
	if len(m.history) == 0 || m.history[len(m.history)-1] != input {
		m.history = append(m.history, input)
		if len(m.history) > 500 {
			m.history = m.history[1:]
		}
	}

	if input == cmdPaste {
		return m, tea.Println(milkTag() + " hint: paste multi-line text directly, or use Alt+Enter to compose multi-line input")
	}

	if cmd, rest, found := extractSlashCommand(input); found {
		echoLine := m.promptText + input // capture label before mode change
		exit, prompt, output := handleSlashCommand(cmd, rest, m.st)
		m.updatePrompt()
		if exit {
			return m, tea.Quit
		}
		if prompt != "" {
			// Echo must print before agent output — do it synchronously in Run().
			var cmds []tea.Cmd
			if output != "" {
				cmds = append(cmds, tea.Println(output))
			}
			cmds = append(cmds, m.execCmdWithEcho(prompt, echoLine))
			return m, tea.Batch(cmds...)
		}
		// Slash-only command: no exec, tea.Println ordering is fine.
		cmds := []tea.Cmd{tea.Println(echoLine)}
		if output != "" {
			cmds = append(cmds, tea.Println(output))
		}
		return m, tea.Batch(cmds...)
	}

	return m, m.execCmd(input)
}

func (m replModel) execCmd(input string) tea.Cmd {
	return m.execCmdWithEcho(input, m.promptText+input)
}

func (m replModel) execCmdWithEcho(input, echoLine string) tea.Cmd {
	return tea.Exec(
		&replExec{
			ctx:      m.ctx,
			st:       m.st,
			rtr:      m.rtr,
			agents:   m.agents,
			input:    input,
			echoLine: echoLine,
		},
		func(err error) tea.Msg {
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
			return replResumeMsg{}
		},
	)
}

func (m replModel) View() string {
	return colorizeInput(m.ta.View())
}

// colorizeInput colorizes /commands and @paths in the textarea view output.
// It only touches the last line (the live input area); prior lines rendered by
// the textarea (continuation lines) are left as-is to avoid double-coloring.
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
	// Split on ANSI sequences so we only process plain-text tokens.
	// Strategy: find the last occurrence of the prompt text (which contains ANSI),
	// colorize only the content after it.
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
	// Preserve trailing space if original had one
	if len(s) > 0 && s[len(s)-1] == ' ' {
		out.WriteByte(' ')
	}
	return out.String()
}

// --- replExec: suspends TUI and runs an agent turn ---

type replExec struct {
	ctx      context.Context
	st       *interactiveState
	rtr      *router.Router
	agents   dispatchAgents
	input    string
	echoLine string // full line to echo before agent output (prompt label + raw input)
}

func (e *replExec) Run() error {
	if e.echoLine != "" {
		fmt.Println(e.echoLine)
	}
	dispatchTurnDirect(e.ctx, e.st, e.rtr, e.agents, e.input)
	return nil
}

func (e *replExec) SetStdin(r io.Reader)  {}
func (e *replExec) SetStdout(w io.Writer) {}
func (e *replExec) SetStderr(w io.Writer) {}

// stripANSI removes ANSI escape sequences for length calculations.
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

// dispatchTurnDirect routes a prompt to the appropriate agent.
func dispatchTurnDirect(ctx context.Context, st *interactiveState, rtr *router.Router, agents dispatchAgents, input string) {
	localAgent := agents.local
	claudeAgent := agents.claude
	localAvail := agents.localAvail
	claudeAvail := agents.claudeAvail

	turnCtx, cancel := context.WithTimeoutCause(ctx, agentTimeout, fmt.Errorf("turn timeout"))
	defer cancel()

	decision, routeErr := rtr.Route(turnCtx, st.sess, input, st.forceEscalate, st.forceLocal)
	if routeErr != nil {
		fmt.Fprintf(os.Stderr, "routing error: %v\n", routeErr)
		return
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

	switch target {
	case router.TargetLocal:
		if err := runLocal(turnCtx, st.cfg, st.sess, localAgent, input); err != nil {
			fmt.Fprintf(os.Stderr, errFmt, err)
		}
	case router.TargetClaude:
		if err := runClaudeWith(turnCtx, st.sess, claudeAgent, input, newStdinInputReader()); err != nil {
			fmt.Fprintf(os.Stderr, errFmt, err)
		}
	}
	fmt.Println()
}

// runREPL is the entry point for interactive mode.
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

	fmt.Printf("%s interactive mode — session %s  (type /help for commands)\n", milkTag(), sess.ID[:8])
	if cfg.DangerouslySkipPermissions {
		fmt.Fprintf(os.Stderr, "%s\n", red("warning: dangerously_skip_permissions is enabled — Claude will auto-approve all tool uses without prompting"))
	}

	st := &interactiveState{sess: sess, cwd: cwd, cfg: cfg}
	agents := dispatchAgents{localAgent, claudeAgent, localAvail, claudeAvail}

	m := newReplModel(ctx, st, rtr, agents)
	p := tea.NewProgram(m, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout))
	_, err = p.Run()
	return err
}
