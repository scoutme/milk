package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/claudesettings"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/mcp"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
)

const cmdAttach = "/attach"
const cmdWorkflow = "/workflow"
const cmdReload = "/reload"
const cmdTasks = "/tasks"
const cmdTask = "/task"
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
const cmdPermissions = "/permissions"
const cmdAgent = "/agent"
const cmdColorize = "/colorize"
const cmdThink = "/think"
const cmdTips = "/tips"
const cmdTheme = "/theme"
const cmdRestart = "/restart"
const cmdInterrupt = "/interrupt"
const cmdSetup = "/setup"
const cmdConfig = "/config"
const cmdInit = "/init"
const cmdOpen = "/open"
const cmdMCP = "/mcp"
const cmdUpdate = "/update"
const cmdServer = "/server"
const cmdBash = "/bash"
const cmdBg = "/bg"
const cmdNotifications = "/notifications"
const cmdKeys = "/keys"

var slashCommands = registryNames()

// initWizardState tracks state for the /config init TUI wizard.
type initWizardState struct {
	step    initWizardStep
	primary config.AgentConfig
	escCLI  bool // whether to use default claude-cli escalation
	// cwd is the directory the wizard resolves project-local config against
	// — the host's currently selected directory (activeConfigDir); "" = the
	// process cwd. Set by initWizardStart, so a sandboxed/foreign session
	// dir writes .milk/config.json in the right place.
	cwd string
	// limits step
	contextWindowTokens int // context_window_tokens (0 = not set); message_budget_chars/
	// max_tool_iterations are auto-derived from this by AgentMessageBudget/
	// AgentContextWindowTokens, not asked separately.
	// agent-tools step
	toolAgentNames []string // agent names the user wants to enable as tools
}

type initWizardStep int

const (
	initStepName       initWizardStep = iota // ask agent name
	initStepProvider                         // ask provider (menu 1–6)
	initStepURL                              // ask URL (providers that need it)
	initStepChatPath                         // ask chat_path (bearer only, skip if standard)
	initStepRunCmd                           // ask run_cmd (local provider only, optional)
	initStepModel                            // ask model
	initStepAuth                             // ask api_key (blank → go to initStepTokenCmd)
	initStepTokenCmd                         // ask token_cmd
	initStepAWSRegion                        // ask aws_region (bedrock only)
	initStepLimits                           // ask context_window_tokens (proposes a models.dev catalog match)
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

var interactiveHelp = buildInteractiveHelp()

// renderHelp applies consistent ANSI styling to the interactiveHelp block:
// section header lines (── … ──) are dimmed and padded to width columns,
// command tokens are bolded. Pass width=0 to use a default of 80.
func renderHelp(s string, width int) string {
	if width <= 0 || width > 80 {
		width = 80
	}
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(line, "──"):
			// Section header: rebuild trailing dashes to fit exactly width cols.
			// "── Name ─…─" → prefix "── Name ", then fill with fresh dashes.
			runes := []rune(line)
			end := len(runes)
			for end > 0 && runes[end-1] == '─' {
				end--
			}
			prefix := string(runes[:end]) // e.g. "── Routing "
			// All chars in prefix are ASCII (1 col) or '─' (1 col), so len(runes[:end]) == prefixCols.
			fill := width - end
			if fill < 1 {
				fill = 1
			}
			b.WriteString(dim(prefix + strings.Repeat("─", fill)))
		case len(line) >= 2 && line[0] == ' ' && line[1] == ' ':
			// Command or detail line: style slash-command lines.
			trimmed := strings.TrimLeft(line, " ")
			if strings.HasPrefix(trimmed, "/") {
				indent := line[:len(line)-len(trimmed)]
				b.WriteString(indent)
				// Emit the /command token in bold-gold.
				rest := trimmed
				sp := strings.IndexByte(rest, ' ')
				if sp < 0 {
					b.WriteString(boldYellow(rest))
					b.WriteByte('\n')
					continue
				}
				b.WriteString(boldYellow(rest[:sp]))
				rest = rest[sp:] // leading space(s) + remainder
				// Bold any immediately-following subcommand words (lowercase alpha only).
				// Multiple leading spaces signal description-column padding — stop there.
				for len(rest) > 0 && rest[0] == ' ' {
					if len(rest) > 1 && rest[1] == ' ' {
						break // padding gap, not a subcommand
					}
					next := rest[1:] // strip the single space
					wordEnd := strings.IndexByte(next, ' ')
					var word string
					if wordEnd < 0 {
						word = next
					} else {
						word = next[:wordEnd]
					}
					if isSubcmd(word) {
						b.WriteByte(' ')
						b.WriteString(bold(word))
						if wordEnd < 0 {
							rest = ""
						} else {
							rest = next[wordEnd:]
						}
					} else {
						break
					}
				}
				b.WriteString(rest)
			} else {
				b.WriteString(line)
			}
		default:
			b.WriteString(line)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// isSubcmd reports whether word is a bare subcommand token (lowercase letters only).
// Used by renderHelp to decide which tokens after the /command get bold treatment.
func isSubcmd(word string) bool {
	if word == "" {
		return false
	}
	for _, ch := range word {
		if ch < 'a' || ch > 'z' {
			return false
		}
	}
	return true
}

const errFmt = "error: %v\n"

// interactiveState holds mutable state for the interactive loop.
type interactiveState struct {
	sess *session.Session
	// claudeApproved records that the user allowed the implicit Claude CLI for
	// this session (fallback_core.go).
	claudeApproved bool
	forceEscalate  bool
	forcePrimary   bool
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
	toolFutures map[string]chan string
	// skipPermissions is the session-level override for
	// DangerouslySkipPermissions. Atomic: running turns read it from agent
	// goroutines while /skip-permissions (busy-safe) writes it from the UI one.
	skipPermissions atomic.Bool
	// permOverrides are the session-level overrides for the timed answer
	// (ADR-0052, /permissions default|timeout), guarded by permMu for the same
	// reason: background asks read them mid-turn. Use permOverridesNow /
	// updatePermOverrides.
	permMu        sync.RWMutex
	permOverrides permOverrides
	localPerms    *local.PermStore // persisted tool grants for the primary local agent
	notifier      oversight.Notifier

	// pendingRemoteInputs queues messages received from the remote oversight
	// interface (e.g. Telegram) while a turn is in progress. Drained one at a
	// time by handleAgentDone once the current turn completes.
	pendingRemoteInputs []string

	// lastEscalationContextHash is a short hash of the last per-turn context block
	// prepended to a resumed CLI escalation prompt. Used to avoid appending the same
	// block to the conversation twice in a row.
	lastEscalationContextHash string

	// pendingSessionContent overrides the user-turn content recorded in session history
	// for the next dispatch. When non-empty, runPrimary/runEscalation use this string
	// instead of the raw prompt. Reset to "" after each turn. Used to store compact
	// attachment placeholders in history instead of full file content.
	pendingSessionContent string

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

// extractSlashCommand recognizes a known slash command only as the leading
// token of the input — never mid-sentence or mid-paste — so mentioning a
// command by name elsewhere in a prompt (typed or pasted) is inert text.
// Returns the command, the remaining text with the token stripped, and
// whether a command was found.
func extractSlashCommand(input string) (cmd, rest string, found bool) {
	words := strings.Fields(input)
	if len(words) == 0 || !strings.HasPrefix(words[0], "/") || !slices.Contains(slashCommands, words[0]) {
		return "", input, false
	}
	return words[0], strings.Join(words[1:], " "), true
}

// promptFriendly is the set of slash commands that can be combined with a prompt.
var promptFriendly = map[string]bool{
	cmdEscalate: true,
	cmdPrimary:  true,
	cmdLearn:    true,
	cmdOtel:     true,
}

// busySafeCommands are slash commands that can run while an agent turn is in progress.
// All are read-only or display-only and never dispatch a new agent turn
// (except /bg start, which spawns a background job — the same thing Ctrl+Enter
// already does while busy — and /bg stop, which is an interrupt-style action).
var busySafeCommands = map[string]bool{
	"/help":          true,
	cmdThink:         true,
	cmdTips:          true,
	cmdTheme:         true,
	cmdColorize:      true,
	cmdPanel:         true,
	cmdHistory:       true,
	cmdNotifications: true,
	cmdKeys:          true,
	cmdInterrupt:     true,
	cmdMemory:        true,
	cmdUsage:         true,
	cmdMetrics:       true,
	cmdExport:        true,
	cmdPaste:         true,
	cmdMCP:           true,
	cmdBg:            true,
	// The permission family acts on session-level settings behind locks/atomics
	// and dispatches nothing: a user waiting on (or about to be asked by) a
	// running turn is exactly who needs them.
	cmdPermissions: true,
	cmdSkipPerms:   true,
}

// handleSlashCommand processes a slash command with optional surrounding prompt text.
// Returns (exit, prompt-to-dispatch, output): exit=true means quit the loop,
// prompt is non-empty when the command should be followed by an immediate dispatch,
// output is text to print via tea.Println.
func handleSlashCommand(cmd, prompt string, st *interactiveState) (exit bool, dispatch, output string) {
	switch cmd {
	case "/exit", "/quit":
		return true, "", ""
	case "/help", cmdPaste:
		output = execNonPromptCmd(cmd, prompt, st)
	case cmdNew, cmdClear, cmdDrop, cmdSessions, cmdResume, cmdListLegacy:
		output = execSessionCmd(cmd, prompt, st)
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
		if st.sess != nil {
			// Use total user-turn count, not local-only: sessionToUnifiedMessages
			// includes all user turns, so the skip window must match.
			st.sess.RepetitionBaselineLocalTurns = st.sess.UserTurnCount()
			// Reset ESCALATION_WAITING state when explicitly switching to primary.
			if st.sess.State == session.StateEscalationWaiting {
				st.sess.ForceState(session.StateRouting)
			}
		}
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
	case cmdBash:
		output = execBash(prompt, st)
	case cmdSkipPerms:
		output = execSkipPerms(prompt, st)
	case cmdPermissions:
		// The listing shows the model's pending prompt queue, so the TUI
		// intercepts this in handleSlashInput (commands.go) before it
		// reaches here. Guard to prevent "unknown command" output, mirroring
		// cmdNotifications.
	case cmdThink:
		// /think is handled in handleSlashInput (commands.go) where it can
		// reach the model's think-block fold state. This case is a no-op
		// here; the TUI intercepts cmdThink before it reaches
		// handleSlashCommand. Guard to prevent "unknown command" output.
	case cmdKeys:
		// Rendered by the TUI in handleSlashInput (commands.go); no-op here.
	case cmdNotifications:
		// History lives on the TUI model; the TUI intercepts this in
		// handleSlashInput (commands.go) before it reaches here. Guard to
		// prevent "unknown command" output, mirroring cmdThink above.
	case cmdSetup:
		// Handled in repl.go (needs model state). No-op here.
	case cmdConfig:
		// Handled in repl.go (needs model state and config path). No-op here.
	case cmdInit:
		// /init is an alias for /config init; the TUI intercepts it in
		// handleSlashInput (commands.go) before it reaches here. Guard to
		// prevent "unknown command" output.
	case cmdOpen:
		// Handled in repl.go (needs tea.ExecProcess). No-op here.
	case cmdAttach:
		// Handled in commands.go (handleAttachCmd) where model state is available.
	case cmdMCP:
		// Handled in commands.go (handleMCPCmd) where model state is available for wizards.
	case cmdUpdate:
		// Handled in repl.go (needs model state for install dispatch). No-op here.
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
		fmt.Fprint(&out, renderHelp(interactiveHelp, 0))
	case cmdPaste:
		fmt.Fprint(&out, milkTag()+" probing clipboard for non-text content (image, PDF, …)")
	}
	return out.String()
}

// execSessionCmd runs the session-management surface (plan D1) in the TUI:
// /sessions [all], /new [name], /clear [name] (alias of /new), /resume <ref>,
// /drop [<ref>] and the deprecated /list alias. The semantics live in the
// host-neutral core (sessionmgmt_core.go); this wrapper only adopts the op's
// binding — handleSlashInput's refreshSessionScopedState notices the st.sess
// swap and rebuilds session-scoped state (and, for /resume, reseeds the
// transcript so the view matches the binding).
func execSessionCmd(cmd, args string, st *interactiveState) string {
	args = strings.TrimSpace(args)
	currentID := ""
	if st.sess != nil {
		currentID = st.sess.ID
	}
	var res sessionOpResult
	switch cmd {
	case cmdNew, cmdClear:
		res = opSessionNew(st.cwd, args)
	case cmdResume:
		if args == "" {
			listing, err := sessionListText(st.cwd, currentID, false)
			if err != nil {
				return fmt.Sprintf(errFmt, err)
			}
			return fmt.Sprintf("%s usage: /resume <%s>\n\n%s", milkTag(), sessionRefHint, listing)
		}
		res = opSessionResume(st.sess, st.cwd, args)
	case cmdDrop:
		res = opSessionDrop(st.sess, st.cwd, args)
	case cmdSessions, cmdListLegacy:
		all := strings.EqualFold(args, "all")
		if args != "" && !all {
			return milkTag() + " usage: /sessions [all]"
		}
		listing, err := sessionListText(st.cwd, currentID, all)
		if err != nil {
			return fmt.Sprintf(errFmt, err)
		}
		if cmd == cmdListLegacy {
			return milkTag() + " /list is deprecated — use /sessions\n\n" + listing
		}
		return listing
	}
	if res.err != nil {
		return renderOpResult(res, args)
	}
	if res.bind != nil {
		st.sess = res.bind
		obs.ResetSessionTokens()
	}
	return res.out
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
		for _, u := range st.sess.TokensSnapshot() {
			sessEntries = append(sessEntries, obs.SessionTokenEntry{
				Model: u.Model, Agent: u.Agent,
				Prompt: u.Prompt, Completion: u.Completion,
				CacheRead: u.CacheRead, CacheCreation: u.CacheCreation,
				DecodeSeconds: u.DecodeSeconds, TTFTSeconds: u.TTFTSeconds, Requests: u.Requests,
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
		st.cfg.DebugSubprocessLog = true
		cliPath, _ := config.CLIDebugLogPath()
		localPath, _ := config.LocalDebugLogPath()
		subprocessPath, _ := config.SubprocessDebugLogPath()
		return milkTag() + " debug logging enabled\n" +
			"  claude NDJSON → " + cliPath + "\n" +
			"  local SSE     → " + localPath + "\n" +
			"  subprocess    → " + subprocessPath + "\n" +
			"  payloads      → " + otelDir + "/logs.jsonl"
	case "debug disable":
		if err := runOtelDebug(false); err != nil {
			return fmt.Sprintf("%s error: %v", milkTag(), err)
		}
		st.cfg.Otel.LogContext = false
		st.cfg.Otel.LogLevel = "INFO" // in-memory reset; disk already restored by runOtelDebug
		st.cfg.DebugCLILog = false
		st.cfg.DebugLocalLog = false
		st.cfg.DebugSubprocessLog = false
		return milkTag() + " debug logging disabled"
	default:
		return milkTag() + " " + obs.FormatStats(otelDir)
	}
}

// execExport dumps a session as text or JSON, optionally to a file. Grammar
// (docs/internal/plans/acp-session-resume-plan.md D6):
// `/export [--no-reasoning] [session <id|prefix>] [json|<path>]` — with a
// `session` target it exports *that* session without attaching it (the
// /list → /export session a1b2 → session/resume preview flow); without one
// it exports the current session (on a resumed session that includes
// pre-resume turns). `--no-reasoning` (or bare `no-reasoning`) anywhere in
// the arguments strips reasoning from the output (issue #277); without the
// flag the argument string is left byte-for-byte intact so paths keep
// spaces and odd whitespace exactly as typed.
func execExport(sub string, st *interactiveState) string {
	sub = strings.TrimSpace(sub)
	opts := session.ExportOptions{}
	if fields := strings.Fields(sub); slices.Contains(fields, "--no-reasoning") || slices.Contains(fields, "no-reasoning") {
		kept := fields[:0]
		for _, tok := range fields {
			if tok == "--no-reasoning" || tok == "no-reasoning" {
				opts.NoReasoning = true
				continue
			}
			kept = append(kept, tok)
		}
		sub = strings.Join(kept, " ")
	}
	sess := st.sess
	if sub == "session" || strings.HasPrefix(sub, "session ") {
		id, after, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(sub, "session")), " ")
		if id == "" {
			return milkTag() + " usage: /export [--no-reasoning] [session <id|prefix>] [json|<path>]"
		}
		target, err := session.Lookup(id) // exact ID or unambiguous prefix
		if err != nil {
			return fmt.Sprintf("%s export error: %v", milkTag(), err)
		}
		sess = target
		sub = strings.TrimSpace(after)
	}
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
		data, err := session.ExportJSONWithOptions(sess, opts)
		if err != nil {
			return fmt.Sprintf("%s export error: %v", milkTag(), err)
		}
		content = string(data)
	default:
		if outputPath != "" {
			content = session.ExportTextWithOptions(sess, opts) // plain — no ANSI in files
		} else {
			content = session.ExportTextColorizedWithOptions(sess, opts) // colorized for terminal
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
	switch {
	case sub == "stale":
		return execMemoryStale(st)
	case sub == "archive":
		return execMemoryArchive(st)
	case sub == "restore":
		return milkTag() + " usage: /memory restore <id|prefix> (ids are listed by /memory archive)"
	}
	if rest, ok := strings.CutPrefix(sub, "restore "); ok {
		return execMemoryRestore(strings.TrimSpace(rest), st)
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

// execMemoryStale lists the global percepts by how long they have gone unused,
// i.e. which ones the next consolidations will archive and when.
func execMemoryStale(st *interactiveState) string {
	entries := st.mem.Stale()
	if len(entries) == 0 {
		return milkTag() + " no non-core global percepts to age (or archiving is disabled: global_archive_after_days < 0)"
	}
	var b strings.Builder
	b.WriteString(milkTag() + " global percepts by idle session time (archived once decayed to the prune weight and idle for the limit):\n")
	for i, e := range entries {
		if i >= 25 {
			fmt.Fprintf(&b, "  … %d more\n", len(entries)-i)
			break
		}
		state := fmt.Sprintf("%.1f days left", e.DaysLeft)
		switch {
		case e.DaysLeft <= 0 && e.AtFloor:
			state = "due for archiving"
		case e.DaysLeft <= 0:
			state = "idle limit reached, still above the prune weight"
		case !e.AtFloor:
			state += ", weight still above the prune weight"
		}
		fmt.Fprintf(&b, "  #%.8s  idle %5.1f d  %s  %s\n", e.Percept.ID, e.IdleDays, state, truncate(e.Percept.Content, 70))
	}
	return strings.TrimRight(b.String(), "\n")
}

// execMemoryArchive lists the archived global percepts.
func execMemoryArchive(st *interactiveState) string {
	list, err := st.mem.Archived()
	if err != nil {
		return fmt.Sprintf("%s error reading the archive: %v", milkTag(), err)
	}
	if len(list) == 0 {
		return milkTag() + " the archive is empty"
	}
	var b strings.Builder
	b.WriteString(milkTag() + " archived global percepts (restore with /memory restore <id>):\n")
	for i, a := range list {
		if i >= 25 {
			fmt.Fprintf(&b, "  … %d more (see global-archive.jsonl)\n", len(list)-i)
			break
		}
		fmt.Fprintf(&b, "  #%.8s  %s  idle %.1f d  %s\n", a.Percept.ID, a.ArchivedAt.Format("2006-01-02"), a.IdleDays, truncate(a.Percept.Content, 70))
	}
	return strings.TrimRight(b.String(), "\n")
}

func execMemoryRestore(prefix string, st *interactiveState) string {
	p, err := st.mem.Restore(strings.TrimPrefix(prefix, "#"))
	if err != nil {
		return fmt.Sprintf("%s cannot restore: %v", milkTag(), err)
	}
	return fmt.Sprintf("%s restored #%.8s to the global store: %s", milkTag(), p.ID, truncate(p.Content, 80))
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
	out, err := st.mem.RecordGlobal(context.Background(), fact, memory.ProducerUser, memory.ConsumerAll, memory.Roles{})
	if out.Reinforced {
		return fmt.Sprintf("%s already known — reinforced (%.0f%% overlap, w=%.2f): %q (#%s)",
			milkTag(), out.Similarity*100, out.W, out.Existing, out.ID[:8])
	}
	if err != nil {
		return fmt.Sprintf("%s error storing memory: %v", milkTag(), err)
	}
	return fmt.Sprintf("%s learned: %q (id %s)", milkTag(), fact, out.ID[:8])
}

// execSkipPerms handles /skip-permissions [on|off].
// execSkipPerms is /skip-permissions over the TUI: the shared grammar
// (execSkipPermsCore), applied to the session flag and styled for the
// transcript. A real toggle also clears the session allow-all/deny-all mode
// (issue #302) so the two "stop asking" states can't stack.
func execSkipPerms(sub string, st *interactiveState) string {
	next, msg, changed := execSkipPermsCore(sub, st.skipPermissions.Load())
	st.skipPermissions.Store(next)
	if changed {
		st.permMu.Lock()
		msg += clearSessionPermMode(&st.permOverrides)
		st.permMu.Unlock()
	}
	if next {
		return milkTag() + " " + red(msg)
	}
	return milkTag() + " " + msg
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
	if err := saveLocalOrGlobal(st.cfg, activeConfigDir(st)); err != nil {
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

// execAgentTool dispatches /agent tool <verb> [args] subcommands.
func execAgentTool(sub string, st *interactiveState) string {
	parts := strings.Fields(sub)
	if len(parts) == 0 {
		return execAgentToolList("", st)
	}
	verb := parts[0]
	rest := strings.TrimSpace(strings.TrimPrefix(sub, verb))

	// parse optional "description=<desc>" and "for <agent>|global" segments
	toolName, desc, scope := parseAgentToolArgs(rest)

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
		if toolName == "" || desc == "" {
			return milkTag() + " usage: /agent tool add <tool-agent> description=<desc> [for <agent>|global]"
		}
		return execAgentToolAdd(toolName, desc, scope, st)
	case "remove":
		if toolName == "" {
			return milkTag() + " usage: /agent tool remove <tool-agent> [for <agent>|global]"
		}
		return execAgentToolRemove(toolName, scope, st)
	default:
		return milkTag() + " unknown subcommand: /agent tool " + verb + "\n  try: list, enable, disable, add, remove"
	}
}

// execAgentToolApply runs one /agent tool mutation from wizard-collected
// parameters — already parsed, so nothing is re-parsed from raw text.
func execAgentToolApply(verb, toolName, desc, scope string, st *interactiveState) string {
	switch verb {
	case "add":
		return execAgentToolAdd(toolName, desc, scope, st)
	case "enable":
		return execAgentToolEnable(toolName, scope, st)
	case "disable":
		return execAgentToolDisable(toolName, scope, st)
	default:
		return execAgentToolRemove(toolName, scope, st)
	}
}

// splitQuotedTokens splits s into whitespace-separated tokens, keeping
// double- or single-quoted segments together (quote characters stripped) so a
// value may contain spaces and reserved words such as "for". A quote that is
// never closed consumes the rest of the string.
func splitQuotedTokens(s string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote := rune(0)
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case inQuote != 0:
			if r == inQuote {
				inQuote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			inQuote = r
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}

// parseAgentToolArgs parses "<tool-agent> [description=<desc>] [for <agent>|global]"
// and returns (toolName, desc, scope). desc and scope are "" when absent; scope
// is "global", an agent name, a comma-separated list, or "" (default: active
// primary agent).
//
// Parsing is quote-aware (#… follow-up): a value wrapped in double or single
// quotes stays one token, so description="coding agent for specific tasks"
// does not split at the "for" inside the quotes. The scope separator is the
// LAST standalone "for" token (trailing per the documented syntax), so an
// unquoted description may itself contain the word "for" as long as a scope
// follows it.
func parseAgentToolArgs(s string) (toolName, desc, scope string) {
	tokens := splitQuotedTokens(s)
	// Scope: everything after the last standalone "for" token.
	for i := len(tokens) - 2; i >= 0; i-- {
		if tokens[i] == "for" {
			scope = strings.TrimSpace(strings.Join(tokens[i+1:], " "))
			tokens = tokens[:i]
			break
		}
	}
	// Name is everything before the description= key; desc is that key's value
	// plus any following tokens (unquoted multi-word descriptions), so the name
	// never absorbs the description text (#165 follow-up).
	for i, tok := range tokens {
		if v, ok := strings.CutPrefix(tok, "description="); ok {
			desc = strings.TrimSpace(strings.Join(append([]string{v}, tokens[i+1:]...), " "))
			tokens = tokens[:i]
			break
		}
	}
	toolName = strings.TrimSpace(strings.Join(tokens, " "))
	return toolName, desc, scope
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

// --- multi-value scope handling (#165) ---
//
// /agent tool enable|disable|add|remove accept a comma-separated agent list in
// their "for <agent>|global" scope (and /mcp assign|unassign on both sides of
// "for"). Validation is all-or-nothing: every scope and name is checked before
// any config mutation, so a typo in one item rejects the whole call with a
// per-item report and changes nothing — a multi-target change is never left
// half-applied without saying so.

// resolveToolScopes expands a "for <agent>|global" segment into concrete
// scopes ("global", or an agent name — the empty segment resolves to the
// active primary agent). Multi-value form: comma-separated agent names;
// "global" is a scope, not a name, and cannot appear in a list. Returns one
// line per invalid item — when non-empty, nothing may be applied.
func resolveToolScopes(seg string, cfg config.Config) (scopes []string, invalid []string) {
	items := splitCommaNames(seg)
	if len(items) == 0 {
		items = []string{""}
	}
	multi := len(items) > 1
	seen := map[string]bool{}
	for _, it := range items {
		if strings.EqualFold(it, "global") {
			if multi {
				invalid = append(invalid, fmt.Sprintf("%s %q is a scope, not an agent name — cannot be combined in a list", milkTag(), it))
				continue
			}
			scopes = append(scopes, "global")
			continue
		}
		name := it
		if name == "" {
			name = cfg.ActiveAgent().Name
		}
		if seen[strings.ToLower(name)] {
			continue // dedupe repeated names: "a,a" acts once
		}
		seen[strings.ToLower(name)] = true
		if findAgentIdx(cfg, name) < 0 {
			invalid = append(invalid, fmt.Sprintf("%s agent %q not found", milkTag(), name))
			continue
		}
		scopes = append(scopes, name)
	}
	return scopes, invalid
}

// toolScopeOp is one /agent tool mutation split into per-scope validation
// (check) and per-scope application (apply), so runToolScopeOp can validate
// every scope of a multi-value list before touching config.
type toolScopeOp struct {
	past  string // "enabled", "added", … — used in outcome/save-failure lines
	check func(toolName, scope string, st *interactiveState) string
	apply func(toolName, scope string, st *interactiveState) string
}

// runToolScopeOp validates then applies op across every scope in the (possibly
// comma-separated) scopeSeg with all-or-nothing semantics, saving the config
// once at the end.
func runToolScopeOp(toolName, scopeSeg string, st *interactiveState, op toolScopeOp) string {
	scopes, invalid := resolveToolScopes(scopeSeg, st.cfg)
	for _, sc := range scopes {
		if msg := op.check(toolName, sc, st); msg != "" {
			invalid = append(invalid, msg)
		}
	}
	if len(invalid) > 0 {
		return strings.Join(invalid, "\n") + "\n" + milkTag() + " nothing applied"
	}
	var msgs []string
	for _, sc := range scopes {
		msgs = append(msgs, op.apply(toolName, sc, st))
	}
	if err := saveLocalOrGlobal(st.cfg, activeConfigDir(st)); err != nil {
		msgs = append(msgs, fmt.Sprintf("%s %s tool-agent %q (config save failed: %v)", milkTag(), op.past, toolName, err))
	}
	return strings.Join(msgs, "\n")
}

// toolScopeMsg formats the per-scope outcome line of a tool-agent mutation.
func toolScopeMsg(past, toolName, scope string) string {
	if scope == "global" {
		return fmt.Sprintf("%s tool-agent %q %s", milkTag(), toolName, past)
	}
	return fmt.Sprintf("%s tool-agent %q %s for agent %q", milkTag(), toolName, past, scope)
}

// checkToolEntryExists validates that toolName exists in the scope (in the
// global list, or as a global entry / per-agent override for agent scopes) —
// used by enable/disable.
func checkToolEntryExists(toolName, scope string, st *interactiveState) string {
	if scope == "global" {
		if findToolEntryIdx(st.cfg.AgentTools, toolName) < 0 {
			return fmt.Sprintf("%s tool-agent %q not found in global list — use /agent tool add first", milkTag(), toolName)
		}
		return ""
	}
	acIdx := findAgentIdx(st.cfg, scope)
	if findToolEntryIdx(st.cfg.Agents[acIdx].Tools, toolName) < 0 &&
		findToolEntryIdx(st.cfg.AgentTools, toolName) < 0 {
		return fmt.Sprintf("%s tool-agent %q not found — use /agent tool add first", milkTag(), toolName)
	}
	return ""
}

// checkToolEntryAbsent validates that toolName does not exist yet in the
// scope — used by add.
func checkToolEntryAbsent(toolName, scope string, st *interactiveState) string {
	if scope == "global" {
		if findToolEntryIdx(st.cfg.AgentTools, toolName) >= 0 {
			return fmt.Sprintf("%s tool-agent %q already exists in global list — use enable/disable to change its state", milkTag(), toolName)
		}
		return ""
	}
	acIdx := findAgentIdx(st.cfg, scope)
	if findToolEntryIdx(st.cfg.Agents[acIdx].Tools, toolName) >= 0 {
		return fmt.Sprintf("%s tool-agent %q already exists for agent %q", milkTag(), toolName, scope)
	}
	return ""
}

// checkToolEntryInScope validates that toolName exists as an entry in exactly
// the given scope — used by remove.
func checkToolEntryInScope(toolName, scope string, st *interactiveState) string {
	if scope == "global" {
		if findToolEntryIdx(st.cfg.AgentTools, toolName) < 0 {
			return fmt.Sprintf("%s tool-agent %q not found in global list", milkTag(), toolName)
		}
		return ""
	}
	acIdx := findAgentIdx(st.cfg, scope)
	if findToolEntryIdx(st.cfg.Agents[acIdx].Tools, toolName) < 0 {
		return fmt.Sprintf("%s tool-agent %q not found for agent %q", milkTag(), toolName, scope)
	}
	return ""
}

// setToolEnabled flips the Enabled flag for toolName in one scope, creating a
// per-agent override from the global entry when the agent has none yet.
// Callers must validate first (checkToolEntryExists).
func setToolEnabled(toolName, scope string, st *interactiveState, val *bool) {
	if scope == "global" {
		st.cfg.AgentTools[findToolEntryIdx(st.cfg.AgentTools, toolName)].Enabled = val
		return
	}
	acIdx := findAgentIdx(st.cfg, scope)
	idx := findToolEntryIdx(st.cfg.Agents[acIdx].Tools, toolName)
	if idx < 0 {
		entry := st.cfg.AgentTools[findToolEntryIdx(st.cfg.AgentTools, toolName)]
		entry.Enabled = val
		st.cfg.Agents[acIdx].Tools = append(st.cfg.Agents[acIdx].Tools, entry)
		return
	}
	st.cfg.Agents[acIdx].Tools[idx].Enabled = val
}

// execAgentToolEnable sets Enabled=true on matching entries in every scope.
func execAgentToolEnable(toolName, scope string, st *interactiveState) string {
	return runToolScopeOp(toolName, scope, st, toolScopeOp{
		past:  "enabled",
		check: checkToolEntryExists,
		apply: func(toolName, scope string, st *interactiveState) string {
			t := true
			setToolEnabled(toolName, scope, st, &t)
			return toolScopeMsg("enabled", toolName, scope)
		},
	})
}

// execAgentToolDisable sets Enabled=false on matching entries in every scope.
func execAgentToolDisable(toolName, scope string, st *interactiveState) string {
	return runToolScopeOp(toolName, scope, st, toolScopeOp{
		past:  "disabled",
		check: checkToolEntryExists,
		apply: func(toolName, scope string, st *interactiveState) string {
			f := false
			setToolEnabled(toolName, scope, st, &f)
			return toolScopeMsg("disabled", toolName, scope)
		},
	})
}

// execAgentToolAdd adds a new tool-agent entry to every target scope.
// desc is the parsed description= value (may contain spaces and the word
// "for" — parsing already happened in parseAgentToolArgs).
func execAgentToolAdd(toolName, desc, scope string, st *interactiveState) string {
	if desc == "" {
		return milkTag() + " usage: /agent tool add <tool-agent> description=<desc> [for <agent>|global]"
	}
	return runToolScopeOp(toolName, scope, st, toolScopeOp{
		past:  "added",
		check: checkToolEntryAbsent,
		apply: func(toolName, scope string, st *interactiveState) string {
			entry := config.AgentToolEntry{Agent: toolName, Description: desc}
			if scope == "global" {
				st.cfg.AgentTools = append(st.cfg.AgentTools, entry)
			} else {
				acIdx := findAgentIdx(st.cfg, scope)
				st.cfg.Agents[acIdx].Tools = append(st.cfg.Agents[acIdx].Tools, entry)
			}
			return toolScopeMsg("added", toolName, scope)
		},
	})
}

// execAgentToolRemove removes tool-agent entries from every target scope.
func execAgentToolRemove(toolName, scope string, st *interactiveState) string {
	return runToolScopeOp(toolName, scope, st, toolScopeOp{
		past:  "removed",
		check: checkToolEntryInScope,
		apply: func(toolName, scope string, st *interactiveState) string {
			if scope == "global" {
				idx := findToolEntryIdx(st.cfg.AgentTools, toolName)
				st.cfg.AgentTools = append(st.cfg.AgentTools[:idx], st.cfg.AgentTools[idx+1:]...)
			} else {
				acIdx := findAgentIdx(st.cfg, scope)
				idx := findToolEntryIdx(st.cfg.Agents[acIdx].Tools, toolName)
				st.cfg.Agents[acIdx].Tools = append(st.cfg.Agents[acIdx].Tools[:idx], st.cfg.Agents[acIdx].Tools[idx+1:]...)
			}
			return toolScopeMsg("removed", toolName, scope)
		},
	})
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
// toolSets is the live runtime map of agent-name → *mcp.ToolSet; may be nil.
func execMCP(sub string, st *interactiveState, toolSets map[string]*mcp.ToolSet) string {
	parts := strings.Fields(sub)
	if len(parts) == 0 {
		return execMCPList("", st, toolSets)
	}
	verb := parts[0]
	rest := strings.TrimSpace(strings.TrimPrefix(sub, verb))

	switch verb {
	case "list":
		return execMCPList(rest, st, toolSets)
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
	case "reconnect":
		return execMCPReconnect(rest, st, toolSets)
	case "auth":
		// /mcp auth is handled in handleMCPCmd (needs the bubbletea program
		// handle to stream status updates back while it runs in the background).
		return milkTag() + " usage: /mcp auth <server-name>  (run this in the TUI)"
	default:
		return milkTag() + " unknown subcommand: /mcp " + verb + "\n  try: list, add, remove, enable, disable, tools, assign, unassign, reconnect, auth"
	}
}

// execMCPList shows the configured MCP servers, optionally filtered to those
// visible by a given agent. When agentFilter is empty, shows all servers.
// toolSets is the live runtime map of agent-name → *mcp.ToolSet; may be nil.
func execMCPList(agentFilter string, st *interactiveState, toolSets map[string]*mcp.ToolSet) string {
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
			writeMCPServerLine(&b, s, agentForServer, toolSets)
		}
	} else {
		fmt.Fprintf(&b, "%s MCP servers (%d):\n", milkTag(), len(servers))
		for _, s := range servers {
			writeMCPServerLine(&b, s, agentForServer, toolSets)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// runtimeMCPStatus returns the runtime ConnectionStatus for serverName by
// checking all toolsets. Returns StatusDisconnected when toolSets is nil or
// the server is not in any active toolset.
func runtimeMCPStatus(serverName string, toolSets map[string]*mcp.ToolSet) mcp.ConnectionStatus {
	for _, ts := range toolSets {
		if s := ts.ServerStatus(serverName); s != mcp.StatusDisconnected {
			return s
		}
	}
	return mcp.StatusDisconnected
}

// runtimeMCPRetryIn returns how long until the named server's backoff expires,
// or 0 when the server is not backed off.
func runtimeMCPRetryIn(serverName string, toolSets map[string]*mcp.ToolSet) time.Duration {
	for _, ts := range toolSets {
		for _, c := range ts.Clients() {
			if strings.EqualFold(c.ServerName(), serverName) {
				until := c.DeadUntil()
				if until.IsZero() {
					return 0
				}
				remaining := time.Until(until)
				if remaining < 0 {
					return 0
				}
				return remaining
			}
		}
	}
	return 0
}

func writeMCPServerLine(b *strings.Builder, s config.MCPServerConfig, agentForServer map[string][]string, toolSets map[string]*mcp.ToolSet) {
	var connStatus string
	if toolSets != nil {
		switch runtimeMCPStatus(s.Name, toolSets) {
		case mcp.StatusConnected:
			connStatus = "  " + green("●")
		case mcp.StatusDead:
			retryIn := runtimeMCPRetryIn(s.Name, toolSets)
			if retryIn > 0 {
				connStatus = "  " + red(fmt.Sprintf("✖ retry in %ds", int(retryIn.Seconds())))
			} else {
				connStatus = "  " + red("✖ backed off")
			}
		default:
			connStatus = "  " + dim("○")
		}
	}
	cfgStatus := ""
	if !s.IsEnabled() {
		cfgStatus = "  " + dim("disabled")
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
	endpoint := s.URL
	if strings.ToLower(s.Transport) == "stdio" {
		endpoint = s.Command
		if len(s.Args) > 0 {
			endpoint += " " + strings.Join(s.Args, " ")
		}
	}
	fmt.Fprintf(b, "  %-20s  %-8s  %s%s%s%s\n", bold(s.Name), auth, endpoint, agentBadge, connStatus, cfgStatus)
}

// execMCPReconnect resets the dead flag on named server(s) so they will
// attempt a lazy reconnect on next use. With no argument, resets all dead servers.
func execMCPReconnect(name string, _ *interactiveState, toolSets map[string]*mcp.ToolSet) string {
	if len(toolSets) == 0 {
		return milkTag() + " no live MCP connections to reconnect (start milk with an agent that has mcp_servers configured)"
	}
	name = strings.TrimSpace(name)
	if name == "" {
		// Reset all dead clients across all toolsets.
		count := 0
		for _, ts := range toolSets {
			for _, c := range ts.Clients() {
				if c.Status() == mcp.StatusDead {
					c.Reset()
					count++
				}
			}
		}
		if count == 0 {
			return milkTag() + " no dead MCP servers to reconnect"
		}
		return fmt.Sprintf("%s reset %d dead MCP server(s) — they will reconnect on next use", milkTag(), count)
	}
	// Reset a specific server by name.
	found := false
	for _, ts := range toolSets {
		if ts.ResetServer(name) {
			found = true
		}
	}
	if !found {
		return fmt.Sprintf("%s MCP server %q not found in any active toolset", milkTag(), name)
	}
	return fmt.Sprintf("%s MCP server %q reset — will reconnect on next use", milkTag(), name)
}

// execMCPAdd adds a new MCP server entry from key=value args.
func execMCPAdd(rest string, st *interactiveState) string {
	fields := parseKVArgs(rest)
	name := fields["name"]
	url := fields["url"]
	if name == "" || url == "" {
		return milkTag() + " usage: /mcp add name=<name> url=<url> [transport=http|sse] [auth=bearer|token_cmd] [api_key=…] [timeout=30s]"
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
	updated := config.UpsertMCPServer(&st.cfg, entry)
	if err := saveLocalOrGlobal(st.cfg, activeConfigDir(st)); err != nil {
		return fmt.Sprintf("%s added MCP server %q (config save failed: %v)", milkTag(), name, err)
	}
	verb := "added"
	if updated {
		verb = "updated"
	}
	return fmt.Sprintf("%s MCP server %q %s — use /mcp assign %s for <agent> to expose it", milkTag(), name, verb, name)
}

// removeMCPServer removes serverName from cfg.MCPServers and cleans up every
// agent's mcp_servers reference to it. Returns false (cfg left unchanged)
// when no matching server was found. Shared by /mcp remove and the headless
// "milk config mcp remove" CLI subcommand.
func removeMCPServer(cfg *config.Config, serverName string) bool {
	idx := findMCPServerIdx(cfg.MCPServers, serverName)
	if idx < 0 {
		return false
	}
	cfg.MCPServers = append(cfg.MCPServers[:idx], cfg.MCPServers[idx+1:]...)
	lower := strings.ToLower(serverName)
	for i, ac := range cfg.Agents {
		var kept []string
		for _, sname := range ac.MCPServers {
			if strings.ToLower(sname) != lower {
				kept = append(kept, sname)
			}
		}
		cfg.Agents[i].MCPServers = kept
	}
	return true
}

// execMCPRemove removes an MCP server by name, also cleaning up all agent references.
func execMCPRemove(name string, st *interactiveState) string {
	if !removeMCPServer(&st.cfg, name) {
		return fmt.Sprintf("%s MCP server %q not found", milkTag(), name)
	}
	if err := saveLocalOrGlobal(st.cfg, activeConfigDir(st)); err != nil {
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
	if err := saveLocalOrGlobal(st.cfg, activeConfigDir(st)); err != nil {
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

// mcpAssignOutcome describes the result of assignMCPServer.
type mcpAssignOutcome int

const (
	mcpAssignOK   mcpAssignOutcome = iota
	mcpAssignNoop                  // already assigned (assign) / not assigned (unassign) — nothing to do
	mcpAssignServerNotFound
	mcpAssignAgentNotFound
)

// assignMCPServer adds or removes serverName from agentName's mcp_servers
// list in cfg. Shared by the TUI's /mcp assign|unassign and the headless
// "milk config mcp assign|unassign" CLI subcommand — callers format their
// own success/error messaging around the returned outcome.
func assignMCPServer(cfg *config.Config, serverName, agentName string, assign bool) mcpAssignOutcome {
	if findMCPServerIdx(cfg.MCPServers, serverName) < 0 {
		return mcpAssignServerNotFound
	}
	acIdx := findAgentIdx(*cfg, agentName)
	if acIdx < 0 {
		return mcpAssignAgentNotFound
	}
	lower := strings.ToLower(serverName)
	existing := cfg.Agents[acIdx].MCPServers
	if assign {
		for _, sn := range existing {
			if strings.ToLower(sn) == lower {
				return mcpAssignNoop
			}
		}
		cfg.Agents[acIdx].MCPServers = append(existing, serverName)
		return mcpAssignOK
	}
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
		return mcpAssignNoop
	}
	cfg.Agents[acIdx].MCPServers = kept
	return mcpAssignOK
}

// execMCPAssign adds or removes an MCP server reference from an agent's
// mcp_servers list. rest is "<server[,server…]> for <agent[,agent…]>": both
// sides accept comma-separated lists (#165) and every server×agent
// combination is applied. Validation is all-or-nothing — an unknown name
// rejects the whole call with a per-item report and changes nothing — and
// outcomes are grouped per server. The config is saved once at the end.
func execMCPAssign(rest string, assign bool, st *interactiveState) string {
	verb := "assign"
	if !assign {
		verb = "unassign"
	}
	serverSeg, agentSeg, ok := parseMCPAssignArgs(rest)
	if !ok {
		return fmt.Sprintf("%s usage: /mcp %s <server> for <agent>", milkTag(), verb)
	}
	servers := splitCommaNames(serverSeg)
	agents := splitCommaNames(agentSeg)
	if len(servers) == 0 || len(agents) == 0 {
		return fmt.Sprintf("%s usage: /mcp %s <server> for <agent>", milkTag(), verb)
	}
	var invalid []string
	for _, s := range servers {
		if findMCPServerIdx(st.cfg.MCPServers, s) < 0 {
			invalid = append(invalid, fmt.Sprintf("%s MCP server %q not found — add it first with /mcp add", milkTag(), s))
		}
	}
	for _, a := range agents {
		if findAgentIdx(st.cfg, a) < 0 {
			invalid = append(invalid, fmt.Sprintf("%s agent %q not found", milkTag(), a))
		}
	}
	if len(invalid) > 0 {
		return strings.Join(invalid, "\n") + "\n" + milkTag() + " nothing applied"
	}
	var msgs []string
	changed := false
	for _, s := range servers {
		var hit, miss []string
		for _, a := range agents {
			switch assignMCPServer(&st.cfg, s, a, assign) {
			case mcpAssignOK:
				changed = true
				hit = append(hit, a)
			default: // mcpAssignNoop — existence validated above
				miss = append(miss, a)
			}
		}
		if len(hit) > 0 {
			if assign {
				msgs = append(msgs, fmt.Sprintf("%s MCP server %q assigned to %s", milkTag(), s, agentTargetList(hit)))
			} else {
				msgs = append(msgs, fmt.Sprintf("%s MCP server %q unassigned from %s", milkTag(), s, agentTargetList(hit)))
			}
		}
		if len(miss) > 0 {
			if assign {
				msgs = append(msgs, fmt.Sprintf("%s MCP server %q already assigned to %s", milkTag(), s, agentTargetList(miss)))
			} else {
				msgs = append(msgs, fmt.Sprintf("%s MCP server %q not assigned to %s", milkTag(), s, agentTargetList(miss)))
			}
		}
	}
	if changed {
		if err := saveLocalOrGlobal(st.cfg, activeConfigDir(st)); err != nil {
			msgs = append(msgs, fmt.Sprintf("%s %sed %s (config save failed: %v)", milkTag(), verb, quotedNames(servers), err))
		}
	}
	return strings.Join(msgs, "\n")
}

// agentTargetList formats agent names for a per-item outcome line:
// "agent \"x\"" (singular) or "agents \"x\", \"y\"" (plural).
func agentTargetList(names []string) string {
	if len(names) == 1 {
		return fmt.Sprintf("agent %q", names[0])
	}
	return "agents " + quotedNames(names)
}

// quotedNames joins names as "a", "b".
func quotedNames(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
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
