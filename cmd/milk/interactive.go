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
	"github.com/scoutme/milk/internal/mcp"
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
const cmdOpen = "/open"
const cmdMCP = "/mcp"

var slashCommands = []string{
	cmdEscalate, cmdPrimary, cmdPaste, cmdLearn, cmdOtel, cmdMetrics, cmdUsage, cmdMemory, cmdExport, cmdHistory, cmdPanel, cmdForget, cmdSkipPerms, cmdAgent, cmdColorize, cmdThink, cmdSetup, cmdConfig, cmdOpen, cmdMCP,
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
	// agent-tools step
	toolAgentNames []string // agent names the user wants to enable as tools
}

type initWizardStep int

const (
	initStepName       initWizardStep = iota // ask agent name
	initStepProvider                         // ask provider (menu 1–6)
	initStepURL                              // ask URL (providers that need it)
	initStepChatPath                         // ask chat_path (bearer only, skip if standard)
	initStepModel                            // ask model
	initStepAuth                             // ask api_key (blank → go to initStepTokenCmd)
	initStepTokenCmd                         // ask token_cmd
	initStepAWSRegion                        // ask aws_region (bedrock only)
	initStepLimits                           // ask large context window + limit overrides
	initStepEscalation                       // ask escalation agent choice
	initStepAgentTools                       // ask which agents to enable as tools
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
  /agent tool list [<agent>|global]              show tool-agents (default: primary)
  /agent tool enable <tool> [for <agent>|global]  enable a tool-agent entry
  /agent tool disable <tool> [for <agent>|global]  disable a tool-agent entry
  /agent tool add <tool> description=<desc> [for <agent>|global]  add a new tool-agent entry
  /agent tool remove <tool> [for <agent>|global]  remove a tool-agent entry
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
  /otel debug enable     enable full debug logging (log_context, debug_*, log_level=DEBUG)
  /otel debug disable    disable debug logging (restores defaults)

── MCP Servers ───────────────────────────────────────────────────────────
  /mcp                   list configured MCP servers and their status
  /mcp list [<agent>]    list MCP servers for an agent (default: primary)
  /mcp add name=… url=… [transport=…] [auth=…] [api_key=…] [timeout=…]
  /mcp remove <name>     remove an MCP server by name
  /mcp enable <name>     enable a disabled MCP server
  /mcp disable <name>    disable an MCP server (keeps config)
  /mcp tools [<name>]    list tools exported by connected MCP server(s)
  /mcp assign <server> for <agent>   add server to agent's mcp_servers list
  /mcp unassign <server> for <agent>  remove server from agent's list

── Setup ─────────────────────────────────────────────────────────────────
  /config                print the current config (~/.milk/config.json)
  /config init           run the setup wizard (configure primary + escalation agents)
  /config open           open config in $EDITOR / system default editor
  /open <file>           open any file in $EDITOR (agent can also call the open_file tool)
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
	cmdMCP:      true,
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
	case cmdOpen:
		// Handled in repl.go (needs tea.ExecProcess). No-op here.
	case cmdMCP:
		output = execMCP(prompt, st)
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
	case "debug enable":
		if err := runOtelDebug(true); err != nil {
			return fmt.Sprintf("%s error: %v", milkTag(), err)
		}
		st.cfg.Otel.LogContext = true
		st.cfg.Otel.LogLevel = "DEBUG"
		st.cfg.DebugCLILog = true
		st.cfg.DebugLocalLog = true
		cliPath, _ := config.CLIDebugLogPath()
		localPath, _ := config.LocalDebugLogPath()
		return milkTag() + " debug logging enabled\n" +
			"  claude NDJSON → " + cliPath + "\n" +
			"  local SSE     → " + localPath + "\n" +
			"  payloads      → " + otelDir + "/logs.jsonl"
	case "debug disable":
		if err := runOtelDebug(false); err != nil {
			return fmt.Sprintf("%s error: %v", milkTag(), err)
		}
		st.cfg.Otel.LogContext = false
		st.cfg.Otel.LogLevel = "INFO" // in-memory reset; disk already restored by runOtelDebug
		st.cfg.DebugCLILog = false
		st.cfg.DebugLocalLog = false
		return milkTag() + " debug logging disabled"
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
		if outputPath != "" {
			content = session.ExportText(st.sess) // plain — no ANSI in files
		} else {
			content = session.ExportTextColorized(st.sess) // colorized for terminal
		}
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

// execAgentTool dispatches /agent tool <verb> [args] subcommands.
func execAgentTool(sub string, st *interactiveState) string {
	parts := strings.Fields(sub)
	if len(parts) == 0 {
		return execAgentToolList("", st)
	}
	verb := parts[0]
	rest := strings.TrimSpace(strings.TrimPrefix(sub, verb))

	// parse optional "for <agent>|global" suffix
	scope, toolName := parseAgentToolScope(rest)

	switch verb {
	case "list":
		// For list, the "tool name" is actually the scope argument.
		listScope := toolName
		if scope != "" {
			listScope = scope
		}
		return execAgentToolList(listScope, st)
	case "enable":
		if toolName == "" {
			return milkTag() + " usage: /agent tool enable <tool-agent> [for <agent>|global]"
		}
		return execAgentToolEnable(toolName, scope, st)
	case "disable":
		if toolName == "" {
			return milkTag() + " usage: /agent tool disable <tool-agent> [for <agent>|global]"
		}
		return execAgentToolDisable(toolName, scope, st)
	case "add":
		if toolName == "" {
			return milkTag() + " usage: /agent tool add <tool-agent> description=<desc> [for <agent>|global]"
		}
		return execAgentToolAdd(toolName, scope, rest, st)
	case "remove":
		if toolName == "" {
			return milkTag() + " usage: /agent tool remove <tool-agent> [for <agent>|global]"
		}
		return execAgentToolRemove(toolName, scope, st)
	default:
		return milkTag() + " unknown subcommand: /agent tool " + verb + "\n  try: list, enable, disable, add, remove"
	}
}

// parseAgentToolScope parses "<tool-name> [for <agent>|global]" and returns (scope, toolName).
// scope is "" (default: active primary), "global", or an agent name.
func parseAgentToolScope(s string) (scope, toolName string) {
	if idx := strings.Index(s, " for "); idx >= 0 {
		toolName = strings.TrimSpace(s[:idx])
		scope = strings.TrimSpace(s[idx+5:])
		return
	}
	toolName = strings.TrimSpace(s)
	return
}

// execAgentToolList shows effective tool-agents for the given scope.
// scope == "" → use the active primary agent; scope == "global" → global list only;
// otherwise → EffectiveToolAgents for the named agent.
func execAgentToolList(scope string, st *interactiveState) string {
	var targetName string
	switch scope {
	case "", "primary":
		targetName = st.cfg.ActiveAgent().Name
	case "global":
		// Show raw global list.
		entries := st.cfg.AgentTools
		if len(entries) == 0 {
			return milkTag() + " no global tool-agents configured"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s global tool-agents:\n", milkTag())
		for _, e := range entries {
			status := "enabled"
			if !e.IsEnabled() {
				status = "disabled"
			}
			desc := e.Description
			if len(desc) > 55 {
				desc = desc[:52] + "..."
			}
			fmt.Fprintf(&b, "  %-20s  %-8s  %-8s  %s\n", e.Agent, status, "global", desc)
		}
		return strings.TrimRight(b.String(), "\n")
	default:
		targetName = scope
	}

	entries := st.cfg.EffectiveToolAgents(targetName)
	if len(entries) == 0 {
		return fmt.Sprintf("%s no tool-agents configured for %q", milkTag(), targetName)
	}

	// Build lookup sets for scope badge computation.
	globalNames := make(map[string]bool, len(st.cfg.AgentTools))
	for _, e := range st.cfg.AgentTools {
		globalNames[strings.ToLower(e.Agent)] = true
	}
	overrideNames := make(map[string]bool)
	for _, ac := range st.cfg.Agents {
		if strings.EqualFold(ac.Name, targetName) {
			for _, te := range ac.Tools {
				overrideNames[strings.ToLower(te.Agent)] = true
			}
			break
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s tool-agents for %q:\n", milkTag(), targetName)
	for _, e := range entries {
		status := "enabled"
		if !e.IsEnabled() {
			status = "disabled"
		}
		key := strings.ToLower(e.Agent)
		scopeBadge := "global"
		if overrideNames[key] && globalNames[key] {
			scopeBadge = "override"
		} else if overrideNames[key] {
			scopeBadge = "local"
		}
		desc := e.Description
		if len(desc) > 55 {
			desc = desc[:52] + "..."
		}
		fmt.Fprintf(&b, "  %-20s  %-8s  %-8s  %s\n", e.Agent, status, scopeBadge, desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

// execAgentToolEnable sets Enabled=true on a matching entry.
func execAgentToolEnable(toolName, scope string, st *interactiveState) string {
	t := true
	if scope == "global" {
		idx := findToolEntryIdx(st.cfg.AgentTools, toolName)
		if idx < 0 {
			return fmt.Sprintf("%s tool-agent %q not found in global list — use /agent tool add first", milkTag(), toolName)
		}
		st.cfg.AgentTools[idx].Enabled = &t
	} else {
		agentName := scope
		if agentName == "" {
			agentName = st.cfg.ActiveAgent().Name
		}
		acIdx := findAgentIdx(st.cfg, agentName)
		if acIdx < 0 {
			return fmt.Sprintf("%s agent %q not found", milkTag(), agentName)
		}
		idx := findToolEntryIdx(st.cfg.Agents[acIdx].Tools, toolName)
		if idx < 0 {
			// Check if it exists globally; if so, create a per-agent override.
			gIdx := findToolEntryIdx(st.cfg.AgentTools, toolName)
			if gIdx < 0 {
				return fmt.Sprintf("%s tool-agent %q not found — use /agent tool add first", milkTag(), toolName)
			}
			entry := st.cfg.AgentTools[gIdx]
			entry.Enabled = &t
			st.cfg.Agents[acIdx].Tools = append(st.cfg.Agents[acIdx].Tools, entry)
		} else {
			st.cfg.Agents[acIdx].Tools[idx].Enabled = &t
		}
	}
	if err := config.Save(st.cfg); err != nil {
		return fmt.Sprintf("%s enabled %q (config save failed: %v)", milkTag(), toolName, err)
	}
	return fmt.Sprintf("%s tool-agent %q enabled", milkTag(), toolName)
}

// execAgentToolDisable sets Enabled=false on a matching entry.
func execAgentToolDisable(toolName, scope string, st *interactiveState) string {
	f := false
	if scope == "global" {
		idx := findToolEntryIdx(st.cfg.AgentTools, toolName)
		if idx < 0 {
			return fmt.Sprintf("%s tool-agent %q not found in global list — use /agent tool add first", milkTag(), toolName)
		}
		st.cfg.AgentTools[idx].Enabled = &f
	} else {
		agentName := scope
		if agentName == "" {
			agentName = st.cfg.ActiveAgent().Name
		}
		acIdx := findAgentIdx(st.cfg, agentName)
		if acIdx < 0 {
			return fmt.Sprintf("%s agent %q not found", milkTag(), agentName)
		}
		idx := findToolEntryIdx(st.cfg.Agents[acIdx].Tools, toolName)
		if idx < 0 {
			// Check if it exists globally; create per-agent override that disables it.
			gIdx := findToolEntryIdx(st.cfg.AgentTools, toolName)
			if gIdx < 0 {
				return fmt.Sprintf("%s tool-agent %q not found — use /agent tool add first", milkTag(), toolName)
			}
			entry := st.cfg.AgentTools[gIdx]
			entry.Enabled = &f
			st.cfg.Agents[acIdx].Tools = append(st.cfg.Agents[acIdx].Tools, entry)
		} else {
			st.cfg.Agents[acIdx].Tools[idx].Enabled = &f
		}
	}
	if err := config.Save(st.cfg); err != nil {
		return fmt.Sprintf("%s disabled %q (config save failed: %v)", milkTag(), toolName, err)
	}
	return fmt.Sprintf("%s tool-agent %q disabled", milkTag(), toolName)
}

// execAgentToolAdd adds a new tool-agent entry to the target scope.
// The rest argument still contains the full "toolName [description=...] [for ...]" text
// so we can extract the description= field.
func execAgentToolAdd(toolName, scope, rest string, st *interactiveState) string {
	// Extract description= from rest.
	desc := ""
	if idx := strings.Index(rest, "description="); idx >= 0 {
		after := rest[idx+len("description="):]
		// strip any trailing " for ..." scope fragment.
		if forIdx := strings.Index(after, " for "); forIdx >= 0 {
			after = after[:forIdx]
		}
		desc = strings.TrimSpace(after)
	}
	if desc == "" {
		return milkTag() + " usage: /agent tool add <tool-agent> description=<desc> [for <agent>|global]"
	}

	entry := config.AgentToolEntry{Agent: toolName, Description: desc}

	if scope == "global" {
		if findToolEntryIdx(st.cfg.AgentTools, toolName) >= 0 {
			return fmt.Sprintf("%s tool-agent %q already exists in global list — use enable/disable to change its state", milkTag(), toolName)
		}
		st.cfg.AgentTools = append(st.cfg.AgentTools, entry)
	} else {
		agentName := scope
		if agentName == "" {
			agentName = st.cfg.ActiveAgent().Name
		}
		acIdx := findAgentIdx(st.cfg, agentName)
		if acIdx < 0 {
			return fmt.Sprintf("%s agent %q not found", milkTag(), agentName)
		}
		if findToolEntryIdx(st.cfg.Agents[acIdx].Tools, toolName) >= 0 {
			return fmt.Sprintf("%s tool-agent %q already exists for agent %q", milkTag(), toolName, agentName)
		}
		st.cfg.Agents[acIdx].Tools = append(st.cfg.Agents[acIdx].Tools, entry)
	}
	if err := config.Save(st.cfg); err != nil {
		return fmt.Sprintf("%s added tool-agent %q (config save failed: %v)", milkTag(), toolName, err)
	}
	return fmt.Sprintf("%s tool-agent %q added", milkTag(), toolName)
}

// execAgentToolRemove removes a tool-agent entry from the target scope.
func execAgentToolRemove(toolName, scope string, st *interactiveState) string {
	if scope == "global" {
		idx := findToolEntryIdx(st.cfg.AgentTools, toolName)
		if idx < 0 {
			return fmt.Sprintf("%s tool-agent %q not found in global list", milkTag(), toolName)
		}
		st.cfg.AgentTools = append(st.cfg.AgentTools[:idx], st.cfg.AgentTools[idx+1:]...)
	} else {
		agentName := scope
		if agentName == "" {
			agentName = st.cfg.ActiveAgent().Name
		}
		acIdx := findAgentIdx(st.cfg, agentName)
		if acIdx < 0 {
			return fmt.Sprintf("%s agent %q not found", milkTag(), agentName)
		}
		idx := findToolEntryIdx(st.cfg.Agents[acIdx].Tools, toolName)
		if idx < 0 {
			return fmt.Sprintf("%s tool-agent %q not found for agent %q", milkTag(), toolName, agentName)
		}
		st.cfg.Agents[acIdx].Tools = append(st.cfg.Agents[acIdx].Tools[:idx], st.cfg.Agents[acIdx].Tools[idx+1:]...)
	}
	if err := config.Save(st.cfg); err != nil {
		return fmt.Sprintf("%s removed tool-agent %q (config save failed: %v)", milkTag(), toolName, err)
	}
	return fmt.Sprintf("%s tool-agent %q removed", milkTag(), toolName)
}

// findToolEntryIdx returns the index of a tool entry by agent name in a slice,
// or -1 if not found. Comparison is case-insensitive.
func findToolEntryIdx(entries []config.AgentToolEntry, agentName string) int {
	lower := strings.ToLower(agentName)
	for i, e := range entries {
		if strings.ToLower(e.Agent) == lower {
			return i
		}
	}
	return -1
}

// findAgentIdx returns the index of an agent by name in cfg.Agents, or -1.
func findAgentIdx(cfg config.Config, agentName string) int {
	lower := strings.ToLower(agentName)
	for i, ac := range cfg.Agents {
		if strings.ToLower(ac.Name) == lower {
			return i
		}
	}
	return -1
}

// execMCP dispatches /mcp <verb> [args] subcommands.
func execMCP(sub string, st *interactiveState) string {
	parts := strings.Fields(sub)
	if len(parts) == 0 {
		return execMCPList("", st)
	}
	verb := parts[0]
	rest := strings.TrimSpace(strings.TrimPrefix(sub, verb))

	switch verb {
	case "list":
		return execMCPList(rest, st)
	case "add":
		return execMCPAdd(rest, st)
	case "remove":
		if rest == "" {
			return milkTag() + " usage: /mcp remove <name>"
		}
		return execMCPRemove(rest, st)
	case "enable":
		if rest == "" {
			return milkTag() + " usage: /mcp enable <name>"
		}
		return execMCPSetEnabled(rest, true, st)
	case "disable":
		if rest == "" {
			return milkTag() + " usage: /mcp disable <name>"
		}
		return execMCPSetEnabled(rest, false, st)
	case "tools":
		return execMCPTools(rest, st)
	case "assign":
		return execMCPAssign(rest, true, st)
	case "unassign":
		return execMCPAssign(rest, false, st)
	default:
		return milkTag() + " unknown subcommand: /mcp " + verb + "\n  try: list, add, remove, enable, disable, tools, assign, unassign"
	}
}

// execMCPList shows the configured MCP servers, optionally filtered to those
// visible by a given agent. When agentFilter is empty, shows all servers.
func execMCPList(agentFilter string, st *interactiveState) string {
	agentFilter = strings.TrimSpace(agentFilter)
	servers := st.cfg.MCPServers
	if len(servers) == 0 {
		return milkTag() + " no MCP servers configured — use /mcp add name=… url=… to add one"
	}

	// Build a lookup of which agents use which servers.
	agentForServer := map[string][]string{}
	for _, ac := range st.cfg.Agents {
		for _, sname := range ac.MCPServers {
			agentForServer[strings.ToLower(sname)] = append(agentForServer[strings.ToLower(sname)], ac.Name)
		}
	}

	var b strings.Builder
	if agentFilter != "" {
		effective := st.cfg.EffectiveMCPServers(agentFilter)
		if len(effective) == 0 {
			return fmt.Sprintf("%s no MCP servers configured for agent %q", milkTag(), agentFilter)
		}
		fmt.Fprintf(&b, "%s MCP servers for %q:\n", milkTag(), agentFilter)
		for _, s := range effective {
			writeMCPServerLine(&b, s, agentForServer)
		}
	} else {
		fmt.Fprintf(&b, "%s MCP servers (%d):\n", milkTag(), len(servers))
		for _, s := range servers {
			writeMCPServerLine(&b, s, agentForServer)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeMCPServerLine(b *strings.Builder, s config.MCPServerConfig, agentForServer map[string][]string) {
	status := green("enabled")
	if !s.IsEnabled() {
		status = dim("disabled")
	}
	auth := s.Auth
	if auth == "" {
		auth = "none"
	}
	agents := agentForServer[strings.ToLower(s.Name)]
	agentBadge := ""
	if len(agents) > 0 {
		agentBadge = "  [" + strings.Join(agents, ", ") + "]"
	}
	fmt.Fprintf(b, "  %-20s  %-10s  %-8s  %s%s\n", bold(s.Name), status, auth, s.URL, agentBadge)
}

// execMCPAdd adds a new MCP server entry from key=value args.
func execMCPAdd(rest string, st *interactiveState) string {
	fields := parseKVArgs(rest)
	name := fields["name"]
	url := fields["url"]
	if name == "" || url == "" {
		return milkTag() + " usage: /mcp add name=<name> url=<url> [transport=http|sse] [auth=bearer|token_cmd] [api_key=…] [timeout=30s]"
	}
	for _, s := range st.cfg.MCPServers {
		if strings.EqualFold(s.Name, name) {
			return fmt.Sprintf("%s MCP server %q already exists — use /mcp enable/disable or /mcp remove first", milkTag(), name)
		}
	}
	entry := config.MCPServerConfig{
		Name:      name,
		URL:       url,
		Transport: fields["transport"],
		Auth:      fields["auth"],
		APIKey:    fields["api_key"],
		TokenCmd:  fields["token_cmd"],
		Timeout:   fields["timeout"],
	}
	st.cfg.MCPServers = append(st.cfg.MCPServers, entry)
	if err := config.Save(st.cfg); err != nil {
		return fmt.Sprintf("%s added MCP server %q (config save failed: %v)", milkTag(), name, err)
	}
	return fmt.Sprintf("%s MCP server %q added — use /mcp assign %s for <agent> to expose it", milkTag(), name, name)
}

// execMCPRemove removes an MCP server by name, also cleaning up all agent references.
func execMCPRemove(name string, st *interactiveState) string {
	idx := findMCPServerIdx(st.cfg.MCPServers, name)
	if idx < 0 {
		return fmt.Sprintf("%s MCP server %q not found", milkTag(), name)
	}
	st.cfg.MCPServers = append(st.cfg.MCPServers[:idx], st.cfg.MCPServers[idx+1:]...)
	// Remove references from all agents.
	lower := strings.ToLower(name)
	for i, ac := range st.cfg.Agents {
		var kept []string
		for _, sname := range ac.MCPServers {
			if strings.ToLower(sname) != lower {
				kept = append(kept, sname)
			}
		}
		st.cfg.Agents[i].MCPServers = kept
	}
	if err := config.Save(st.cfg); err != nil {
		return fmt.Sprintf("%s removed MCP server %q (config save failed: %v)", milkTag(), name, err)
	}
	return fmt.Sprintf("%s MCP server %q removed", milkTag(), name)
}

// execMCPSetEnabled enables or disables an MCP server.
func execMCPSetEnabled(name string, enabled bool, st *interactiveState) string {
	idx := findMCPServerIdx(st.cfg.MCPServers, name)
	if idx < 0 {
		return fmt.Sprintf("%s MCP server %q not found", milkTag(), name)
	}
	st.cfg.MCPServers[idx].Enabled = &enabled
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	if err := config.Save(st.cfg); err != nil {
		return fmt.Sprintf("%s %s MCP server %q (config save failed: %v)", milkTag(), verb, name, err)
	}
	return fmt.Sprintf("%s MCP server %q %s", milkTag(), name, verb)
}

// execMCPTools connects to the named server (or all servers for the primary agent)
// and lists their available tools.
func execMCPTools(serverName string, st *interactiveState) string {
	serverName = strings.TrimSpace(serverName)
	var servers []config.MCPServerConfig
	if serverName != "" {
		idx := findMCPServerIdx(st.cfg.MCPServers, serverName)
		if idx < 0 {
			return fmt.Sprintf("%s MCP server %q not found", milkTag(), serverName)
		}
		if !st.cfg.MCPServers[idx].IsEnabled() {
			return fmt.Sprintf("%s MCP server %q is disabled", milkTag(), serverName)
		}
		servers = []config.MCPServerConfig{st.cfg.MCPServers[idx]}
	} else {
		servers = st.cfg.EffectiveMCPServers(st.cfg.ActiveAgent().Name)
		if len(servers) == 0 {
			return milkTag() + " no MCP servers configured for primary agent — specify a server name or use /mcp list"
		}
	}

	var b strings.Builder
	ctx := context.Background()
	for _, s := range servers {
		fmt.Fprintf(&b, "%s %s tools:\n", milkTag(), bold(s.Name))
		c := mcpClientForConfig(s)
		if err := c.Connect(ctx); err != nil {
			fmt.Fprintf(&b, "  error: %v\n", err)
			continue
		}
		defer c.Close(ctx)
		tools := c.Tools()
		if len(tools) == 0 {
			fmt.Fprintf(&b, "  (no tools)\n")
			continue
		}
		for _, t := range tools {
			desc := t.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			fmt.Fprintf(&b, "  %-30s  %s\n", t.Name, desc)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// execMCPAssign adds or removes an MCP server reference from an agent's mcp_servers list.
// rest is "<server-name> for <agent-name>".
func execMCPAssign(rest string, assign bool, st *interactiveState) string {
	verb := "assign"
	if !assign {
		verb = "unassign"
	}
	serverName, agentName, ok := parseMCPAssignArgs(rest)
	if !ok {
		return fmt.Sprintf("%s usage: /mcp %s <server> for <agent>", milkTag(), verb)
	}
	if findMCPServerIdx(st.cfg.MCPServers, serverName) < 0 {
		return fmt.Sprintf("%s MCP server %q not found — add it first with /mcp add", milkTag(), serverName)
	}
	acIdx := findAgentIdx(st.cfg, agentName)
	if acIdx < 0 {
		return fmt.Sprintf("%s agent %q not found", milkTag(), agentName)
	}
	lower := strings.ToLower(serverName)
	existing := st.cfg.Agents[acIdx].MCPServers
	if assign {
		for _, sn := range existing {
			if strings.ToLower(sn) == lower {
				return fmt.Sprintf("%s MCP server %q already assigned to agent %q", milkTag(), serverName, agentName)
			}
		}
		st.cfg.Agents[acIdx].MCPServers = append(existing, serverName)
	} else {
		var kept []string
		found := false
		for _, sn := range existing {
			if strings.ToLower(sn) == lower {
				found = true
			} else {
				kept = append(kept, sn)
			}
		}
		if !found {
			return fmt.Sprintf("%s MCP server %q not assigned to agent %q", milkTag(), serverName, agentName)
		}
		st.cfg.Agents[acIdx].MCPServers = kept
	}
	if err := config.Save(st.cfg); err != nil {
		return fmt.Sprintf("%s %sed %q for agent %q (config save failed: %v)", milkTag(), verb, serverName, agentName, err)
	}
	if assign {
		return fmt.Sprintf("%s MCP server %q assigned to agent %q — restart milk to connect", milkTag(), serverName, agentName)
	}
	return fmt.Sprintf("%s MCP server %q unassigned from agent %q — restart milk to disconnect", milkTag(), serverName, agentName)
}

// parseMCPAssignArgs parses "<server> for <agent>" from the rest string.
func parseMCPAssignArgs(rest string) (serverName, agentName string, ok bool) {
	idx := strings.Index(rest, " for ")
	if idx < 0 {
		return "", "", false
	}
	serverName = strings.TrimSpace(rest[:idx])
	agentName = strings.TrimSpace(rest[idx+5:])
	return serverName, agentName, serverName != "" && agentName != ""
}

// mcpClientForConfig builds an mcp.Client from an MCPServerConfig.
func mcpClientForConfig(s config.MCPServerConfig) *mcp.Client {
	return mcp.New(s)
}

// findMCPServerIdx returns the index of an MCP server by name, or -1.
func findMCPServerIdx(servers []config.MCPServerConfig, name string) int {
	lower := strings.ToLower(name)
	for i, s := range servers {
		if strings.ToLower(s.Name) == lower {
			return i
		}
	}
	return -1
}

// parseKVArgs parses "key=value key2=value2 …" into a map. Values may be
// quoted with double quotes. Unrecognised tokens (no "=") are skipped.
func parseKVArgs(s string) map[string]string {
	result := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			break
		}
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+1:]
		var val string
		if len(s) > 0 && s[0] == '"' {
			// Quoted value: scan to closing quote.
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				val = s[1:]
				s = ""
			} else {
				val = s[1 : end+1]
				s = s[end+2:]
			}
		} else {
			// Unquoted: read until next whitespace.
			sp := strings.IndexAny(s, " \t")
			if sp < 0 {
				val = s
				s = ""
			} else {
				val = s[:sp]
				s = s[sp:]
			}
		}
		if key != "" {
			result[key] = val
		}
	}
	return result
}

// splitCommaNames splits a comma-separated string into trimmed, non-empty names.
func splitCommaNames(s string) []string {
	var result []string
	for _, part := range strings.Split(s, ",") {
		name := strings.TrimSpace(part)
		if name != "" {
			result = append(result, name)
		}
	}
	return result
}
