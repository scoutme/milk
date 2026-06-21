package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/claudesettings"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
)

const cmdEscalate = "/escalate"
const cmdPrimary = "/primary"
const cmdPaste = "/paste"
const cmdLearn = "/learn"
const cmdOtel = "/otel"
const cmdMetrics = "/metrics"
const cmdUsage = "/usage"
const cmdMemory = "/memory"
const cmdExport = "/export"
const cmdHistory = "/history"
const cmdPanel = "/panel"
const cmdForget = "/forget"
const cmdSkipPerms = "/skip-permissions"
const cmdAgent = "/agent"
const cmdColorize = "/colorize"
const cmdThink = "/think"
const cmdSetup = "/setup"
const cmdConfig = "/config"

var slashCommands = []string{
	cmdEscalate, cmdPrimary, cmdPaste, cmdLearn, cmdOtel, cmdMetrics, cmdUsage, cmdMemory, cmdExport, cmdHistory, cmdPanel, cmdForget, cmdSkipPerms, cmdAgent, cmdColorize, cmdThink, cmdSetup, cmdConfig,
	"/new", "/drop", "/list", "/help", "/exit", "/quit",
}

// initWizardState tracks state for the /config init TUI wizard.
type initWizardState struct {
	step    initWizardStep
	primary config.AgentConfig
	escCLI  bool // whether to use default claude-cli escalation
	// limits step
	largeCtx       bool
	limitToolIter  int // max_tool_iterations override (0 = not set)
	limitMsgBudget int // message_budget_chars override (0 = not set)
	limitCtxBudget int // context_budget_chars override (0 = not set)
	limitsSubStep  int // 0=ask large? 1=tool_iter 2=msg_budget 3=ctx_budget
}

type initWizardStep int

const (
	initStepGreet      initWizardStep = iota // display greeting, advance immediately
	initStepName                             // ask agent name
	initStepProvider                         // ask provider (menu 1–6)
	initStepURL                              // ask URL (providers that need it)
	initStepChatPath                         // ask chat_path (bearer only, skip if standard)
	initStepModel                            // ask model
	initStepAuth                             // ask api_key (blank → go to initStepTokenCmd)
	initStepTokenCmd                         // ask token_cmd
	initStepAWSRegion                        // ask aws_region (bedrock only)
	initStepLimits                           // ask large context window + limit overrides
	initStepEscalation                       // ask escalation agent choice
	initStepOpenConfig                       // ask whether to open config in editor
	initStepDone
)

// telegramSetupState tracks state for the /setup telegram wizard.
type telegramSetupState struct {
	token   string
	chatID  int64
	botName string // @username of the bot, set after token validation
	step    telegramSetupStep
}

type telegramSetupStep int

const (
	telegramStepToken   telegramSetupStep = iota // waiting for token input
	telegramStepWaitMsg                          // token validated; waiting for user to message the bot
	telegramStepDone
)

func promptLabel(_ *interactiveState) string {
	return "❯ "
}

const interactiveHelp = `
── Routing ──────────────────────────────────────────────────────────────
  /escalate              pin all turns to escalation agent (/primary to unpin)
  /escalate <msg>        force this turn to escalation agent, then resume routing
  /escalate fresh        force a new escalation context (new session + memory instructions re-injected)
  /escalate fresh <msg>  same, single-turn override
  /primary               pin all turns to primary agent (/escalate to unpin)
  /primary <msg>         force this turn to primary agent, then resume routing

── Sessions ─────────────────────────────────────────────────────────────
  /list                  list sessions for current directory
  /new                   start a fresh session
  /drop                  delete current session
  /export                print session transcript (text)
  /export json           print session transcript as JSON
  /export <path>         write session transcript to file

── Agents ───────────────────────────────────────────────────────────────
  /agent                 show active primary and escalation agents
  /agent list            list all configured agents (* = active)
  /agent add             add a new agent interactively
  /agent add name=… url=… model=… [provider=…] [api_key=…] [aws_region=…]
  /agent switch <name> [as primary|escalation]   (prompts if args missing)
  /skip-permissions      show current skip-permissions state
  /skip-permissions on   all agents auto-approve tool uses (no prompts)
  /skip-permissions off  agents prompt before running side-effecting tools

── Memory ───────────────────────────────────────────────────────────────
  /learn <fact>          store a persistent memory
  /memory                list all percepts (session + global)
  /memory global         list only global percepts
  /memory session        list only session percepts
  /memory <pat>          list percepts whose content matches <pat>
  /memory show <pat|#id>  show full details of matching percepts
  /forget <pat|#id>      delete a percept (asks for confirmation)
  /panel memory          toggle the memory panel (right side)

── Display ──────────────────────────────────────────────────────────────
  /colorize              show current colorization mode
  /colorize off          no colorization
  /colorize fenced       highlight fenced code blocks only
  /colorize balanced     fenced blocks + inline markdown (default)
  /colorize full         full glamour markdown render (experimental)
  /think                 show current reasoning visibility
  /think on              show thinking/reasoning tokens inline
  /think off             hide thinking tokens ([thinking…] placeholder)
  /history               show current history navigation mode
  /history global        navigate global input history
  /history session       navigate session input history (default)

── Observability ────────────────────────────────────────────────────────
  /usage                 token usage report for this session and all-time totals
  /metrics               show latest metric values
  /otel                  show OTel file sizes and record counts
  /otel on               enable OTel for this session
  /otel off              disable OTel for this session
  /otel trim             archive current OTel files and start fresh

── Setup ─────────────────────────────────────────────────────────────────
  /config                print the current config (~/.milk/config.json)
  /config init           run the setup wizard (configure primary + escalation agents)
  /config open           open config in $EDITOR / system default editor
  /setup telegram        configure Telegram remote oversight interactively
  /setup telegram on     enable Telegram (credentials must already be configured)
  /setup telegram off    disable Telegram (credentials are preserved)

── General ──────────────────────────────────────────────────────────────
  /help                  show this help
  /exit  /quit           quit

── Keyboard ─────────────────────────────────────────────────────────────
  Scrolling
    Mouse wheel / PgUp/PgDn / Ctrl+U / Ctrl+F   scroll transcript

  Agent control
    Ctrl+C   interrupt current agent turn
    Ctrl+T   toggle thinking/reasoning visibility (works during streaming)

  Input history
    Up / Down (single-line input)   navigate history
    Ctrl+Up / Ctrl+Down             navigate history
    Ctrl+R                          reverse incremental search
    Ctrl+S                          forward incremental search

  Multi-line input
    Ctrl+N / Shift+Alt+Enter / Alt+Enter   insert newline
    Paste                                  multi-line paste sent as one block

  @ prefix
    @path   reference a file path (Tab-completes)`

const errFmt = "error: %v\n"

// interactiveState holds mutable state for the interactive loop.
type interactiveState struct {
	sess          *session.Session
	forceEscalate bool
	forcePrimary  bool
	// stickyEscalate is set when the user explicitly calls /escalate with no
	// prompt. It causes every subsequent turn to route to the escalation agent
	// until the user calls /primary or closes the session. forceEscalate is reset after each
	// turn; stickyEscalate persists across turns. Shown as "(pinned)" in the status bar.
	stickyEscalate bool
	// autoStickyEscalate is set automatically when the router first escalates
	// (without an explicit /escalate command) and cfg.StickyEscalationEnabled() is true.
	// Cleared by /primary, Ctrl+C on empty input, or a forcePrimary turn.
	// Shown as "(sticky)" in the status bar to distinguish it from user-pinned.
	autoStickyEscalate bool
	// stickyPrimary mirrors stickyEscalate for the local model.
	stickyPrimary bool
	cwd           string
	cfg           config.Config
	mem           *memory.Store
	cs            *claudesettings.Store // Claude project settings (permissions persistence)
	program       *tea.Program          // set after tea.NewProgram, before Run

	// toolFutures caches per-tool answer channels created by OnToolUse as soon
	// as each tool call is detected in the stream. The user is asked immediately
	// via the TUI; handleStructuredDenials reads the answer (blocking briefly if
	// the user hasn't responded yet). Keyed by tool name.
	toolFutures     map[string]chan string
	skipPermissions bool             // session-level override for DangerouslySkipPermissions
	localPerms      *local.PermStore // persisted tool grants for the primary local agent
	notifier        oversight.Notifier

	// lastEscalationContextHash is a short hash of the last --append-system-prompt-file
	// content sent to the CLI escalation agent. Used to suppress re-sends when unchanged.
	lastEscalationContextHash string

	// activeFallbackTarget is set by runTurn just before a turn runs when the
	// router's decision was overridden by availability (e.g. local down → escalation).
	// "" means no override is active. Used by agentLabel to show the correct agent
	// during the turn without waiting for session state to update.
	activeFallbackTarget string
}

// primaryAgentName returns the display name of the configured primary agent.
func (st *interactiveState) primaryAgentName() string {
	return st.cfg.ActiveAgent().Name
}

// escalationAgentName returns the display name of the configured escalation agent.
func (st *interactiveState) escalationAgentName() string {
	return st.cfg.EscalationAgentConfig().Name
}

// extractSlashCommand scans input for a known slash command token anywhere in
// the line. Returns the command, the remaining text with the token stripped,
// and whether a command was found.
func extractSlashCommand(input string) (cmd, rest string, found bool) {
	words := strings.Fields(input)
	var keep []string
	for _, w := range words {
		if !found && strings.HasPrefix(w, "/") {
			if slices.Contains(slashCommands, w) {
				cmd = w
				found = true
			} else {
				keep = append(keep, w)
			}
		} else {
			keep = append(keep, w)
		}
	}
	return cmd, strings.Join(keep, " "), found
}

// promptFriendly is the set of slash commands that can be combined with a prompt.
var promptFriendly = map[string]bool{
	cmdEscalate: true,
	cmdPrimary:  true,
	cmdLearn:    true,
	cmdOtel:     true,
}

// busySafeCommands are slash commands that can run while an agent turn is in progress.
// All are read-only or display-only and never dispatch a new agent turn.
var busySafeCommands = map[string]bool{
	"/help":     true,
	cmdThink:    true,
	cmdColorize: true,
	cmdPanel:    true,
	cmdHistory:  true,
	cmdMemory:   true,
	cmdUsage:    true,
	cmdMetrics:  true,
	cmdExport:   true,
	cmdPaste:    true,
}

// handleSlashCommand processes a slash command with optional surrounding prompt text.
// Returns (exit, prompt-to-dispatch, output): exit=true means quit the loop,
// prompt is non-empty when the command should be followed by an immediate dispatch,
// output is text to print via tea.Println.
func handleSlashCommand(cmd, prompt string, st *interactiveState) (exit bool, dispatch, output string) {
	switch cmd {
	case "/exit", "/quit":
		return true, "", ""
	case "/help", "/new", "/drop", "/list", cmdPaste:
		output = execNonPromptCmd(cmd, prompt, st)
	case cmdLearn:
		output = execLearn(prompt, st)
	case cmdOtel:
		output = execOtel(prompt, st)
	case cmdMetrics:
		output = execMetrics()
	case cmdUsage:
		output = execUsage(st)
	case cmdMemory:
		output = execMemory(prompt, st)
	case cmdExport:
		output = execExport(prompt, st)
	case cmdEscalate:
		st.forcePrimary = false
		st.stickyPrimary = false
		if prompt == "fresh" || strings.HasPrefix(prompt, "fresh ") {
			// /escalate fresh [msg]: force ContextModeFirst on the next escalation turn,
			// dropping the existing session ID and nonce so Claude starts with clean context.
			rest := strings.TrimPrefix(strings.TrimPrefix(prompt, "fresh"), " ")
			st.sess.ForceFreshEscalation = true
			st.forceEscalate = rest != ""
			if rest == "" {
				st.stickyEscalate = true
				output = milkTag() + " fresh escalation context — next turn starts a new " + blue(st.escalationAgentName()) + " session"
			} else {
				output = milkTag() + " fresh escalation context for this turn"
			}
			return false, rest, output
		}
		if prompt == "" {
			// No inline prompt: pin all subsequent turns to escalation agent.
			st.stickyEscalate = true
			st.forceEscalate = false
			output = milkTag() + " pinned to " + blue(st.escalationAgentName()) + " (use /primary to unpin)"
		} else {
			// Inline prompt: single-turn override only.
			st.forceEscalate = true
		}
		return false, prompt, output
	case cmdPrimary:
		st.forceEscalate = false
		st.stickyEscalate = false
		st.autoStickyEscalate = false
		if prompt == "" {
			// No inline prompt: pin all subsequent turns to local.
			st.stickyPrimary = true
			st.forcePrimary = false
			output = milkTag() + " pinned to " + green(st.cfg.ActiveAgent().Name) + " (use /escalate to unpin)"
		} else {
			// Inline prompt: single-turn override only.
			st.forcePrimary = true
		}
		return false, prompt, output
	case cmdSkipPerms:
		output = execSkipPerms(prompt, st)
	case cmdThink:
		// execThink is handled in repl.go where it can toggle model.showThinking.
		// This case is a no-op here; the TUI intercepts cmdThink before it reaches
		// handleSlashCommand. Guard to prevent "unknown command" output.
	case cmdSetup:
		// Handled in repl.go (needs model state). No-op here.
	case cmdConfig:
		// Handled in repl.go (needs model state and config path). No-op here.
	default:
		output = fmt.Sprintf("unknown command %q — type /help", cmd)
	}
	return false, "", output
}

// execNonPromptCmd runs a command that has no prompt semantics.
// Returns any output to be printed. Warns if the user included extra text.
func execNonPromptCmd(cmd, prompt string, st *interactiveState) string {
	var out strings.Builder
	if prompt != "" && !promptFriendly[cmd] {
		fmt.Fprintf(&out, "%s %s does not accept a prompt — text ignored\n", milkTag(), cmd)
	}
	switch cmd {
	case "/help":
		fmt.Fprint(&out, interactiveHelp)
	case "/new":
		var err error
		st.sess, err = session.New(st.cwd, "")
		if err != nil {
			fmt.Fprintf(&out, errFmt, err)
			return out.String()
		}
		obs.ResetSessionTokens()
		fmt.Fprintf(&out, "%s new session %s", milkTag(), st.sess.ID[:8])
	case "/drop":
		if err := dropAndNewSession(st, &out); err != nil {
			fmt.Fprintf(&out, red("error: ")+"%v", err)
		}
	case "/list":
		if err := listSessions(st.cwd, &out); err != nil {
			fmt.Fprintf(&out, errFmt, err)
		}
	case cmdPaste:
		fmt.Fprint(&out, milkTag()+" hint: paste multi-line text directly, or use Ctrl+N / Shift+Alt+Enter to insert a newline")
	}
	return out.String()
}

// execUsage prints token usage by agent role and model, plus current-session totals.
func execUsage(st *interactiveState) string {
	otelDir, err := config.OtelDir()
	if err != nil {
		return fmt.Sprintf("%s error: %v", milkTag(), err)
	}
	var sessEntries []obs.SessionTokenEntry
	var turns int64
	if st != nil && st.sess != nil {
		for _, u := range st.sess.Tokens {
			sessEntries = append(sessEntries, obs.SessionTokenEntry{
				Model: u.Model, Agent: u.Agent,
				Prompt: u.Prompt, Completion: u.Completion,
				CacheRead: u.CacheRead, CacheCreation: u.CacheCreation,
			})
		}
		turns = int64(st.sess.EscalationTurnCount() + st.sess.LocalTurnCount())
	}
	return milkTag() + " " + obs.FormatTokenUsage(context.Background(), otelDir, sessEntries, turns)
}

// execMetrics prints the most recent metric values from the otel metrics file.
func execMetrics() string {
	otelDir, err := config.OtelDir()
	if err != nil {
		return fmt.Sprintf("%s error: %v", milkTag(), err)
	}
	return milkTag() + " " + obs.FormatMetrics(otelDir)
}

// execOtel handles /otel [trim|off|on] commands.
func execOtel(sub string, st *interactiveState) string {
	otelDir, err := config.OtelDir()
	if err != nil {
		return fmt.Sprintf("%s error: %v", milkTag(), err)
	}
	switch strings.TrimSpace(sub) {
	case "trim":
		if err := obs.Trim(otelDir); err != nil {
			return fmt.Sprintf("%s trim failed: %v", milkTag(), err)
		}
		return milkTag() + " otel files archived — starting fresh"
	case "off":
		st.cfg.Otel.Enabled = false
		return milkTag() + " OTel disabled for this session"
	case "on":
		st.cfg.Otel.Enabled = true
		return milkTag() + " OTel re-enabled for this session"
	default:
		return milkTag() + " " + obs.FormatStats(otelDir)
	}
}

// execExport dumps the current session as text or JSON, optionally to a file.
// sub may be "json", a file path, or empty (text to stdout).
func execExport(sub string, st *interactiveState) string {
	sub = strings.TrimSpace(sub)
	format := "text"
	outputPath := ""
	if sub == "json" {
		format = "json"
	} else if sub != "" {
		outputPath = sub
	}

	var content string
	switch format {
	case "json":
		data, err := session.ExportJSON(st.sess)
		if err != nil {
			return fmt.Sprintf("%s export error: %v", milkTag(), err)
		}
		content = string(data)
	default:
		content = session.ExportText(st.sess)
	}

	if outputPath != "" {
		if err := os.WriteFile(outputPath, []byte(content), 0o644); err != nil {
			return fmt.Sprintf("%s export error: %v", milkTag(), err)
		}
		return fmt.Sprintf("%s session exported to %s (%d bytes)", milkTag(), outputPath, len(content))
	}
	return content
}

// execMemory lists percepts from the memory store with optional filters.
// sub may be "global", "session", or a free-form content pattern.
func execMemory(sub string, st *interactiveState) string {
	if st.mem == nil {
		return milkTag() + " memory store not available"
	}
	sub = strings.TrimSpace(sub)

	if sub == "show" {
		return milkTag() + " usage: /memory show <description> or /memory show #<id>"
	}
	if rest, ok := strings.CutPrefix(sub, "show "); ok {
		return execMemoryShow(strings.TrimSpace(rest), st)
	}

	opts := memory.ListOpts{}
	switch sub {
	case "global":
		opts.Scope = "global"
	case "session":
		opts.Scope = "session"
	default:
		opts.Pattern = sub
	}
	percepts := st.mem.List(opts)
	if len(percepts) == 0 {
		return milkTag() + " (no percepts found)"
	}
	return milkTag() + "\n" + memory.FormatList(percepts)
}

func execMemoryShow(pat string, st *interactiveState) string {
	var percepts []memory.Percept
	if strings.HasPrefix(pat, "#") {
		percepts = st.mem.FindByIDPrefix(pat[1:])
	} else {
		percepts = st.mem.List(memory.ListOpts{Pattern: pat})
	}
	if len(percepts) == 0 {
		return milkTag() + " no percepts match " + fmt.Sprintf("%q", pat)
	}
	return milkTag() + "\n" + memory.FormatListVerbose(percepts)
}

// execLearn stores a user fact in the global memory store.
func execLearn(fact string, st *interactiveState) string {
	if strings.TrimSpace(fact) == "" {
		return milkTag() + " usage: /learn <fact to remember>"
	}
	if st.mem == nil {
		return milkTag() + " memory store not available"
	}
	id, err := st.mem.RecordGlobal(context.Background(), fact, memory.ProducerUser, memory.ConsumerAll, memory.Roles{})
	if dup, ok := memory.IsDuplicate(err); ok {
		return fmt.Sprintf("%s skipped — similar memory already exists (%.0f%% overlap): %q (#%s)",
			milkTag(), dup.Similarity*100, dup.Existing.Content, id[:8])
	}
	if err != nil {
		return fmt.Sprintf("%s error storing memory: %v", milkTag(), err)
	}
	return fmt.Sprintf("%s learned: %q (id %s)", milkTag(), fact, id[:8])
}

// execSkipPerms handles /skip-permissions [on|off].
func execSkipPerms(sub string, st *interactiveState) string {
	switch strings.TrimSpace(sub) {
	case "on":
		st.skipPermissions = true
		return milkTag() + " " + red("dangerously_skip_permissions ON") + " — all agents will auto-approve tool uses"
	case "off":
		st.skipPermissions = false
		return milkTag() + " dangerously_skip_permissions OFF — agents will prompt before running tools"
	default:
		state := "off"
		if st.skipPermissions {
			state = red("on")
		}
		return fmt.Sprintf("%s dangerously_skip_permissions is %s  (use /skip-permissions on|off)", milkTag(), state)
	}
}

// execColorize handles /colorize [off|fenced|balanced|full].
// With no arg: shows the current mode. With a valid mode: switches it live and saves to config.
func execColorize(sub string, st *interactiveState) string {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return fmt.Sprintf("%s colorization mode: %s  (off | fenced | balanced | full[experimental])", milkTag(), bold(string(ParseColorizeMode(st.cfg.Colorization))))
	}
	valid := map[string]bool{"off": true, "fenced": true, "balanced": true, "full": true}
	if !valid[sub] {
		return fmt.Sprintf("%s unknown mode %q — valid values: off, fenced, balanced, full (experimental)", milkTag(), sub)
	}
	st.cfg.Colorization = sub
	if err := config.Save(st.cfg); err != nil {
		return fmt.Sprintf("%s set colorization to %s (config save failed: %v)", milkTag(), bold(sub), err)
	}
	return fmt.Sprintf("%s colorization set to %s", milkTag(), bold(sub))
}

// execAgent shows the active local-agent provider configuration (no credentials).
// arg is the remainder after "/agent" — empty for status display.
func execAgent(st *interactiveState) string {
	return agentLine("primary", st.cfg.ActiveAgent()) + "\n" +
		agentLine("escalation", st.cfg.EscalationAgentConfig())
}

// agentLine formats a single agent config summary line for /agent output.
func agentLine(role string, ac config.AgentConfig) string {
	provider := strings.ToLower(strings.TrimSpace(ac.Provider))
	var authDesc string
	switch provider {
	case "", "local":
		authDesc = "none (local / no-auth)"
	case "claude-cli":
		authDesc = "claude CLI subprocess"
	case "bedrock":
		region := ac.AWSRegion
		if region == "" {
			region = "(unset)"
		}
		service := ac.AWSService
		if service == "" {
			service = "bedrock"
		}
		authDesc = fmt.Sprintf("AWS SigV4 (region: %s, service: %s)", region, service)
	default:
		if ac.APIKey != "" {
			authDesc = fmt.Sprintf("Bearer token (%s, key set)", provider)
		} else {
			authDesc = fmt.Sprintf("Bearer token (%s, key NOT set)", provider)
		}
	}

	extraHeaders := len(ac.Headers)
	var headerNote string
	if extraHeaders > 0 {
		headerNote = fmt.Sprintf(", %d extra header(s)", extraHeaders)
	}

	name := ac.Name
	if name == "" {
		name = role
	}

	if ac.IsCLI() {
		return fmt.Sprintf("%s %s agent: %s\n  auth:   %s", milkTag(), role, bold(name), authDesc)
	}
	return fmt.Sprintf("%s %s agent: %s\n  url:    %s\n  model:  %s\n  auth:   %s%s",
		milkTag(), role, bold(name), bold(ac.URL), bold(ac.Model), authDesc, headerNote)
}

// dropAndNewSession drops the current session, creates a fresh one, and writes output to w.
func dropAndNewSession(st *interactiveState, w *strings.Builder) error {
	id := st.sess.ID
	if err := session.Drop(id, st.cwd); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s dropped session %s\n", milkTag(), id[:8])
	var err error
	st.sess, err = session.New(st.cwd, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s new session %s", milkTag(), st.sess.ID[:8])
	return nil
}

// listSessions writes the session list for cwd to w.
func listSessions(cwd string, w *strings.Builder) error {
	entries, err := session.List(cwd)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprint(w, "no sessions found")
		return nil
	}
	for dir, list := range entries {
		fmt.Fprintf(w, "%s\n", dir)
		for _, e := range list {
			name := e.Name
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Fprintf(w, "  %s  %-20s  %s", e.ID[:8], name, e.LastUsed.Format("2006-01-02 15:04"))
		}
	}
	return nil
}

func loadSession(cwd string, flagNew bool, flagSession string) (*session.Session, error) {
	if flagNew {
		return session.New(cwd, flagSession)
	}
	return session.Resume(cwd, flagSession)
}
