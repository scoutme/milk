package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/claudesettings"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/session"
)

const cmdEscalate = "/escalate"
const cmdLocal = "/local"
const cmdPaste = "/paste"
const cmdLearn = "/learn"
const cmdOtel = "/otel"
const cmdMetrics = "/metrics"
const cmdMemory = "/memory"
const cmdExport = "/export"
const cmdHistory = "/history"
const cmdPanel = "/panel"
const cmdForget = "/forget"
const cmdSkipPerms = "/skip-permissions"
const cmdProvider = "/provider"

var slashCommands = []string{
	cmdEscalate, cmdLocal, cmdPaste, cmdLearn, cmdOtel, cmdMetrics, cmdMemory, cmdExport, cmdHistory, cmdPanel, cmdForget, cmdSkipPerms, cmdProvider,
	"/new", "/drop", "/list", "/help", "/exit", "/quit",
}

func promptLabel(_ *interactiveState) string {
	return "> "
}

const interactiveHelp = `Slash commands:
  /escalate        pin all subsequent turns to Claude (until /local or Ctrl+C clears it)
  /escalate <msg>  force this single turn to Claude, then return to normal routing
  /local           pin all subsequent turns to local model (until /escalate or Ctrl+C)
  /local <msg>     force this single turn to local model, then return to normal routing
  /learn <fact>    store a persistent memory (e.g. /learn prefer JSON output)
  /metrics         show most recent metric values (memory stats, otel sizes)
  /otel            show observability file sizes and record counts
  /export          print current session transcript (text)
  /export json     print session as JSON
  /export <path>   write session transcript to file
  /memory          list all percepts (global + session)
  /memory global   list only global percepts
  /memory session  list only session percepts
  /memory <pat>    list percepts whose content contains <pat>
  /memory show <pat|#id>  show full details of matching percepts
  /otel trim       archive current otel files and start fresh
  /otel off        disable OTel for this session
  /otel on         re-enable OTel for this session
  /history         show current history mode (session or global)
  /history global  switch input navigation to global history
  /history session switch input navigation to session history (default)
  /panel memory    toggle the memory panel (right-side percept viewer)
  /forget <pat>    delete a percept by description or #id (asks for confirmation)
  /skip-permissions        show current dangerously_skip_permissions state
  /skip-permissions on     enable skip-permissions for this session (Claude auto-approves all tools)
  /skip-permissions off    disable skip-permissions for this session (Claude prompts for tool use)
  /provider                show active local-agent provider (URL, model, auth method)
  /new             start a fresh session
  /drop            delete current session
  /list            list sessions for current directory
  /help            show commands and key bindings
  /exit            quit

Scrolling:
  Mouse wheel       scroll transcript
  PgUp/PgDn         scroll transcript half page
  Ctrl+U/Ctrl+F     scroll transcript half page

History:
  Up/Down (single-line)   navigate input history
  Ctrl+Up/Ctrl+Down       navigate input history
  Ctrl+R                  reverse search through input history (type to filter, Ctrl+R again for older, Ctrl+S for newer, Enter to accept, Esc to cancel)
  Ctrl+S                  forward search through input history

Multi-line input:
  Ctrl+N              insert a newline (most reliable)
  Shift+Alt+Enter     insert a newline
  AltGr+Enter         insert a newline
  Alt+Enter           insert a newline (Windows Terminal may capture this)
  Paste               multi-line pastes are sent as a single block automatically

@ prefix:
  @path       reference a file path`

const errFmt = "error: %v\n"

// interactiveState holds mutable state for the interactive loop.
type interactiveState struct {
	sess          *session.Session
	forceEscalate bool
	forceLocal    bool
	// stickyEscalate is set when the user explicitly calls /escalate with no
	// prompt. It causes every subsequent turn to route to Claude until the user
	// calls /local or closes the session. forceEscalate is reset after each
	// turn; stickyEscalate persists across turns.
	stickyEscalate bool
	// stickyLocal mirrors stickyEscalate for the local model.
	stickyLocal bool
	cwd         string
	cfg         config.Config
	mem         *memory.Store
	cs          *claudesettings.Store // Claude project settings (permissions persistence)
	program     *tea.Program          // set after tea.NewProgram, before Run

	// toolFutures caches per-tool answer channels created by OnToolUse as soon
	// as each tool call is detected in the stream. The user is asked immediately
	// via the TUI; handleStructuredDenials reads the answer (blocking briefly if
	// the user hasn't responded yet). Keyed by tool name.
	toolFutures     map[string]chan string
	skipPermissions bool // session-level override for DangerouslySkipPermissions
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
	cmdLocal:    true,
	cmdLearn:    true,
	cmdOtel:     true,
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
	case cmdMemory:
		output = execMemory(prompt, st)
	case cmdExport:
		output = execExport(prompt, st)
	case cmdEscalate:
		st.forceLocal = false
		st.stickyLocal = false
		if prompt == "" {
			// No inline prompt: pin all subsequent turns to Claude.
			st.stickyEscalate = true
			st.forceEscalate = false
			output = milkTag() + " pinned to " + blue("Claude") + " (use /local to unpin)"
		} else {
			// Inline prompt: single-turn override only.
			st.forceEscalate = true
		}
		return false, prompt, output
	case cmdLocal:
		st.forceEscalate = false
		st.stickyEscalate = false
		if prompt == "" {
			// No inline prompt: pin all subsequent turns to local.
			st.stickyLocal = true
			st.forceLocal = false
			output = milkTag() + " pinned to " + green("local model") + " (use /escalate to unpin)"
		} else {
			// Inline prompt: single-turn override only.
			st.forceLocal = true
		}
		return false, prompt, output
	case cmdSkipPerms:
		output = execSkipPerms(prompt, st)
	case cmdProvider:
		output = execProvider(st)
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
		return milkTag() + " " + red("dangerously_skip_permissions ON") + " — Claude will auto-approve all tool uses"
	case "off":
		st.skipPermissions = false
		return milkTag() + " dangerously_skip_permissions OFF — Claude will prompt for tool permissions"
	default:
		state := "off"
		if st.skipPermissions {
			state = red("on")
		}
		return fmt.Sprintf("%s dangerously_skip_permissions is %s  (use /skip-permissions on|off)", milkTag(), state)
	}
}

// execProvider shows the active local-agent provider configuration (no credentials).
func execProvider(st *interactiveState) string {
	cfg := st.cfg

	url := cfg.LlamaURL
	if url == "" {
		url = "http://localhost:8080"
	}
	model := cfg.LlamaModel
	if model == "" {
		model = "qwen2.5-coder"
	}

	provider := strings.ToLower(strings.TrimSpace(cfg.LlamaProvider))
	var authDesc string
	switch provider {
	case "", "local":
		authDesc = "none (local / no-auth)"
	case "bedrock":
		region := cfg.LlamaAWSRegion
		if region == "" {
			region = "(unset)"
		}
		service := cfg.LlamaAWSService
		if service == "" {
			service = "bedrock"
		}
		authDesc = fmt.Sprintf("AWS SigV4 (region: %s, service: %s)", region, service)
	default:
		if cfg.LlamaAPIKey != "" {
			authDesc = fmt.Sprintf("Bearer token (%s, key set)", provider)
		} else {
			authDesc = fmt.Sprintf("Bearer token (%s, key NOT set)", provider)
		}
	}

	extraHeaders := len(cfg.LlamaHeaders)
	var headerNote string
	if extraHeaders > 0 {
		headerNote = fmt.Sprintf(", %d extra header(s)", extraHeaders)
	}

	return fmt.Sprintf("%s local agent provider\n  url:    %s\n  model:  %s\n  auth:   %s%s",
		milkTag(), bold(url), bold(model), authDesc, headerNote)
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
