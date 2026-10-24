package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/mcpauth"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/tasks"
)

// handleInterruptCmd runs /interrupt: cancel the running turn — exactly
// what Ctrl+C does in handleBusyKey (cancelTurn + the [interrupted]
// completion note) — or report that nothing is running. It exists for
// surfaces where Ctrl+C isn't available (Telegram remote input, ACP
// prompts) but is a first-class TUI command too; busyEnter executes it via
// busySafeCommands, remote input routes it around the busy queue.
func (m model) handleInterruptCmd() (tea.Model, tea.Cmd) {
	if m.cancelTurn != nil {
		m.cancelTurn()
		m.cancelTurn = nil
		m.interrupted = true
		return m, nil
	}
	m.appendTranscript(milkTag() + " nothing to interrupt — no agent turn running\n")
	return m, nil
}

func (m model) handleSlashInput(cmd, rest string) (tea.Model, tea.Cmd) {
	oldSessionID := ""
	if m.st != nil && m.st.sess != nil {
		oldSessionID = m.st.sess.ID
	}
	if m.workflowRunning {
		switch cmd {
		case cmdNew, cmdClear, cmdDrop, cmdResume:
			// The workflow's checkpoint and completion notice belong to this session.
			m.appendTranscript(milkTag() + " a workflow is running in this session — /workflow cancel it before " + cmd + "\n")
			return m, nil
		}
	}
	switch cmd {
	case cmdNew, cmdClear, cmdDrop, cmdResume:
		// Session-switching commands orphan staged attachments (/attach,
		// clipboard paste): the turn they would ride belongs to a different
		// conversation. Drop them with a notice instead of leaking them
		// across the switch.
		if len(m.pendingAttachments) > 0 {
			m.appendTranscript(milkTag() + fmt.Sprintf(" dropped %d staged attachment(s) — staged files do not survive %s\n", len(m.pendingAttachments), cmd))
			m.pendingAttachments = nil
		}
	}
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
		return m.handleThinkCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdInterrupt {
		return m.handleInterruptCmd()
	}
	if cmd == cmdTheme {
		return m.handleThemeCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdTips {
		return m.handleTipsCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdKeys {
		m.appendTranscript("\n" + renderKeysHelp(m.vpWidth()) + "\n")
		return m, nil
	}
	if cmd == cmdNotifications {
		return m.handleNotificationsCmd(strings.TrimSpace(rest)), nil
	}
	if cmd == cmdPermissions {
		return m.handlePermissionsCmd(strings.TrimSpace(rest)), nil
	}
	if cmd == cmdSetup {
		return m.handleSetupCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdConfig {
		return m.handleConfigCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdInit {
		// /init is the alias for /config init.
		return m.handleConfigInitCmd()
	}
	if cmd == cmdOpen {
		return m.handleOpenCmd(rest)
	}
	if cmd == cmdUpdate {
		return m.handleUpdateCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdAttach {
		return m.handleAttachCmd(strings.TrimSpace(rest)), nil
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
	if cmd == cmdReload {
		return m.handleReloadCmd()
	}
	if cmd == cmdTasks {
		return m.handleTasksCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdTask {
		return m.handleTaskCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdBg {
		return m.handleBgCmd(strings.TrimSpace(rest))
	}
	if cmd == cmdRestart {
		return m.handleRestartCmd(strings.TrimSpace(rest))
	}
	if cmd == "/help" {
		output := renderHelp(interactiveHelp, m.vpWidth())
		m.colorizeForce = true
		m.appendTranscript(output + "\n")
		return m, nil
	}
	exit, dispatch, output := handleSlashCommand(cmd, rest, m.st)
	if err := m.refreshSessionScopedState(oldSessionID); err != nil {
		if output != "" {
			output += "\n"
		}
		output += fmt.Sprintf("%s warning: session switch state refresh failed: %v", milkTag(), err)
	}
	// A binding change can switch directories (/resume <id> into another
	// project): project-local config must follow the *currently selected*
	// dir — the newly bound session's cwd — and the agents must match it.
	// Re-merge on every swap; rebuild the runners only when agent-relevant
	// fields actually changed (same rule as configReloadMsg).
	var rebuildCmd tea.Cmd
	if m.st != nil && m.st.sess != nil && oldSessionID != "" && m.st.sess.ID != oldSessionID {
		if fresh, ferr := config.LoadMergedIn(activeConfigDir(m.st)); ferr == nil {
			agentChanged := agentConfigChanged(m.st.cfg, fresh)
			m.st.cfg = fresh
			if agentChanged {
				m, rebuildCmd = m.rebuildRunners()
				if output != "" {
					output += "\n"
				}
				output += milkTag() + " config re-merged from the selected dir — agents rebuilt, no restart needed"
			}
		}
	}
	// /resume switches conversations, not just state: reseed the transcript
	// from the newly bound session so the view matches the binding. /new,
	// /clear and /drop land on fresh sessions and leave the visible
	// transcript as it was.
	if cmd == cmdResume && m.st != nil && m.st.sess != nil && oldSessionID != "" && m.st.sess.ID != oldSessionID {
		m.reseedTranscriptFromHistory()
	}
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
		dm, dcmd := m.dispatchAgent(dispatch)
		return dm, tea.Batch(rebuildCmd, dcmd)
	}
	return m, rebuildCmd
}

func (m *model) refreshSessionScopedState(oldSessionID string) error {
	if m.st == nil || m.st.sess == nil || oldSessionID == "" || m.st.sess.ID == oldSessionID {
		return nil
	}
	if sp, err := sessionHistoryPath(oldSessionID); err == nil {
		writeHistoryFile(sp, m.sessionHistory)
	}
	if sp, err := sessionHistoryPath(m.st.sess.ID); err == nil {
		m.sessionHistory = readHistoryFile(sp)
	} else {
		m.sessionHistory = nil
	}
	m.histIdx = -1
	m.saved = ""
	m.useGlobalHistory = false

	if dir, err := memoryDir(); err == nil {
		if mem, memErr := memory.NewStoreParams(dir, m.st.sess.ID, memParams(m.st.cfg)); memErr == nil {
			m.mem = mem
			m.st.mem = mem
		} else {
			return memErr
		}
	}

	if dir, err := config.Dir(); err == nil {
		// Re-point background-job persistence at the new session's jobs file
		// (ADR-0043 triage). SetStateFile is otherwise only called with the
		// startup session's ID (repl.go's Manager construction; acp_features.go
		// does its own per-session equivalent), so without this every job
		// spawned after /new, /clear or /drop would keep persisting under the
		// *previous* session's file and the new session's triage record would
		// never appear. Jobs spawned before the swap keep their own file — see
		// internal/agent/local's persistLocked per-job attribution.
		if m.agents.backgroundMgr != nil && m.st.sess.ID != "" {
			m.agents.backgroundMgr.SetStateFile(filepath.Join(dir, "jobs", m.st.sess.ID+".json"))
		}
		taskStore, taskErr := tasks.New(filepath.Join(dir, "tasks"), m.st.sess.ID)
		if taskErr != nil {
			return taskErr
		}
		if m.st.program != nil {
			taskStore.SetOnChange(func() { m.st.program.Send(taskStoreChangedMsg{}) })
		}
		m.taskStore = taskStore
		adapter := tasks.NewAdapter(taskStore)
		if m.agents.local != nil {
			m.agents.local = m.agents.local.WithTaskStore(adapter)
		}
		if m.agents.escalationLocal != nil {
			m.agents.escalationLocal = m.agents.escalationLocal.WithTaskStore(adapter)
		}
	}

	m.workflowResumeInit = nil
	m.panelOffset = 0
	m.tasksOffset = 0
	m.refreshPrompt()
	m.syncLayout()
	return nil
}

// handleTasksCmd handles `/tasks` — lists current session + global tasks in the transcript.
func (m model) handleTasksCmd(arg string) (tea.Model, tea.Cmd) {
	m.appendTranscript(execTasks(m.taskStore))
	return m, nil
}

// execTasks renders the session and global task lists (trailing newline included).
func execTasks(store *tasks.Store) string {
	if store == nil {
		return milkTag() + " task store not available\n"
	}
	sessionTasks, _ := store.List(tasks.ListOpts{})
	globalTasks, _ := store.List(tasks.ListOpts{IncludeGlobal: true})

	// Build session ID set to find global-only tasks.
	sessIDs := map[string]bool{}
	for _, t := range sessionTasks {
		sessIDs[t.ID] = true
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s tasks:\n", milkTag())
	if len(sessionTasks) == 0 {
		fmt.Fprintf(&sb, "  session: (none)\n")
	} else {
		fmt.Fprintf(&sb, "  session:\n")
		for _, t := range sessionTasks {
			fmt.Fprintf(&sb, "    %s  %-10s  %s\n", t.ID, t.Status, t.Title)
		}
	}
	var globalOnly []tasks.Task
	for _, t := range globalTasks {
		if !sessIDs[t.ID] {
			globalOnly = append(globalOnly, t)
		}
	}
	if len(globalOnly) > 0 {
		fmt.Fprintf(&sb, "  global:\n")
		for _, t := range globalOnly {
			fmt.Fprintf(&sb, "    %s  %-10s  %s\n", t.ID, t.Status, t.Title)
		}
	}
	return sb.String()
}

// handleTaskCmd handles `/task done <id>` shortcut.
func (m model) handleTaskCmd(arg string) (tea.Model, tea.Cmd) {
	m.appendTranscript(execTask(arg, m.taskStore) + "\n")
	return m, nil
}

// execTask runs `/task done <id>` and returns the message to show (no trailing newline).
func execTask(arg string, store *tasks.Store) string {
	const donePrefix = "done "
	const usage = " usage: /task done <id>"
	if !strings.HasPrefix(arg, donePrefix) {
		return milkTag() + usage
	}
	id := strings.TrimSpace(strings.TrimPrefix(arg, donePrefix))
	if id == "" {
		return milkTag() + usage
	}
	if store == nil {
		return milkTag() + " task store not available"
	}
	if err := store.Complete(id); err != nil {
		return fmt.Sprintf("%s task %q: %v", milkTag(), id, err)
	}
	return fmt.Sprintf("%s task %s marked done", milkTag(), id)
}

// handleBgCmd handles /bg — list, start, and stop background agents.
func (m model) handleBgCmd(arg string) (tea.Model, tea.Cmd) {
	mgr := m.agents.backgroundMgr
	if mgr == nil {
		m.appendTranscript(milkTag() + " background agents unavailable — no inference-server-backed agent configured\n")
		return m, nil
	}
	parts := strings.Fields(arg)
	switch {
	case len(parts) == 0 || parts[0] == "list":
		return m.handleBgList(mgr), nil
	case parts[0] == "start":
		spec, task, err := parseBgStartArgs(strings.TrimPrefix(arg, "start"))
		if err != nil || task == "" {
			m.appendTranscript(milkTag() + " usage: /bg start [--agent <name|primary|escalation>] <task>\n")
			return m, nil
		}
		return m.startUserBackgroundAgent(task, spec)
	case parts[0] == "stop":
		id := strings.TrimSpace(strings.TrimPrefix(arg, "stop"))
		if id == "" {
			m.appendTranscript(milkTag() + " usage: /bg stop <id>\n")
			return m, nil
		}
		if mgr.Cancel(id) {
			m.appendTranscript(fmt.Sprintf("%s background agent %s — cancelling\n", milkTag(), id))
		} else if _, ok := local.KillBackgroundShell(id); ok {
			// Not a job — one of the background-shell rows (ADR-0064):
			// /bg stop doubles as the UI's kill_bash.
			m.appendTranscript(fmt.Sprintf("%s background shell %s — killing\n", milkTag(), id))
		} else {
			m.appendTranscript(fmt.Sprintf("%s background agent %s not found or already finished\n", milkTag(), id))
		}
		return m, nil
	case parts[0] == "show":
		id := strings.TrimSpace(strings.TrimPrefix(arg, "show"))
		if id == "" {
			m.appendTranscript(milkTag() + " usage: /bg show <id>\n")
			return m, nil
		}
		for _, j := range mgr.Jobs() {
			if j.ID == id {
				m.appendTranscript(renderBgShow(j))
				return m, nil
			}
		}
		if info, ok := local.BackgroundShell(id); ok {
			m.appendTranscript(renderBgShowShell(info))
			return m, nil
		}
		m.appendTranscript(fmt.Sprintf("%s background agent %s not found\n", milkTag(), id))
		return m, nil
	default:
		m.appendTranscript(milkTag() + " usage: /bg [list | show <id> | start [--agent <name>] <task> | stop <id>] (ids: background agents and shells)\n")
		return m, nil
	}
}

// handleBgList renders the background-agents + background-shells table into
// the transcript.
func (m model) handleBgList(mgr *local.Manager) model {
	m.appendTranscript(renderBgList(mgr.Jobs(), local.BackgroundShells()))
	return m
}

// renderBgList formats the background-agents + background-shells table
// (trailing newline included) — one merged /bg surface for both kinds of
// background work (ADR-0064).
func renderBgList(jobs []local.Job, shells []local.ShellInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s background agents:\n", milkTag())
	if len(jobs) == 0 {
		fmt.Fprintf(&sb, "  (none)\n")
	} else {
		for _, j := range jobs {
			elapsed := ""
			if !j.EndedAt.IsZero() {
				elapsed = j.EndedAt.Sub(j.StartedAt).Round(time.Second).String()
			} else {
				elapsed = time.Since(j.StartedAt).Round(time.Second).String() + "…"
			}
			status := string(j.Status)
			if j.TimedOut {
				status = "timed-out"
			}
			fmt.Fprintf(&sb, "  %-8s  %-10s  %8s  %s\n", j.ID, status, elapsed, j.Label)
		}
	}
	if len(shells) > 0 {
		fmt.Fprintf(&sb, "%s background shells:\n", milkTag())
		for _, s := range shells {
			elapsed := elapsedShort(s.StartedAt, s.FinishedAt)
			if s.Running {
				elapsed += "…"
			}
			fmt.Fprintf(&sb, "  %-8s  %-10s  %8s  %s\n", s.ID, s.StatusLabel(), elapsed, s.Command)
		}
	}
	return sb.String()
}

// handleRestartCmd runs /restart (issue #258): relaunch milk with the very
// same starting command, pinning the current session. Refused while
// anything is running — the process is about to be replaced. On success the
// TUI quits with m.restartPlan armed; runREPL performs the actual exec after
// the terminal is restored.
func (m model) handleRestartCmd(rest string) (tea.Model, tea.Cmd) {
	if rest != "" {
		m.appendTranscript(milkTag() + " usage: /restart\n")
		return m, nil
	}
	if reason := m.restartBusyReason(); reason != "" {
		m.appendTranscript(fmt.Sprintf("%s /restart refused — %s. Finish or cancel it first (Ctrl+C, /bg stop), then /restart.\n", milkTag(), reason))
		return m, nil
	}
	sessID := ""
	if m.st != nil && m.st.sess != nil {
		sessID = m.st.sess.ID
	}
	plan := capturedLaunch.planRestart(sessID)
	m.restartPlan = &plan
	m.appendTranscript(fmt.Sprintf("%s restarting: %s\n", milkTag(), plan.Why))
	return m, tea.Quit
}

// restartBusyReason reports the in-flight work that blocks /restart ("" =
// idle): a turn, a PTY shell pane, a workflow, background agents or
// background shells — anything whose state would be lost or orphaned by the
// process swap.
func (m model) restartBusyReason() string {
	if m.busy {
		return "a turn is running"
	}
	if m.ptyPane != nil {
		return "a shell pane is running"
	}
	if m.workflowRunning {
		return "a workflow is running"
	}
	if st := m.workflowState; st != nil && st.WorkflowName != "" && st.Role != "done" {
		return "a workflow is active"
	}
	if m.agents.backgroundMgr != nil {
		if n := m.agents.backgroundMgr.ActiveCount(); n > 0 {
			return fmt.Sprintf("%d background agent(s) running", n)
		}
	}
	for _, s := range local.BackgroundShells() {
		if s.Running {
			return fmt.Sprintf("background shell %s (%s) is running", s.ID, s.Command)
		}
	}
	return ""
}

// handleReloadCmd re-parses config.json immediately and sends a configReloadMsg.
// This is the manual equivalent of the file-system watcher callback — useful
// when the watcher misses a write (e.g. symlink swap, editor atomic replace).
// Re-merges against the currently selected directory (activeConfigDir), so a
// project-local overlay survives /reload instead of being dropped — and a
// /resume'd session under another dir reloads *that* dir's overlay.
func (m model) handleReloadCmd() (tea.Model, tea.Cmd) {
	dir := activeConfigDir(m.st)
	return m, func() tea.Msg {
		newCfg, err := config.LoadMergedIn(dir)
		return configReloadMsg{cfg: newCfg, err: err}
	}
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
			// Mode change invalidates per-turn colorization cache.
			m.turnColorCache = nil
			m.turnRawCache = nil
		}
	}
	m.appendTranscript(output + "\n")
	return m
}

// handleThinkCmd handles `/think [expand|collapse|view]` — the fold surface
// of the think-block model (ADR-0067). There is no visibility toggle any
// more: every block always renders its header row, folded by default —
// streaming blocks included (the header's line count updates live) — so
// /think on|off are gone. The bare query answers in
// the transcript like every other explicit query answer; state *changes* are
// turn-unrelated informational events → notification toasts (issue #162).
func (m model) handleThinkCmd(arg string) (model, tea.Cmd) {
	switch arg {
	case "expand":
		m.setAllThinkBlocks(true)
		m.notify("reasoning: all blocks expanded", "/think collapse")
	case "collapse":
		m.setAllThinkBlocks(false)
		m.notify("reasoning: all blocks folded", "/think expand")
	case "view":
		return m.startThinkView()
	case "":
		expanded := 0
		for i := range m.thinkBlocks {
			if m.thinkBlocks[i].expanded {
				expanded++
			}
		}
		m.appendTranscript(fmt.Sprintf("%s reasoning: %d thinking block(s) · %d expanded, %d folded  (use /think expand|collapse|view)\n",
			milkTag(), len(m.thinkBlocks), expanded, len(m.thinkBlocks)-expanded))
	case "on", "off":
		m.appendTranscript(milkTag() + " /think on|off are gone — reasoning always renders as foldable blocks, folded by default (the line count updates live while streaming; Ctrl+T or a ▸ header click opens one). Use /think expand|collapse|view.\n")
	default:
		m.appendTranscript(milkTag() + " usage: /think [expand|collapse|view]\n")
	}
	return m, nil
}

// handleTipsCmd handles `/tips …` — the idle tip line below the input
// (ADR-0061): visibility (on/off, persisted like /colorize) and tip
// browsing (next, prev, pick, search). With no arg: shows the current
// visibility and where in the rotation the shown tip sits. The toggle
// reflows the layout by one row via syncLayout, since hintRegion reserves
// the row while enabled.
//
// State *changes* are turn-unrelated informational events → notification
// toasts (issue #162); query and browse output stays in the transcript
// like every other explicit query answer.
func (m model) handleTipsCmd(arg string) (tea.Model, tea.Cmd) {
	switch arg {
	case "on", "off":
		want := arg == "on"
		if m.showTips == want {
			other := "on"
			if want {
				other = "off"
			}
			m.notify("tip line: already "+arg, "/tips "+other)
			return m, nil
		}
		m.showTips = want
		m.st.cfg.ShowTips = &want
		other := "on"
		if want {
			other = "off"
		}
		if err := saveLocalOrGlobal(m.st.cfg, activeConfigDir(m.st)); err != nil {
			m.appendTranscript(fmt.Sprintf("%s tip line: %s (config save failed: %v)\n", milkTag(), bold(arg), err))
		} else {
			m.notify("tip line: "+arg, "/tips "+other)
		}
	case "":
		state := "off"
		if m.showTips {
			state = "on"
		}
		where := ""
		if pos, total := m.tipPos(); total > 0 {
			where = fmt.Sprintf(" (tip %d/%d)", pos, total)
		}
		m.appendTranscript(fmt.Sprintf("%s tip line: %s%s  (use /tips on|off — browse: /tips next|prev|pick|search <query>)\n", milkTag(), bold(state), where))
		return m, nil
	default:
		mm, cmd := m.handleTipsBrowse(arg)
		return mm, cmd
	}
	m.syncLayout()
	return m, nil
}

// handleTipsBrowse runs the tip-browsing half of /tips: next/prev step the
// rotation (the same tipSeek the Alt+Up/Down shortcuts use), pick lists
// every tip or jumps to one (by number, or by text when it matches exactly
// one tip), search lists the tips matching a query. A pick always echoes
// the chosen tip to the transcript — deliberate selections are meant to be
// read at full width — while next/prev only echo when the tip line is
// hidden (tipBrowseEcho). The returned command restarts the cycling timer
// for a successful move (and is nil otherwise): a browse resets the
// rotation countdown instead of being cut short by the old schedule.
func (m model) handleTipsBrowse(arg string) (model, tea.Cmd) {
	list := buildTipList()
	var tick tea.Cmd
	usage := func() {
		m.appendTranscript(milkTag() + " usage: /tips [on|off|next|prev|pick [<n>|<text>]|search <query>]\n")
	}
	switch {
	case arg == "next" || arg == "prev":
		if len(list) == 0 {
			usage()
			return m, nil
		}
		if arg == "next" {
			tick = m.tipSeek(1)
		} else {
			tick = m.tipSeek(-1)
		}
		m.tipBrowseEcho()
	case arg == "pick":
		if len(list) == 0 {
			usage()
			return m, nil
		}
		pos, total := m.tipPos()
		m.appendTranscript(fmt.Sprintf("%s %d tips — * = showing now (tip %d/%d); /tips pick <n> shows one\n", milkTag(), total, pos, total))
		for i, tip := range list {
			m.appendTranscript(tipListEntry(i, tip, i == pos-1))
		}
	case strings.HasPrefix(arg, "search "), strings.HasPrefix(arg, "pick "):
		verb, q := arg[:strings.IndexByte(arg, ' ')], strings.TrimSpace(arg[strings.IndexByte(arg, ' '):])
		if q == "" {
			usage()
			return m, nil
		}
		if verb == "pick" {
			if n, err := strconv.Atoi(q); err == nil {
				ok, cmd := m.tipPick(n)
				if !ok {
					_, total := m.tipPos()
					m.appendTranscript(fmt.Sprintf("%s no tip %d — tip numbers run 1–%d (/tips pick lists them)\n", milkTag(), n, total))
					return m, nil
				}
				m.tipEcho()
				return m, cmd
			}
		}
		hits := searchTips(q)
		if len(hits) == 0 {
			m.appendTranscript(fmt.Sprintf("%s no tips match %q — /tips pick lists all %d\n", milkTag(), q, len(list)))
			return m, nil
		}
		if verb == "pick" && len(hits) == 1 {
			_, cmd := m.tipPick(hits[0] + 1)
			m.tipEcho()
			return m, cmd
		}
		m.appendTranscript(tipMatchHeader(len(hits), q))
		pos, _ := m.tipPos()
		for _, i := range hits {
			m.appendTranscript(tipListEntry(i, list[i], i == pos-1))
		}
	default:
		usage()
	}
	return m, tick
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
	case "telegram status":
		m.appendTranscript(telegramStatusText(m.st.cfg) + "\n")
		return m, nil
	default:
		m.appendTranscript(milkTag() + " usage: /setup telegram | /setup telegram on | /setup telegram off | /setup telegram status\n")
		return m, nil
	}
}

// handleTelegramSetupKey handles keypresses during the /setup telegram wizard.
func (m model) handleTelegramSetupKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	if err := setTelegramEnabledCfg(&m.st.cfg, on, activeConfigDir(m.st)); err != nil {
		m.appendTranscript(milkTag() + " " + err.Error() + "\n")
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

	if err := saveLocalOrGlobal(m.st.cfg, activeConfigDir(m.st)); err != nil {
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
	notifyAnnouncement(m.st.notifier, "milk", "setup", "Telegram oversight configured successfully ✓")
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

	if verb == "auth" {
		return m.handleMCPAuth(rest)
	}

	m.appendTranscript(execMCP(arg, m.st, m.agents.mcpToolSets) + "\n")
	switch verb {
	case "remove", "enable", "disable", "assign", "unassign":
		m = m.refreshMCPToolSets()
	}
	return m, nil
}

// handlePathPasteKey handles keypresses while pendingPathPaste is set.
// "y"/"Y"/Enter → attach the file; "n"/"N"/Esc/Ctrl+C → insert as plain text.
func (m model) handlePathPasteKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	path := m.pendingPathPaste
	m.pendingPathPaste = ""

	switch strings.ToLower(msg.String()) {
	case "y", "enter":
		m.appendTranscript("Y\n")
		return m.handleAttachCmd(path), nil
	default:
		// "n", esc, ctrl+c, any other key → insert as @path reference.
		m.appendTranscript("n\n")
		m.ta.InsertString("@" + path)
		m.syncLayout()
		return m, nil
	}
}

// handleAttachCmd implements `/attach <path>`.
// It loads the file, detects the MIME type, and stages it in pendingAttachments.
// The next submitted turn will include the file as a context block (text) or a
// multipart vision payload (image). Returns the updated model without a tea.Cmd.
func (m model) handleAttachCmd(path string) model {
	return m.handleAttachFile(path, "")
}

// handleAttachFile is handleAttachCmd with the MIME type already known
// (clipboard probes know exactly what they read — the temp file's name tells
// detection nothing). Empty knownMIME falls back to detection.
func (m model) handleAttachFile(path, knownMIME string) model {
	if path == "" {
		m.appendTranscript(milkTag() + " usage: /attach <path>\n")
		return m
	}
	a, err := loadAttachmentMIME(path, knownMIME)
	if err != nil {
		m.appendTranscript(fmt.Sprintf("%s error: %v\n", milkTag(), err))
		return m
	}
	m.pendingAttachments = append(m.pendingAttachments, a)
	kind := "text"
	if a.isImage() {
		kind = "image"
	}
	m.appendTranscript(fmt.Sprintf("%s staged %s %q (%s, %d bytes) — %d attachment(s) pending\n",
		milkTag(), kind, a.Name, a.MIMEType, len(a.Data), len(m.pendingAttachments)))
	if a.Note != "" {
		m.appendTranscript(milkTag() + " " + a.Note + "\n")
	}
	return m
}

// validateMCPAuthArgs checks the arguments for /mcp auth and returns an error
// message string (non-empty = invalid) or the resolved server config index.
// Separated from handleMCPAuth so it can be unit-tested without a live model.
func validateMCPAuthArgs(serverName string, cfg config.Config) (idx int, errMsg string) {
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return -1, "usage: /mcp auth <server-name>"
	}
	idx = findMCPServerIdx(cfg.MCPServers, serverName)
	if idx < 0 {
		return -1, fmt.Sprintf("MCP server %q not found — add it with /mcp add first", serverName)
	}
	return idx, ""
}

// handleMCPAuth implements `/mcp auth <server-name>`.
// It runs milk's native OAuth flow (RFC 9728/8414 discovery, RFC 7591 dynamic
// client registration, PKCE Authorization Code) in the background: the TUI
// stays interactive while a local loopback listener waits for the browser
// redirect. The authorization URL is printed to the transcript (and a
// best-effort browser-open is attempted) as soon as it's known; the status
// bar shows "MCP OAuth: <server>" while waiting, then ok/failed on completion.
func (m model) handleMCPAuth(serverName string) (model, tea.Cmd) {
	idx, errStr := validateMCPAuthArgs(serverName, m.st.cfg)
	if errStr != "" {
		m.appendTranscript(milkTag() + " " + errStr + "\n")
		return m, nil
	}
	serverName = strings.TrimSpace(serverName)
	server := m.st.cfg.MCPServers[idx]

	m.credRefreshing = true
	m.credLabel = "MCP OAuth: " + serverName

	prog := m.st.program
	ctx := m.ctx
	go func() {
		opts := mcpauth.Options{
			OnAuthURL: func(authURL string) {
				prog.Send(mcpOAuthStartedMsg{serverName: serverName, authURL: authURL})
			},
		}
		err := mcpauth.Authorize(ctx, server, opts)
		prog.Send(credRefreshReadyMsg{label: "MCP OAuth: " + serverName, err: err})
	}()

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
		return m.startAgentTool(sub)

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

// agentRemoveOutcome describes the result of removeAgentConfig.
type agentRemoveOutcome int

const (
	agentRemoveOK agentRemoveOutcome = iota
	agentRemoveIsPrimary
	agentRemoveIsEscalation
	agentRemoveNotFound
)

// removeAgentConfig removes the named agent from cfg.Agents, refusing when it
// is the active primary or escalation agent. Shared by /agent remove and the
// headless "milk config agent remove" CLI subcommand. removed is the
// agent's stored name (for message formatting); empty unless outcome is OK.
func removeAgentConfig(cfg *config.Config, name string) (outcome agentRemoveOutcome, removed string) {
	primaryName := cfg.ActiveAgent().Name
	escalationName := cfg.EscalationAgentConfig().Name
	if strings.EqualFold(name, primaryName) {
		return agentRemoveIsPrimary, ""
	}
	if strings.EqualFold(name, escalationName) {
		return agentRemoveIsEscalation, ""
	}
	idx := -1
	for i, a := range cfg.Agents {
		if strings.EqualFold(a.Name, name) {
			idx = i
			break
		}
	}
	if idx == -1 {
		return agentRemoveNotFound, ""
	}
	removed = cfg.Agents[idx].Name
	cfg.Agents = append(cfg.Agents[:idx], cfg.Agents[idx+1:]...)
	return agentRemoveOK, removed
}

// preferredSaveScope returns the save scope for TUI/ACP config changes.
// In the TUI we cannot prompt stdin (Bubble Tea owns the terminal), so the
// scope is auto-detected: save to local if the *currently selected* directory
// (dir — see activeConfigDir) has a .milk/config.json, else global.
// When saving to local, ensures the .milk/ directory and config file exist.
func preferredSaveScope(dir string) string {
	if config.HasLocalConfigIn(dir) {
		return "local"
	}
	return "global"
}

// saveLocalOrGlobal saves config using the host auto-detected scope for dir
// ("" = process cwd). If the scope is local, ensures the .milk/config.json
// file exists first.
func saveLocalOrGlobal(cfg config.Config, dir string) error {
	scope := preferredSaveScope(dir)
	if scope == "local" {
		if err := ensureLocalConfig(dir); err != nil {
			return fmt.Errorf("creating local config: %w", err)
		}
	}
	return config.SaveScopeIn(cfg, scope, dir)
}

// execAgentRemove removes the named agent from config.
// Refuses if the agent is currently active as primary or escalation.
func execAgentRemove(name string, st *interactiveState) string {
	if name == "" {
		return milkTag() + " usage: /agent remove <name>"
	}
	switch outcome, removed := removeAgentConfig(&st.cfg, name); outcome {
	case agentRemoveIsPrimary:
		return fmt.Sprintf("%s cannot remove %q — it is the active primary agent (switch first with /agent switch)", milkTag(), name)
	case agentRemoveIsEscalation:
		return fmt.Sprintf("%s cannot remove %q — it is the active escalation agent (switch first with /agent switch)", milkTag(), name)
	case agentRemoveNotFound:
		return fmt.Sprintf("%s no agent named %q", milkTag(), name)
	default:
		if err := saveLocalOrGlobal(st.cfg, activeConfigDir(st)); err != nil {
			return fmt.Sprintf("%s error saving config: %v", milkTag(), err)
		}
		return fmt.Sprintf("%s agent %q removed", milkTag(), removed)
	}
}

// execBash handles /bash [list | allow <prefix> | deny <prefix>].
func execBash(sub string, st *interactiveState) string {
	sub = strings.TrimSpace(sub)
	switch {
	case sub == "" || sub == "list":
		if len(st.cfg.DirectBashAllow) == 0 {
			return milkTag() + " direct_bash_allow: (empty — all commands prompt for confirmation)"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s direct_bash_allow:\n", milkTag())
		for _, p := range st.cfg.DirectBashAllow {
			fmt.Fprintf(&b, "  %s\n", p)
		}
		return strings.TrimRight(b.String(), "\n")
	case strings.HasPrefix(sub, "allow "):
		prefix := strings.TrimSpace(strings.TrimPrefix(sub, "allow "))
		if prefix == "" {
			return milkTag() + " usage: /bash allow <prefix>"
		}
		for _, p := range st.cfg.DirectBashAllow {
			if strings.EqualFold(p, prefix) {
				return fmt.Sprintf("%s %q is already in the allow list", milkTag(), prefix)
			}
		}
		st.cfg.DirectBashAllow = append(st.cfg.DirectBashAllow, prefix)
		if err := saveLocalOrGlobal(st.cfg, activeConfigDir(st)); err != nil {
			return fmt.Sprintf("%s error saving config: %v", milkTag(), err)
		}
		return fmt.Sprintf("%s added %q to direct_bash_allow", milkTag(), prefix)
	case strings.HasPrefix(sub, "deny "):
		prefix := strings.TrimSpace(strings.TrimPrefix(sub, "deny "))
		if prefix == "" {
			return milkTag() + " usage: /bash deny <prefix>"
		}
		idx := -1
		for i, p := range st.cfg.DirectBashAllow {
			if strings.EqualFold(p, prefix) {
				idx = i
				break
			}
		}
		if idx == -1 {
			return fmt.Sprintf("%s %q not found in allow list", milkTag(), prefix)
		}
		st.cfg.DirectBashAllow = append(st.cfg.DirectBashAllow[:idx], st.cfg.DirectBashAllow[idx+1:]...)
		if err := saveLocalOrGlobal(st.cfg, activeConfigDir(st)); err != nil {
			return fmt.Sprintf("%s error saving config: %v", milkTag(), err)
		}
		return fmt.Sprintf("%s removed %q from direct_bash_allow", milkTag(), prefix)
	default:
		return milkTag() + " usage: /bash list | /bash allow <prefix> | /bash deny <prefix>"
	}
}

// handleConfigCmd dispatches /config, /config init, /config open.
func (m model) handleConfigCmd(sub string) (tea.Model, tea.Cmd) {
	switch strings.ToLower(sub) {
	case "init":
		return m.handleConfigInitCmd()
	case "open":
		return m.handleConfigOpenCmd()
	case "show":
		return m.handleConfigShowCmd()
	case "":
		out, err := renderConfigJSON(activeConfigDir(m.st))
		if err != nil {
			m.appendTranscript(fmt.Sprintf("%s error: %v\n", milkTag(), err))
			return m, nil
		}
		m.colorizeForce = true
		m.appendTranscript(out)
		return m, nil
	default:
		m.appendTranscript(milkTag() + " usage: /config | /config show | /config init | /config open\n")
		return m, nil
	}
}

// handleConfigShowCmd displays the merged config with field source annotations
// (local/global/default) in the TUI transcript.
func (m model) handleConfigShowCmd() (tea.Model, tea.Cmd) {
	out, err := renderConfigShow(activeConfigDir(m.st))
	if err != nil {
		m.appendTranscript(fmt.Sprintf("%s error: %v\n", milkTag(), err))
		return m, nil
	}
	m.appendTranscript(out)
	return m, nil
}

// handleConfigOpenCmd opens the config file in $EDITOR or xdg-open.
// It opens the local config (.milk/config.json) when one exists, otherwise the
// global one — configOpenTarget, the same rule the CLI and the ACP server use.
// Uses tea.ExecProcess so the TUI is properly suspended
// while the editor runs.
func (m model) handleConfigOpenCmd() (tea.Model, tea.Cmd) {
	cfgPath, local, err := configOpenTarget(activeConfigDir(m.st))
	if err != nil {
		m.appendTranscript(fmt.Sprintf("%s error: %v\n", milkTag(), err))
		return m, nil
	}
	if local {
		m.appendTranscript(fmt.Sprintf("%s opening local config (inherits unset fields from global)…\n", milkTag()))
	} else {
		m.appendTranscript(fmt.Sprintf("%s opening %s…\n", milkTag(), cfgPath))
	}
	cmd := m.openInEditor(cfgPath)
	if cmd == nil {
		m.appendTranscript(fmt.Sprintf("%s no editor found — set $EDITOR or configure config_editors in config\n", milkTag()))
		return m, nil
	}
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		enableMouseTracking()
		if err != nil {
			return errMsg{err: fmt.Errorf("editor exited with error: %w", err)}
		}
		// Re-parse the config file after the editor exits so changes take
		// effect — merged against the currently selected directory (the
		// editor may have edited either the local or the global file).
		newCfg, parseErr := config.LoadMergedIn(activeConfigDir(m.st))
		return configReloadMsg{cfg: newCfg, err: parseErr}
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
		enableMouseTracking()
		if msg.respCh != nil {
			msg.respCh <- err
		}
		if err != nil {
			return errMsg{err: fmt.Errorf("editor exited with error: %w", err)}
		}
		return nil
	})
}

// handleUpdateCmd handles /update [check|status|install|skip].
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
			rel, err := updateCheck(ctx, &cfg, activeConfigDir(m.st))
			if err != nil {
				return errMsg{err: fmt.Errorf("update check: %w", err)}
			}
			if rel == nil {
				return errMsg{err: fmt.Errorf("already up to date (%s)", version)}
			}
			return updateAvailableMsg{release: rel, checkedAt: cfg.UpdateLastCheck}
		}

	case "status":
		// Renders from what is already known — no network round trip
		// (the acp_update.go updateStatusText parity). Install state
		// first (most action-relevant), then the pending release, then
		// the skip note.
		if m.updateInstalling {
			m.appendTranscript(milkTag() + " update install in progress\n")
			return m, nil
		}
		if m.pendingUpdate != nil {
			m.appendTranscript(fmt.Sprintf("%s %s available — /update install (or /update skip)\n", milkTag(), m.pendingUpdate.Tag))
			return m, nil
		}
		if skip := m.st.cfg.UpdateSkippedVersion; skip != "" {
			m.appendTranscript(fmt.Sprintf("%s %s skipped — it will not be announced again\n", milkTag(), skip))
			return m, nil
		}
		m.appendTranscript(milkTag() + " no update information yet — /update check\n")
		return m, nil

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
			return updateDoneMsg{err: updateInstall(ctx, rel, func(done, total int64) {
				send(updateProgressMsg{done: done, total: total})
			})}
		}

	case "skip":
		if m.pendingUpdate == nil {
			m.appendTranscript(milkTag() + " no pending update\n")
			return m, nil
		}
		skipUpdateRelease(&m.st.cfg, m.pendingUpdate.Tag, activeConfigDir(m.st))
		m.pendingUpdate = nil
		m.appendTranscript(milkTag() + " update skipped\n")
		return m, nil

	default:
		m.appendTranscript(milkTag() + " usage: /update [check|status|install|skip]\n")
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
func (m model) handleForgetKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	// Deleting the last percept empties the store — the memory panel's
	// finalization event: re-arm auto-open for its next content (ADR-0059).
	m.resetManualOverrideIfFinalized(regionMemory)
	return m
}

// handleSearchKey handles keypresses while ctrl+r search is active.
// Printable chars extend the query; ctrl+r searches again (older);
// backspace shrinks the query; enter/esc/ctrl+c accept and exit search.
func (m model) handleSearchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	if m.panelManualOverride == nil {
		m.panelManualOverride = map[panelRegion]bool{}
	}
	// Every toggle records the manual override; a toggle that *closes* a
	// panel whose content has already finalized drops it again right away
	// (resetManualOverrideIfFinalized) — the sticky rule exists to keep
	// live content from re-opening a dismissed panel, not to lock a
	// settled one out of its next content forever (ADR-0059).
	switch sub {
	case "memory":
		m.panelManualOverride[regionMemory] = true
		m.panelMemory = !m.panelMemory
		if err := m.saveMemoryPreference(); err != nil {
			m.appendTranscript(fmt.Sprintf("%s memory panel preference could not be saved: %v\n", milkTag(), err))
		}
		m.refreshPrompt()
		m.syncLayout()
		var tick tea.Cmd
		if m.panelMemory {
			m.notify("memory panel: on", "/panel memory")
			tick = memoryPollTick()
		} else {
			m.notify("memory panel: off", "/panel memory")
		}
		return m, tick
	case "tasks":
		m.panelManualOverride[regionTasks] = true
		m.panelTasks = !m.panelTasks
		if !m.panelTasks {
			m.resetManualOverrideIfFinalized(regionTasks)
		}
		m.refreshPrompt()
		m.syncLayout()
		if m.panelTasks {
			m.notify("tasks panel: on", "/panel tasks")
		} else {
			m.notify("tasks panel: off", "/panel tasks")
		}
		return m, nil
	case "workflow":
		m.panelManualOverride[regionWorkflow] = true
		m.workflowPanelOpen = !m.workflowPanelOpen
		if !m.workflowPanelOpen {
			m.resetManualOverrideIfFinalized(regionWorkflow)
		}
		m.syncLayout()
		if m.workflowPanelOpen {
			m.notify("workflow panel: on", "/panel workflow")
		} else {
			m.notify("workflow panel: off", "/panel workflow")
		}
		return m, nil
	case "background":
		m.panelManualOverride[regionBackground] = true
		m.panelBackground = !m.panelBackground
		if !m.panelBackground {
			m.resetManualOverrideIfFinalized(regionBackground)
		}
		m.syncLayout()
		if m.panelBackground {
			m.notify("background panel: on", "/panel background")
		} else {
			m.notify("background panel: off", "/panel background")
		}
		return m, nil
	case "project":
		m.panelManualOverride[regionProject] = true
		m.panelProject = !m.panelProject
		if !m.panelProject {
			m.resetManualOverrideIfFinalized(regionProject)
		}
		m.syncLayout()
		if m.panelProject {
			m.notify("project panel: on", "/panel project")
			// First refresh immediately; the poll keeps it live-ish afterwards.
			return m, tea.Batch(projectPollTick(), refreshProjectCmd(m.st.cwd))
		}
		m.notify("project panel: off", "/panel project")
		return m, nil
	case "git":
		m.panelManualOverride[regionGit] = true
		m.panelGit = !m.panelGit
		if !m.panelGit {
			m.resetManualOverrideIfFinalized(regionGit)
		}
		m.syncLayout()
		if m.panelGit {
			m.notify("git panel: on", "/panel git")
			return m, tea.Batch(gitPollTick(), refreshGitCmd(m.st.cwd))
		}
		m.notify("git panel: off", "/panel git")
		return m, nil
	default:
		m.appendTranscript(milkTag() + " usage: /panel memory|tasks|background|workflow|project|git (or F1-F6)\n")
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
