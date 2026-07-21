package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/updater"
)

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
	if cmd == cmdConfig {
		return m.handleConfigCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdOpen {
		return m.handleOpenCmd(rest)
	}
	if cmd == cmdUpdate {
		return m.handleUpdateCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdMCP {
		return m.handleMCPCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdWorkflow {
		return m.handleWorkflowCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdServer {
		return m.handleServerCmd(strings.TrimSpace(rest))
	}
	if cmd == "/help" {
		output := renderHelp(interactiveHelp, m.vpWidth())
		m.colorizeForce = true
		m.appendTranscript(output + "\n")
		return m, nil
	}
	exit, dispatch, output := handleSlashCommand(cmd, rest, m.st)
	m.refreshPrompt()
	if exit {
		return m, tea.Quit
	}
	if output != "" {
		m.colorizeForce = true // slash command output may be large — force full re-colorize
		m.appendTranscript(output + "\n")
		m.st.notifier.NotifyResponse(context.Background(), "milk", cmd+": "+output)
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

// handleMCPCmd handles `/mcp [list|add [key=val ...]|remove|enable|disable|tools|assign|unassign|reconnect]`.
// /mcp add with missing required fields launches an interactive wizard; all other
// subcommands are stateless and delegate directly to the exec* helpers.
func (m model) handleMCPCmd(arg string) (model, tea.Cmd) {
	parts := strings.Fields(arg)
	if len(parts) == 0 {
		m.appendTranscript(execMCP("", m.st, m.agents.mcpToolSets) + "\n")
		return m, nil
	}
	verb := parts[0]
	rest := strings.TrimSpace(strings.TrimPrefix(arg, verb))

	if verb == "add" {
		return m.startAddMCP(rest), nil
	}

	m.appendTranscript(execMCP(arg, m.st, m.agents.mcpToolSets) + "\n")
	return m, nil
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

	case strings.HasPrefix(arg, "remove"):
		name := strings.TrimSpace(strings.TrimPrefix(arg, "remove"))
		m.appendTranscript(execAgentRemove(name, m.st) + "\n")

	case arg == "tool", strings.HasPrefix(arg, "tool "):
		sub := strings.TrimSpace(strings.TrimPrefix(arg, "tool"))
		m.appendTranscript(execAgentTool(sub, m.st) + "\n")

	default:
		m.appendTranscript(milkTag() + " usage: /agent [list|add|remove <name>|switch <name>]\n")
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

// execAgentRemove removes the named agent from config.
// Refuses if the agent is currently active as primary or escalation.
func execAgentRemove(name string, st *interactiveState) string {
	if name == "" {
		return milkTag() + " usage: /agent remove <name>"
	}
	primaryName := st.cfg.ActiveAgent().Name
	escalationName := st.cfg.EscalationAgentConfig().Name
	if strings.EqualFold(name, primaryName) {
		return fmt.Sprintf("%s cannot remove %q — it is the active primary agent (switch first with /agent switch)", milkTag(), name)
	}
	if strings.EqualFold(name, escalationName) {
		return fmt.Sprintf("%s cannot remove %q — it is the active escalation agent (switch first with /agent switch)", milkTag(), name)
	}
	idx := -1
	for i, a := range st.cfg.Agents {
		if strings.EqualFold(a.Name, name) {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Sprintf("%s no agent named %q", milkTag(), name)
	}
	removed := st.cfg.Agents[idx].Name
	st.cfg.Agents = append(st.cfg.Agents[:idx], st.cfg.Agents[idx+1:]...)
	if err := config.Save(st.cfg); err != nil {
		return fmt.Sprintf("%s error saving config: %v", milkTag(), err)
	}
	return fmt.Sprintf("%s agent %q removed", milkTag(), removed)
}

// handleConfigCmd dispatches /config, /config init, /config open.
func (m model) handleConfigCmd(sub string) (tea.Model, tea.Cmd) {
	switch strings.ToLower(sub) {
	case "init":
		return m.handleConfigInitCmd()
	case "open":
		return m.handleConfigOpenCmd()
	case "":
		dir, err := config.Dir()
		if err != nil {
			m.appendTranscript(fmt.Sprintf("%s error: %v\n", milkTag(), err))
			return m, nil
		}
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			m.appendTranscript(fmt.Sprintf("%s error reading config: %v\n", milkTag(), err))
			return m, nil
		}
		m.colorizeForce = true
		m.appendTranscript(milkTag() + " ~/.milk/config.json\n```json\n" + string(data) + "\n```\n")
		return m, nil
	default:
		m.appendTranscript(milkTag() + " usage: /config | /config init | /config open\n")
		return m, nil
	}
}

// handleConfigOpenCmd opens ~/.milk/config.json in $EDITOR or xdg-open.
// Uses tea.ExecProcess so the TUI is properly suspended while the editor runs.
func (m model) handleConfigOpenCmd() (tea.Model, tea.Cmd) {
	dir, err := config.Dir()
	if err != nil {
		m.appendTranscript(fmt.Sprintf("%s error: %v\n", milkTag(), err))
		return m, nil
	}
	cfgPath := filepath.Join(dir, "config.json")
	cmd := m.openInEditor(cfgPath)
	if cmd == nil {
		m.appendTranscript(fmt.Sprintf("%s no editor found — set $EDITOR or configure config_editors in config\n", milkTag()))
		return m, nil
	}
	m.appendTranscript(fmt.Sprintf("%s opening %s…\n", milkTag(), cfgPath))
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			return errMsg{err: fmt.Errorf("editor exited with error: %w", err)}
		}
		return configReloadMsg{}
	})
}

// resolveEditorCmd returns the editor executable and any extra args from the
// config_editors list (or built-in defaults). Returns ("", nil) when nothing is found.
func (m model) resolveEditorCmd() (string, []string) {
	defaultEditors := []string{"$EDITOR", "$VISUAL", "nano", "vim", "vi"}
	list := m.st.cfg.ConfigEditors
	if len(list) == 0 {
		list = defaultEditors
	}
	var candidates []string
	for _, e := range list {
		expanded := os.ExpandEnv(e)
		if expanded != "" {
			candidates = append(candidates, expanded)
		}
	}
	for _, c := range candidates {
		parts := strings.Fields(c)
		if len(parts) == 0 {
			continue
		}
		if _, err := exec.LookPath(parts[0]); err == nil {
			return parts[0], parts[1:]
		}
	}
	return "", nil
}

// openInEditor builds an exec.Cmd for opening path in the resolved editor.
// Returns nil when no editor is found.
func (m model) openInEditor(path string) *exec.Cmd {
	editorCmd, editorArgs := m.resolveEditorCmd()
	if editorCmd == "" {
		return nil
	}
	return exec.Command(editorCmd, append(editorArgs, path)...)
}

// handleOpenCmd handles /open <path>: resolves the path and opens it in the editor.
func (m model) handleOpenCmd(path string) (tea.Model, tea.Cmd) {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "@") // support /open @cmd/milk/repl.go notation
	if path == "" {
		m.appendTranscript(milkTag() + " usage: /open <file>  or  /open @<file>\n")
		return m, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.st.cwd, path)
	}
	return m, func() tea.Msg { return openFileMsg{path: path} }
}

// handleOpenFileMsg opens path in the editor via tea.ExecProcess.
// If msg.respCh is non-nil (agent tool call), the result is sent back to unblock the goroutine.
func (m model) handleOpenFileMsg(msg openFileMsg) (tea.Model, tea.Cmd) {
	cmd := m.openInEditor(msg.path)
	if cmd == nil {
		errOut := fmt.Errorf("no editor found — set $EDITOR or configure config_editors in config")
		m.appendTranscript(fmt.Sprintf("%s %v\n", milkTag(), errOut))
		if msg.respCh != nil {
			msg.respCh <- errOut
		}
		return m, nil
	}
	m.appendTranscript(fmt.Sprintf("%s opening %s…\n", milkTag(), msg.path))
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		if msg.respCh != nil {
			msg.respCh <- err
		}
		if err != nil {
			return errMsg{err: fmt.Errorf("editor exited with error: %w", err)}
		}
		return nil
	})
}

// handleUpdateCmd handles /update [check|install|skip].
func (m model) handleUpdateCmd(sub string) (tea.Model, tea.Cmd) {
	switch sub {
	case "check", "":
		if m.pendingUpdate != nil {
			m.appendTranscript(fmt.Sprintf("%s update available: %s — use /update install\n", milkTag(), m.pendingUpdate.Tag))
			return m, nil
		}
		m.appendTranscript(milkTag() + " checking for updates…\n")
		cfg := m.st.cfg
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			rel, err := updater.CheckLatest(ctx, version, cfg.UpdateCheckIncludePrerelease())
			if err != nil {
				return errMsg{err: fmt.Errorf("update check: %w", err)}
			}
			if rel == nil {
				return errMsg{err: fmt.Errorf("already up to date (%s)", version)}
			}
			return updateAvailableMsg{release: rel}
		}

	case "install":
		if m.updateInstalling {
			m.appendTranscript(milkTag() + " update already in progress\n")
			return m, nil
		}
		if m.pendingUpdate == nil {
			m.appendTranscript(milkTag() + " no update available — run /update check first\n")
			return m, nil
		}
		rel := m.pendingUpdate
		m.updateInstalling = true
		m.updateProgress = 0
		m.updateTotal = 0
		m.appendTranscript(fmt.Sprintf("%s installing %s…\n", milkTag(), rel.Tag))
		send := func(msg tea.Msg) { m.st.program.Send(msg) }
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			dest, err := updater.CurrentBinaryPath()
			if err != nil {
				return updateDoneMsg{err: err}
			}
			err = updater.Apply(ctx, rel, dest, func(done, total int64) {
				send(updateProgressMsg{done: done, total: total})
			})
			return updateDoneMsg{err: err}
		}

	case "skip":
		if m.pendingUpdate == nil {
			m.appendTranscript(milkTag() + " no pending update\n")
			return m, nil
		}
		cfg := m.st.cfg
		cfg.UpdateSkippedVersion = m.pendingUpdate.Tag
		_ = config.Save(cfg)
		m.st.cfg = cfg
		m.pendingUpdate = nil
		m.appendTranscript(milkTag() + " update skipped\n")
		return m, nil

	default:
		m.appendTranscript(milkTag() + " usage: /update [check|install|skip]\n")
		return m, nil
	}
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
	case "workflow":
		m.workflowPanelOpen = !m.workflowPanelOpen
		m.syncLayout()
		if m.workflowPanelOpen {
			m.appendTranscript(milkTag() + " workflow panel: on\n")
		} else {
			m.appendTranscript(milkTag() + " workflow panel: off\n")
		}
		return m, nil
	default:
		m.appendTranscript(milkTag() + " usage: /panel memory|workflow\n")
		return m, nil
	}
}

// handleServerCmd handles /server [status|start for <agent>|stop [<agent>]].
func (m model) handleServerCmd(arg string) (tea.Model, tea.Cmd) {
	// Parse verb and optional agent name.
	// Supported forms:
	//   /server status [<agent>]
	//   /server start for <agent>
	//   /server stop [<agent>]
	var verb, agentArg string
	if strings.HasPrefix(arg, "start for ") {
		verb = "start"
		agentArg = strings.TrimSpace(strings.TrimPrefix(arg, "start for "))
	} else {
		parts := strings.SplitN(arg, " ", 2)
		verb = parts[0]
		if len(parts) == 2 {
			agentArg = strings.TrimSpace(parts[1])
		}
	}

	// Resolve agent config.
	resolveAC := func() (config.AgentConfig, bool) {
		cfg := m.st.cfg
		if agentArg == "" {
			return activeLocalAgentConfig(cfg), true
		}
		for _, a := range cfg.Agents {
			if strings.EqualFold(a.Name, agentArg) {
				return a, true
			}
		}
		m.appendTranscript(fmt.Sprintf("%s no agent named %q in config\n", milkTag(), agentArg))
		return config.AgentConfig{}, false
	}

	switch verb {
	case "status", "":
		ac, ok := resolveAC()
		if !ok {
			return m, nil
		}
		m.appendTranscript(fmt.Sprintf("%s %s: %s\n", milkTag(), ac.Name, serverStatus(ac.Name, ac.URL)))
		return m, nil

	case "start":
		ac, ok := resolveAC()
		if !ok {
			return m, nil
		}
		if ac.RunCmd == "" {
			m.appendTranscript(fmt.Sprintf("%s agent %q has no run_cmd configured\n", milkTag(), ac.Name))
			return m, nil
		}
		if isReachable(ac.URL) {
			m.appendTranscript(fmt.Sprintf("%s server for %q is already reachable at %s\n", milkTag(), ac.Name, ac.URL))
			return m, nil
		}
		m.appendTranscript(fmt.Sprintf("%s starting server for %q…\n", milkTag(), ac.Name))
		agentName, url, runCmd := ac.Name, ac.URL, ac.RunCmd
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
			defer cancel()
			if err := ensureServerRunning(ctx, url, runCmd, agentName); err != nil {
				return serverStartDoneMsg{agentName: agentName, url: url, err: err}
			}
			pid, _ := readPID(agentName)
			return serverStartDoneMsg{agentName: agentName, url: url, pid: pid}
		}

	case "stop":
		ac, ok := resolveAC()
		if !ok {
			return m, nil
		}
		agentName := ac.Name
		return m, func() tea.Msg {
			stopped, err := serverStop(agentName)
			return serverStopDoneMsg{agentName: agentName, stopped: stopped, err: err}
		}

	default:
		m.appendTranscript(milkTag() + " usage: /server status [<agent>] | /server start for <agent> | /server stop [<agent>]\n")
		return m, nil
	}
}
