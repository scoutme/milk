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

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
	viewport "github.com/scoutme/milk/internal/vp"

	"github.com/scoutme/milk/internal/agent/aider"
	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/agent/smolagent"
	"github.com/scoutme/milk/internal/agent/subprocess"
	"github.com/scoutme/milk/internal/claudesettings"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/events"
	"github.com/scoutme/milk/internal/loop"
	"github.com/scoutme/milk/internal/mcp"
	"github.com/scoutme/milk/internal/mcpauth"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/shelldetect"
	"github.com/scoutme/milk/internal/tasks"
	"github.com/scoutme/milk/internal/updater"
	"github.com/scoutme/milk/internal/workflow"
	"github.com/scoutme/milk/internal/workflow/interp"
)

// agentTimeout bounds only the routing/classifier step (turn_routing.go's
// routeTurn) — not the agent's turn, which has no hard deadline by default
// (issue #305: idle warnings + opt-in limits.turn_deadline_secs).
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
	// agentWiring is the host callback set buildToolRunner applies to
	// lazily-built tool-agent runners (issue #188) so they ask for
	// permissions exactly like the session's own agents. Zero value = no
	// host wiring (headless callers), which keeps the pre-#188 defaults.
	agentWiring localAgentWiring
	// mcpToolSets holds the live MCP ToolSet for each agent that has MCP servers
	// configured, keyed by agent name. Used by /mcp list and /mcp reconnect.
	mcpToolSets map[string]*mcp.ToolSet
	// mcpServersSeen caches the EffectiveMCPServers resolution last used to
	// build mcpToolSets/mcpServers for each agent name, so refreshMCPForRole
	// can skip rebuilding when nothing has actually changed.
	mcpServersSeen map[string][]config.MCPServerConfig
	// backgroundMgr tracks spawn_background_agent jobs (ADR-0043) across
	// however many turns this session runs. Constructed once per session
	// (not per turn — buildTUIAgents re-wires the same *Manager onto each
	// turn's freshly-copied local.Agent) with a base context that outlives
	// any single turn's cancellable context. Nil when the active agent
	// config doesn't support it (e.g. no local provider available yet).
	backgroundMgr *local.Manager
}

// agentConfigChanged reports whether a config change touches anything
// rebuildRunners constructs — the agent entries, the active-agent selections,
// or the agent-as-tool list (whose lazily-built runners pin old agents).
// Everything else hot-reloads without a runner rebuild.
func agentConfigChanged(a, b config.Config) bool {
	return a.Agent != b.Agent ||
		a.EscalationAgent != b.EscalationAgent ||
		!reflect.DeepEqual(a.Agents, b.Agents) ||
		!reflect.DeepEqual(a.AgentTools, b.AgentTools)
}

// refreshMCPToolSets rebuilds the MCP toolset for the primary and escalation
// agents when their EffectiveMCPServers has changed since it was last built
// — on a config-write event (the config-watcher reload path, or right after
// any wizard/exec command saves a change that could affect MCPServers or an
// agent's mcp_servers list). When nothing changed for a role, this is a
// no-op: the existing runner/toolset is reused exactly as before, with no
// added per-turn cost. A turn already in flight keeps using the
// runner/toolset snapshot it started with — only the next turn on an
// affected role sees the rebuilt one.
func (m model) refreshMCPToolSets() model {
	var changedPrimary, changedEscalation bool
	m, changedPrimary = m.refreshMCPForRole(RolePrimary, activeLocalAgentConfig(m.st.cfg).Name)
	m, changedEscalation = m.refreshMCPForRole(RoleEscalation, m.st.cfg.EscalationAgentConfig().Name)
	if changedPrimary || changedEscalation {
		m.appendTranscript(milkTag() + " MCP servers reconnected to match current config\n")
	}
	return m
}

// refreshMCPForRole rebuilds the live MCP connections/server list for the
// TurnRunner currently serving role, if agentName's EffectiveMCPServers no
// longer matches what was last built for it. Returns changed=true when a
// rebuild happened.
func (m model) refreshMCPForRole(role AgentRole, agentName string) (model, bool) {
	return m.refreshMCPForRoleForce(role, agentName, false)
}

// refreshMCPForRoleForce is refreshMCPForRole with the "already up to date"
// short-circuit disabled. rebuildRunners uses it after replacing the runners:
// the mcpServersSeen cache still describes the replaced runners, so every
// toolset/server list must be re-attached even when the server config itself
// is unchanged.
func (m model) refreshMCPForRoleForce(role AgentRole, agentName string, force bool) (model, bool) {
	if agentName == "" {
		return m, false
	}
	if m.agents.mcpServersSeen == nil {
		m.agents.mcpServersSeen = map[string][]config.MCPServerConfig{}
	}
	newServers := m.st.cfg.EffectiveMCPServers(agentName)
	if !force && reflect.DeepEqual(newServers, m.agents.mcpServersSeen[agentName]) {
		return m, false
	}
	if force && len(newServers) == 0 {
		// Nothing to attach — the fresh runner starts without a toolset, the
		// same as the startup wiring. Just drop the stale bookkeeping.
		if old := m.agents.mcpToolSets[agentName]; old != nil {
			old.Close(m.ctx)
			delete(m.agents.mcpToolSets, agentName)
		}
		m.agents.mcpServersSeen[agentName] = newServers
		return m, false
	}
	m.agents.mcpServersSeen[agentName] = newServers

	var runner TurnRunner
	if role == RolePrimary {
		runner = m.agents.primary
	} else {
		runner = m.agents.escalation
	}

	switch r := runner.(type) {
	case *localRunner:
		old := m.agents.mcpToolSets[agentName]
		_, ts, err := buildMCPToolSet(m.ctx, m.st.cfg, agentName)
		if err != nil {
			m.appendTranscript(fmt.Sprintf("%s MCP connect error (agent %q): %v\n", milkTag(), agentName, err))
		}
		if ts == nil {
			ts = mcp.NewToolSet(nil)
		}
		if role == RolePrimary {
			m.agents.local = m.agents.local.WithMCPToolSet(ts)
			m.agents.primary = newLocalRunner(m.agents.local, agentName)
		} else {
			m.agents.escalationLocal = m.agents.escalationLocal.WithMCPToolSet(ts)
			m.agents.escalation = newLocalRunner(m.agents.escalationLocal, agentName)
		}
		m = m.trackMCPToolSet(agentName, old, ts)
	case *subprocessRunner:
		if r.agent.SupportsNativeMCP() {
			// Native MCP (smolagent, issue #122): no milk-side ToolSet exists
			// for this agent — only the resolved server list on the runner
			// needs refreshing. Close any stale toolset defensively (a native
			// agent never registers one; this guards a provider switch).
			if old := m.agents.mcpToolSets[agentName]; old != nil {
				old.Close(m.ctx)
				delete(m.agents.mcpToolSets, agentName)
			}
			newRunner := r.withMCPServers(newServers)
			if role == RolePrimary {
				m.agents.primary = newRunner
			} else {
				m.agents.escalation = newRunner
			}
			break
		}
		old := m.agents.mcpToolSets[agentName]
		servers, ts, err := buildMCPToolSet(m.ctx, m.st.cfg, agentName)
		if err != nil {
			m.appendTranscript(fmt.Sprintf("%s MCP connect error (agent %q): %v\n", milkTag(), agentName, err))
		}
		if ts == nil {
			ts = mcp.NewToolSet(nil)
		}
		newRunner := r.withMCPToolSet(servers, ts)
		if role == RolePrimary {
			m.agents.primary = newRunner
		} else {
			m.agents.escalation = newRunner
		}
		m = m.trackMCPToolSet(agentName, old, ts)
	case *cliRunner:
		// No live connection in milk's own process — the claude subprocess
		// connects directly via --mcp-config, regenerated fresh every turn
		// (internal/agent/claude/claude.go writeMCPConfigFile). Only the
		// cached server-name list needs updating.
		newRunner := r.withMCPServers(newServers)
		if role == RolePrimary {
			m.agents.primary = newRunner
		} else {
			m.agents.escalation = newRunner
		}
	}
	return m, true
}

// trackMCPToolSet closes old (if any) and records ts as the live toolset for
// agentName, so /mcp list and the next refresh comparison see the update.
func (m model) trackMCPToolSet(agentName string, old, ts *mcp.ToolSet) model {
	if old != nil {
		old.Close(m.ctx)
	}
	if m.agents.mcpToolSets == nil {
		m.agents.mcpToolSets = map[string]*mcp.ToolSet{}
	}
	m.agents.mcpToolSets[agentName] = ts
	return m
}

// --- TUI message types ---

// chunkMsg carries a chunk of streamed agent output.
type chunkMsg struct{ text string }

// requestSizeMsg carries the exact marshaled size (bytes) of the request
// just sent to a local-provider agent for the current turn, replacing the
// live ctx% estimate's crude len(userInput)-based guess with the real
// figure. Fired once per inference call, so a multi-step tool-calling turn
// updates it again on each subsequent call.
type requestSizeMsg struct{ bytes int64 }

// prefixChunkMsg carries the agent-name prefix printed before streaming begins.
// It is appended to the transcript but excluded from the live token estimate.
type prefixChunkMsg struct{ text string }

// thinkChunkMsg carries a chunk of streamed thinking/reasoning output, kept
// separate from regular content so it can be shown or hidden independently.
type thinkChunkMsg struct{ text string }

// workflowThinkChunkMsg is a workflow agent's reasoning chunk. It goes to the
// workflow's own live buffer, never the main transcript (see workflowDisplaySend).
type workflowThinkChunkMsg struct{ text string }

// retractMsg tells the TUI that the agent replaced already-streamed response
// text (from) with a clean one (to) — see Agent.WithOnRetract.
type retractMsg struct{ from, to string }

// reasoningPromotedMsg signals that the current turn's already-streamed
// reasoning text has been promoted (by the local agent) to become the
// turn's actual final answer, so the accumulated thinking for this turn
// should be discarded rather than persisted as a duplicate of the answer.
type reasoningPromotedMsg struct{}

// agentDoneMsg signals the agent goroutine finished.
type agentDoneMsg struct{ err error }

// backgroundJobStartedMsg is sent immediately when a spawn_background_agent
// job (ADR-0043) is created — from Manager.SetOnStart, off the goroutine that
// called Spawn (which may be a background tool-loop, not the TUI's own).
// Used solely to auto-open the background-agents panel so its activity is
// visible without the user having to notice and press F3 first; see
// (*model).autoOpenBackgroundPanel.
type backgroundJobStartedMsg struct{}

// backgroundJobDoneMsg is sent immediately when a spawn_background_agent job
// (ADR-0043) completes or fails — independent of, and typically well before,
// the turn-boundary path (drainBackgroundJobs) that injects the same result
// into the next turn's context. This is purely the live-notification path:
// it lets the transcript/status bar reflect completion as soon as it
// happens, without waiting for the user's next input.
type backgroundJobDoneMsg struct{ job *local.Job }

// backgroundShellStartedMsg is sent when a background shell is started via
// bash(run_in_background) — from local.AddShellListener, off the tool dispatch
// goroutine that ran the tool (ADR-0064). Used to auto-open the background
// panel so the new shell row is visible without pressing F3 first.
type backgroundShellStartedMsg struct{ info local.ShellInfo }

// backgroundShellDoneMsg is sent when a background shell exits (ADR-0064).
// Unlike backgroundJobDoneMsg it writes no transcript block: a shell's
// output is live-buffer material, read through the attach view or
// bash_output, not turn output the model reported. Toast + panel
// finalization only.
type backgroundShellDoneMsg struct{ info local.ShellInfo }

// backgroundBatchDoneMsg is sent exactly once when the last currently-
// outstanding spawn_background_agent job finishes (Manager.SetOnBatchDone —
// ActiveCount reaches 0), i.e. once per wave rather than once per job. This
// is what actually turns "results are ready" into a real follow-up turn:
// neither backgroundJobDoneMsg (a passive transcript line) nor the
// turn-boundary drain path (which only runs when some other turn happens to
// be dispatched) generates a response on their own.
type backgroundBatchDoneMsg struct{}

// backgroundSpawnedMsg is sent after a user-initiated background agent spawn
// completes asynchronously (via tea.Cmd). Carries the job ID and label so
// the Update handler can append the transcript confirmation.
type backgroundSpawnedMsg struct {
	jobID string
	label string
	model string
}

// backgroundUserJobDoneMsg is sent when a user-initiated background job
// (spawned via the busy-key "Ctrl+Enter" flow, not a tool call)
// finishes. Unlike backgroundBatchDoneMsg, this fires per job rather than
// waiting for a whole wave — there's nothing to consolidate; the user forked
// off one specific side-question and the result should reach the main agent
// as soon as it's free, not held back for unrelated jobs still running.
type backgroundUserJobDoneMsg struct{}

// startWorkflowFromToolMsg is sent when a local-provider agent's
// start_workflow tool call signals a launch request (dispatch.go's
// onWorkflowStart callback, wired from the turn-dispatch goroutine). Handled
// by launching the workflow directly via launchGenericWorkflow, bypassing
// the interactive per-role wizard entirely — ws.Roles already has every role
// resolved (explicit override or defaulted to "escalation" by
// dispatchOneTool), since a tool call has no way to answer wizard prompts.
type startWorkflowFromToolMsg struct{ ws *local.WorkflowStartSignal }

// directBashDoneMsg is sent when a direct-bash command exits (PTY or ExecProcess path).
type directBashDoneMsg struct {
	err     error
	logPath string // path to script(1) typescript; empty when using PTY pane
}

// ptyOutputMsg is sent by the PTY read goroutine whenever new bytes have been
// written to the VT emulator. Triggers a View() re-render of the PTY pane.
type ptyOutputMsg struct{}

// turnWatchNoticeMsg carries one idle-watchdog notice line (issue #305):
// the advisory "no activity" warning, the deadline wrap-up request, or the
// hard-stop note. Preformatted by turnIdleWatch's shared text builders so the
// TUI and ACP word them identically. Never cancels anything by itself.
type turnWatchNoticeMsg struct{ text string }

type spinnerTickMsg struct{}

// workflowIdleCheckMsg is sent periodically while a workflow is running. It
// checks whether the current role's turn has exceeded that agent's configured
// turn timeout since the last streamed activity, and warns once per idle
// stretch — the workflow equivalent of turnWatchNoticeMsg, needed because a
// workflow's overall run spans many turns rather than the single turn a
// one-shot timer could cover.
type workflowIdleCheckMsg struct{ gen int }

// copyFeedbackClearMsg clears the transient copy confirmation in the status bar.
type copyFeedbackClearMsg struct{}

// busyHintClearMsg clears the transient "agent is responding" hint in the status bar.
type busyHintClearMsg struct{}

// quitPendingClearMsg clears the "press ctrl+c again to exit" hint.
type quitPendingClearMsg struct{}

// dragResetMsg fires when the mouse-drag safety timeout expires.  If the
// terminal's MouseActionRelease event was dropped (pointer drifted outside
// reported viewport bounds between frames), mode 1002 stays enabled and
// wheel-scroll stops working.  The timeout — scheduled on press, rescheduled
// on every motion, cancelled on release — detects this condition and resets
// the terminal to mode 1000 (basic tracking, reliable wheel).
//
// Each scheduling call increments the generation counter; a stale message
// (generation mismatch) is ignored so that a timeout from an earlier motion
// cannot reset the mode while the user is still actively dragging.
type dragResetMsg struct{ gen uint64 }

// memoryRefreshMsg fires on a periodic tick to redraw the memory panel.
type memoryRefreshMsg struct{}

// taskStoreChangedMsg is sent whenever the task store mutates (create,
// update, complete, delete — Store.SetOnChange fires on all of them), off
// whatever goroutine made the change, typically a tool call mid-turn. Used
// to auto-open the tasks panel so newly-active task content is visible
// without the user having to notice and press F2 first; see
// (*model).autoOpenPanel. Bubbletea's own post-Update redraw is what
// actually repaints the panel — this message just carries the "wake up and
// check" signal across goroutines.
type taskStoreChangedMsg struct{}

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
type updateAvailableMsg struct {
	release *updater.Release
	// checkedAt is the UpdateLastCheck timestamp updateCheck already wrote to
	// disk, so the in-memory config copy can sync without a re-read.
	checkedAt string
}

// workflowResumeCheckMsg is sent at startup when a saved workflow state file
// was found for the current session. The TUI prints a one-line resume offer.
type workflowResumeCheckMsg struct {
	state *workflow.State
	// genericName/genericTask are set instead of state for an unfinished
	// interpreter-driven (non-"dev") checkpoint — it has no sprint/pass to
	// report, just a name and task.
	genericName string
	genericTask string
}

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
	// autoDefault ("y"/"n") and autoAt mark a background ask with a timed
	// answer (ADR-0052): the queue applies autoDefault when autoAt passes
	// with no reply, shows a countdown for it, and answers overflow with it.
	autoDefault string
	autoAt      time.Time
	// freeText marks an ask_user question prompt: the answer is whatever the
	// user types, so the "a"/"d" bulk keys are ordinary text and overflow
	// resolves with silence (no autoDefault to apply).
	freeText bool
}

// permDismissMsg withdraws a permission prompt whose answer came from
// somewhere else (remote oversight won the race — see readLineCtx). respCh is
// used only for identity: the racing ask already stopped reading it.
type permDismissMsg struct {
	respCh chan string
}

// notifyMsg is tuiHost's Notify bridge (events.Host) — sent from a turn's own
// goroutine, consumed by one Update() case that calls the existing m.notify,
// same async-bridge shape as permRequestMsg above.
type notifyMsg struct {
	text string
	hint string
}

// oauthRequiredMsg is sent by the claude agent when stderr indicates an MCP
// server requires OAuth authorization. serverName may be empty when not
// detectable; authURL may be empty when no URL appeared in the error.
type oauthRequiredMsg struct {
	serverName string
	authURL    string
}

// mcpOAuthStartedMsg is sent once /mcp auth has discovered the authorization
// URL for a server (after RFC 9728/8414 discovery and, if needed, RFC 7591
// dynamic client registration), so the transcript can print it and the
// browser open can be attempted. The terminal state (success/failure) is
// reported separately via credRefreshReadyMsg.
type mcpOAuthStartedMsg struct {
	serverName string
	authURL    string
}

// mcpConnectResult is one server's outcome from the deferred startup MCP
// connect (#161). err is nil when the client connected; a server backing
// several agents' toolsets is collapsed to a single result by
// mergeConnectResults before it ever reaches the TUI.
type mcpConnectResult struct {
	server string
	err    error
}

// mcpConnectReadyMsg delivers the merged per-server outcomes of the startup
// MCP connect. Emitted by the mcpConnectInit cmd from Init() — never from a
// goroutine started before p.Run() — and consumed as toasts only: an
// unconnected client stays wired and retries lazily on first use, so this is
// turn-unrelated information, not turn output (ADR-0048's boundary).
type mcpConnectReadyMsg struct {
	results []mcpConnectResult
}

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
	return r.readLineCtx(context.Background(), prompt, label, "", time.Time{})
}

// readLineCtx is readLineLabeled with cancellation and an optional timed
// answer (autoDefault/autoAt — ADR-0052): when ctx ends before the user
// answers — a racing remote-oversight surface won the permission race, or a
// bounded background ask hit its deadline — the prompt is withdrawn via
// permDismissMsg and the error returned. respCh is buffered so a just-in-time
// answer that nobody reads any more can never block the UI goroutine.
func (r *tuiInputReader) readLineCtx(ctx context.Context, prompt, label, autoDefault string, autoAt time.Time) (string, error) {
	respCh := make(chan string, 1)
	r.send(permRequestMsg{prompt: prompt, label: label, respCh: respCh, autoDefault: autoDefault, autoAt: autoAt})
	select {
	case s := <-respCh:
		return s, nil
	case <-ctx.Done():
		r.send(permDismissMsg{respCh: respCh})
		return "", ctx.Err()
	}
}

// readLineFreeText is readLineCtx for an ask_user question prompt: same
// bridge, but the prompt is free text (no y/n, no bulk keys) and a timed ask
// resolves with silence rather than an autoDefault string.
func (r *tuiInputReader) readLineFreeText(ctx context.Context, prompt, label string, autoAt time.Time) (string, error) {
	respCh := make(chan string, 1)
	r.send(permRequestMsg{prompt: prompt, label: label, respCh: respCh, autoAt: autoAt, freeText: true})
	select {
	case s := <-respCh:
		return s, nil
	case <-ctx.Done():
		r.send(permDismissMsg{respCh: respCh})
		return "", ctx.Err()
	}
}

// makeLocalPermAsk returns the permAsk callback for the local agent, routed
// through events.Host.RequestPermission (Phase 1's one real Host migration —
// see docs/internal/design/machine-readable-output-design.md) and raced over the three
// answer sources (ADR-0052's permask.go) — direct input and remote oversight
// here, no timed answer: a foreground ask waits for a human. host is backed
// by tuiHost, which reuses the existing TUI permRequestMsg flow: the
// goroutine blocks while the TUI displays a yellow permission prompt, and a
// remote answer withdraws it via permDismissMsg (readLineCtx). Grants are
// persisted to ps (may be nil). Session-level skipPermissions is handled by
// the caller via WithSkipPermissions before this is ever called. n may be
// nil — no remote surface.
func makeLocalPermAsk(host events.Host, ps *local.PermStore, n oversight.Notifier, st *interactiveState) func(tool, summary string) bool {
	return func(tool, summary string) bool {
		p := foregroundPermPolicy(st.permOverridesNow())
		p.sticky = stickySetter(st.setSessionPermMode, func(note string) {
			host.Notify(events.Notification{Text: note})
		})
		allow, _ := askPermission(context.Background(), n,
			oversight.PermRequest{ToolName: tool, Input: summary},
			func(ctx context.Context) bool {
				outcome, err := host.RequestPermission(ctx, events.PermissionRequest{Prompt: permAskPrompt(tool, summary), Tool: tool, Summary: summary})
				return err == nil && outcome.Allow
			},
			p)
		return allow
	}
}

// makeLocalUserAsk returns the ask_user callback for the local agent: the
// question race (ADR-0052's ask layer with a question instead of a
// permission — issue #249) over direct input (host.Elicit — the TUI's
// free-text prompt) and remote oversight (Telegram's AskInput), no timed
// answer: a foreground question waits for a human. Silence resolves nothing
// — only a real answer or ctx cancellation returns.
func makeLocalUserAsk(host events.Host, n oversight.Notifier) func(ctx context.Context, question string, options []string) (string, bool) {
	return func(ctx context.Context, question string, options []string) (string, bool) {
		answer, ok, _ := askUserQuestion(ctx, n, oversight.InputRequest{Question: question, Options: options},
			func(c context.Context) (string, bool) {
				res, err := host.Elicit(c, events.ElicitationRequest{
					Prompt: userAskPrompt(question, options, ""),
					Label:  "ask_user",
				})
				if err != nil || res.Cancelled || strings.TrimSpace(res.Value) == "" {
					return "", false
				}
				return res.Value, true
			},
			foregroundAskPolicy)
		return answer, ok
	}
}

// permAskPrompt builds the human-readable permission prompt for a local-agent
// tool ask — the text both hosts show (the TUI verbatim; over ACP it rides on
// session/request_permission's description and on the remote-oversight
// request). Shared so a question can't read differently depending on which
// surface asked it. The a/d choices are TUI keys (issue #302): they answer
// this ask and the whole session; over ACP the same choices are the
// "Always allow"/"Always deny" dialog options.
func permAskPrompt(tool, summary string) string {
	prompt := fmt.Sprintf("\n%s permission request — primary agent tool: %s", milkTag(), bold(tool))
	if summary != "" {
		prompt += fmt.Sprintf("  (%s)", dim(summary))
	}
	prompt += fmt.Sprintf("\n%s Allow? [Y/n/a/d] (a: allow all, d: deny all) ", milkTag())
	return prompt
}

// --- Styles ---

// Chrome styles depend on the terminal background; rebuildStyles (theme.go)
// fills them at startup and again when the background is detected.
var (
	styleHeaderBar     lipgloss.Style
	styleStatusBar     lipgloss.Style
	styleStatusBarPerm lipgloss.Style
	styleBorder        lipgloss.Style
)

// --- model ---

type model struct {
	vp     viewport.Model
	ta     textarea.Model
	width  int
	height int
	ready  bool

	// transcript accumulator (pointer — strings.Builder must not be copied by value).
	// Holds the regular (non-reasoning) content. Reasoning lives in thinkBlocks
	// (ADR-0067) and is spliced in as fold containers at render time by
	// transcriptDisplay — there is no second buffer variant to keep in lockstep.
	transcript *strings.Builder
	// thinkBlocks are this session's reasoning blocks (see thinkblocks.go).
	// Rendered at their anchor offsets as collapsible containers.
	thinkBlocks []thinkBlock
	// thinkBlocksTurnStart is the thinkBlocks index where the current turn's
	// blocks begin — reasoningPromotedMsg drops from here when the streamed
	// reasoning is promoted to the turn's answer.
	thinkBlocksTurnStart int
	// thinkFoldOpen is the session's current think-block fold status: a new
	// reasoning block is created in this state (it keeps the folding status
	// the previous block was left in). Seeded from config
	// think_fold_default at session start (folded unless "expanded" —
	// issue #278); updated only by explicit fold actions (header click,
	// Ctrl+T, /think expand|collapse); the turn-boundary tidy that
	// collapses previous turns' blocks does not change it — and does not
	// run while it is open (/think expand defines the current default).
	thinkFoldOpen bool

	// showTips toggles the idle tip line below the input (ADR-0061, /tips).
	// Defaults on (config show_tips); the line's row is reserved while it is
	// on — see hintRegion — so toggling never shifts the layout mid-session
	// more than the one reflow /tips itself triggers.
	showTips bool
	// tipIdx is the idle tip rotation's position in buildTipList() (tips.go).
	// Advanced by tipsTickMsg while the user is idle; frozen while a turn
	// runs or a prompt is pending.
	tipIdx int
	// tipPinned marks a manual tip browse (Alt+Up/Down, /tips
	// next|prev|pick): the rotation's entry then outranks contextual hints
	// (tips.go currentTip) until the next automatic advance clears the flag,
	// so stepping through tips never looks like a no-op while an urgent
	// hint is up.
	tipPinned bool
	// tipsGen tags the idle-rotation tick in flight (tipsTickMsg.gen): a
	// manual browse restarts the cycling timer (restartTipsTick), so a tick
	// scheduled before the restart carries an older gen and is dropped
	// instead of cutting the browse short (the toastGen pattern).
	tipsGen int

	// anim holds the spring animations (animations.go); a pointer, so the
	// copies Bubble Tea passes around share one state. Mutated in Update only.
	anim *animState

	// palette is the open command palette (palette.go), nil when closed.
	palette *paletteState

	// blurred is true while the terminal window lacks focus (tea.BlurMsg).
	// Zero value = focused, so a bare model behaves as before. Background
	// polling (tips rotation, project/git refresh) pauses while it is set.
	blurred bool
	// currentTurnThinking accumulates thinking text for the current in-progress
	// turn so it can be stored in session.Turn.Thinking when the turn completes.
	currentTurnThinking *strings.Builder

	// spinner state
	busy         bool
	spinnerFrame int

	// history navigation
	sessionHistory     []string // entries for this session only (default navigation)
	globalHistory      []string // entries across all sessions
	useGlobalHistory   bool     // when true, navigate globalHistory instead
	histIdx            int
	saved              string
	savedLeadingPasted bool // leadingPasted snapshot for saved, restored on historyForward back to it

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
	tabValueMode    bool     // true when tabMatches are concrete parameter values (#166): replace/append the token under the cursor
	tabNsLabel      string   // name-space shown beside value hints (value mode only)
	tabSegPrefix    string   // multi-value params (#165): comma-prefix preserved when inserting a value
	tabHints        []string // hint lines shown below viewport (may have one entry highlighted)
	tabHintsBase    []string // same lines without any highlight; source of truth for highlightHint
	hintIdx         int      // selected inline hint (-1 = none)

	// pending permission request (non-nil while waiting for user y/n) and queue
	// for tool-use permission prompts that arrive while a prior one is active.
	pendingPerm *permRequestMsg
	permQueue   []permRequestMsg
	// permInputStash keeps the draft the user was typing when the first
	// permission prompt arrived (background asks can land mid-draft), so it
	// can be restored once no prompt is pending any more (ADR-0052).
	permInputStash *string

	// cancelTurn cancels the context of the running agent turn; nil when idle.
	cancelTurn  context.CancelFunc
	interrupted bool // set when user cancels a turn via ctrl+c

	// pendingBackgroundFollowup is set when an agent-initiated
	// spawn_background_agent wave finishes (backgroundBatchDoneMsg) while
	// busy or otherwise blocked, so handleAgentDone can retry the
	// auto-follow-up once idle again. Only fires once every job in the
	// wave has finished (ActiveCount reaches 0) — the calling agent
	// designed a consolidated, multi-part wave meant to be reported
	// together.
	pendingBackgroundFollowup bool
	// pendingUserBackgroundFollowup is the equivalent for a user-initiated
	// spawn (via the busy-key "Ctrl+Enter" flow below, not a
	// tool call): there is no "wave" to consolidate, so this fires as soon
	// as the model goes idle regardless of whether other jobs — agent- or
	// user-initiated — are still running. Deliver what's ready rather than
	// waiting on unrelated work the user didn't ask this particular
	// request to wait for.
	pendingUserBackgroundFollowup bool

	// active tool use — non-empty while the escalation agent is executing a tool call
	activeToolUse string

	// panelManualOverride tracks which panels the user has explicitly
	// shown/hidden (via /panel or its F1-F6 shortcut). Once a region is in
	// here, automatic "this panel's content just became active" opens (see
	// autoOpenPanel) skip it — the user's own choice sticks over automatic
	// management. With the default "remember_panel_state" (ADR-0078) the
	// choice is a durable preference that never expires (and is restored
	// from the folder's panel-state file at startup); with it off, stickiness
	// only lasts while the panel's content stays live:
	// resetManualOverrideIfFinalized drops the entry again the moment the
	// content finalizes (emptied, completed, or done —
	// panelContentFinalized), re-arming auto-open for the next new content
	// (ADR-0059).
	panelManualOverride map[panelRegion]bool

	// panelPrefs is the durable half of a manual toggle (ADR-0078 as
	// amended by ADR-0087): the user's chosen open/closed state per panel,
	// persisted to the folder-bound state file and restored as *startup
	// layout* only. It never vetoes content-driven auto-open — the
	// in-session veto while a dismissal covers live content is
	// panelManualOverride, which expires per ADR-0059.
	panelPrefs map[panelRegion]bool

	// panel state persistence (ADR-0078): where panel preferences live on
	// disk (empty = persistence unavailable, e.g. in tests) and whether the
	// "remember_panel_state" feature is on (default true). panelStateFolder
	// is the folder the state belongs to, stored inside the file for
	// self-description (panelStateFile.Folder).
	panelStatePath     string
	panelStateFolder   string
	rememberPanelState bool

	// panelAutoOpen is the master switch for content-driven auto-open
	// (ADR-0087, "/panel autoopen on|off", config panel_auto_open,
	// default on): while off, autoOpenPanel and applyMemoryContentDefault
	// never open a panel — panels open only on an explicit toggle
	// (/panel <name> or F1-F6). Per-panel manual overrides and the
	// tabbed-theme gate layer on top when it is on.
	panelAutoOpen bool

	// panelTabMRU is the tabbed-theme (ADR-0079) most-recently-viewed stack
	// of panel tabs, front = the panel in the shared slot. Activation order
	// only — which panels exist and which are open lives in the per-panel
	// flags — so closing the front tab hands the slot back to the tab the
	// user looked at before, like closing a browser tab. Unused when the
	// theme has no tabs (panels are laid out side by side there).
	panelTabMRU []panelRegion

	// memory panel (in-place expansion mirrors into the tasks panel)
	panelMemory        bool
	memoryExpanded     string
	panelOffset        int
	mem                *memory.Store
	lastPanelClickID   string
	lastPanelClickTime time.Time

	// tasks panel
	panelTasks   bool
	tasksOffset  int
	taskStore    *tasks.Store
	taskExpanded string

	// background-agents panel (ADR-0043)
	panelBackground  bool
	backgroundOffset int

	// project tree + git graph panels (issue #206, ADR-0066)
	panelProject     bool
	projectOffset    int
	projectExpanded  map[string]bool
	projectFiles     []string
	projectTruncated bool
	projectLines     []string
	projectIDs       []string
	// Session changeset for the files panel (issue #331): last porcelain
	// snapshot (git), the merged map the tree renders, and its cwd/repo
	// bookkeeping — see changeset.go.
	changesetFiles    map[string]session.ChangeKind
	changesetGit      map[string]session.ChangeKind
	changesetGitOK    bool
	changesetNoGit    bool
	changesetCwd      string
	lastTreeClickPath string
	lastTreeClickTime time.Time
	panelGit          bool
	gitOffset         int
	gitRows           []gitGraphLine
	gitBranch         string
	gitDirty          int
	gitErr            string
	gitLines          []string
	gitIDs            []string

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

	// pending /agent tool add|enable|disable|remove wizard (missing args)
	pendingTool *agentToolWizardState

	// pending "which agent runs this background job?" question
	pendingBgAgent *bgAgentWizardState

	// pending /setup telegram wizard
	pendingTelegramSetup *telegramSetupState

	// pending /init wizard
	pendingInit *initWizardState

	// prompt width (visual columns) set by the most recent refreshPrompt call;
	// used by taRows() to compute the exact content wrap width.
	promptWidth int

	// click-to-select state (content-space coordinates; -1 = none)
	selAnchorLine    int
	selAnchorCol     int
	selEndLine       int
	selEndCol        int
	selDragging      bool   // true once the mouse has moved after the initial press
	selText          string // plain text of the selected range (populated after release)
	dragResetPending bool   // true while a drag-timeout cmd is outstanding
	dragResetGen     uint64 // generation counter; stale dragResetMsgs are ignored
	dragScrollDir    int    // -1/+1 while a drag's pointer is above/below the scrollable rows (autoscroll), else 0
	dragScrollX      int    // pointer column the autoscroll keeps extending the selection to
	dragScrollGen    uint64 // generation counter; stale dragScrollMsgs are ignored
	dragScrollArmed  bool   // an autoscroll tick loop is live
	dragScrollIdle   int    // consecutive ticks that could not scroll (at a limit)
	dragSawOutside   bool   // true if the last drag motion/release left the drag's own area (dropped-release evidence, see dragResetMsg)

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

	// click-to-select state for the attach view (ADR-0047 / issue #207):
	// content-space coordinates into the attach viewport's own lines (the
	// "── attached: … ──" header included; -1 = none). Kept separate from the
	// transcript selection above so selecting attached text never highlights
	// or copies the hidden transcript underneath, and vice versa.
	attachSelAnchorLine int
	attachSelAnchorCol  int
	attachSelEndLine    int
	attachSelEndCol     int
	attachSelDragging   bool
	attachSelText       string

	copyFeedback string // transient "[copied N chars]" shown in status bar
	busyHint     string // transient "agent is responding" shown in status bar
	// busyEnterArmed is set by the first Enter while busy (the busy-input
	// disposition's confirm step): the first press announces both choices
	// (queue / fork), the second press on the same gesture queues. Any
	// non-Enter key disarms (handleBusyKey top).
	busyEnterArmed bool
	credRefreshing bool   // true while any background credential refresh is running
	credLabel      string // which credential is being refreshed (e.g. "AWS", "token")
	credStatus     string // non-empty after refresh completes: last result message
	credOK         bool   // true if last refresh succeeded, false if failed

	// Notification toasts (issue #162): turn-unrelated informational events
	// render as a floating overlay instead of entering the transcript.
	// Full lifecycle detail lives in notify.go (queue → visible → history).
	toastQueue     []toastEvent // waiting to become visible (FIFO)
	toastVisible   []toastEvent // currently rendered, each with expiresAt set
	toastHistory   []toastEvent // every event ever, capped; shown by /notifications
	toastGen       int          // generation of the live expiry tick (stale ticks ignored)
	toastTickArmed bool         // true while a toastTickMsg is in flight

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

	// restartPlan is armed when /restart quits the TUI (issue #258): runREPL
	// relaunches with this plan after the program exits and the terminal is
	// restored. See restart_core.go.
	restartPlan *RestartPlan

	// pendingForceFresh is set to true in dispatchAgent when the turn begins with
	// ForceFreshEscalation so that handleAgentDone can zero lastEscalationContextHash
	// after the turn completes, preventing a stale hash from suppressing the fresh
	// session's first context block.
	pendingForceFresh bool

	// pendingDirectBash is non-nil while waiting for y/N confirmation to run a
	// shell command directly. The string holds the command to run on approval.
	pendingDirectBash *string

	// bangMode is true after the user presses "!" as the first character of
	// an empty textarea — the Claude Code-style direct-execution-mode trigger.
	// The "!" itself is consumed (never inserted into the textarea); only the
	// prompt label changes to a red "!" while active. Entered/exited in
	// handleKey (on "!" / backspace-on-empty), handleCtrlC (clearing the
	// line), and handleEnter (on submit, which re-prepends "!" to the
	// submitted text so submitInput's stripBangPrefix still applies).
	bangMode bool

	// leadingPasted is true when the character(s) at the very start of the
	// current textarea buffer arrived via a real bracketed-paste event rather
	// than being typed. Set in handleKey's msg.Paste branch when the cursor
	// sits at absolute buffer offset 0 at the moment of the paste; cleared at
	// every point the buffer is reset or replaced wholesale (submit, Ctrl+C
	// clear, history recall). Used to keep slash/bang/direct-bash triggering
	// gated to deliberately typed input — see submitInput and handleBusyKey
	// (issue #151: pasted content, e.g. a copied transcript that happens to
	// start with "/learn" or "!rm -rf", must not execute as a command).
	leadingPasted bool

	// ptyPane is non-nil while a shell command is running inside an embedded PTY.
	ptyPane *ptyPaneState

	// attached is non-nil while the TUI is showing a live-attach view over a
	// background job's or workflow's live buffer instead of the main
	// transcript viewport (ADR-0047; issue #154). See attach.go.
	attached *attachState

	// directBashConcurrentTurn is true when a direct-bash/bang command (via
	// launchPTYPane or launchDirectBashFallback) was launched while an agent
	// turn was already in progress — i.e. from handleBusyKey rather than the
	// idle path (issue #128). directBashDoneMsg's cleanup uses this to avoid
	// clobbering the still-running turn's busy/cancelTurn state, and to route
	// the command's output into sess.PendingBangOutput for the next turn
	// instead of relying solely on the (agent-invisible) transcript.
	directBashConcurrentTurn bool

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
	// Cumulative cache tokens (used to compute total context input).
	primaryCacheRead        int64
	primaryCacheCreation    int64
	escalationCacheRead     int64
	escalationCacheCreation int64

	// Live turn output: chars written during the current turn, used as a streaming proxy.
	// Reset at turn start.
	currentTurnChars int64
	// currentTurnInputChars holds the estimated input size (bytes/chars) for
	// the current turn's live ctx% status-bar estimate. Set to just the typed
	// prompt's length at dispatch (a placeholder for the brief window before
	// the first request goes out, and the only value ever available for
	// non-local-provider agents, e.g. claude-cli); overwritten with the exact
	// marshaled request payload size by requestSizeMsg once a local-provider
	// agent actually sends the request — updated again on each subsequent
	// call within a multi-step tool-calling turn.
	currentTurnInputChars int64
	// lastTurnPrompt/Completion are per-role deltas from the last completed turn
	// for each agent, captured at agentDoneMsg. lastTurnCacheRead/Creation track
	// the cache-token deltas for the same turn — needed by ctx:x/y to estimate
	// the actual conversation size sent to the model (per-turn, not cumulative).
	lastTurnPrompt      map[string]int64
	lastTurnCompletion  map[string]int64
	lastTurnCacheRead   map[string]int64
	lastTurnCacheCreate map[string]int64
	// lastTokenRole tracks which role's counters were last displayed; used to detect
	// role changes and clear stale last-turn counters between turns.
	lastTokenRole string
	// lastTurnTiming holds per-role throughput timing deltas (TTFT sum,
	// decode-seconds sum, measured requests) of the last completed turn;
	// seenTiming is the last observed cumulative timing per role — the delta
	// base, mirroring primaryPrompt/escalationPrompt above.
	lastTurnTiming map[string]turnTiming
	seenTiming     map[string]turnTiming
	// turnFirstOutputAt stamps the first streamed output token of the current
	// turn (content or thinking). statusTokens divides the estimated output so
	// far by the elapsed time since this stamp for its live tok/s estimate;
	// zero until the first output arrives (a prefill-phase rate would be
	// fiction). Cleared wherever currentTurnChars is reset.
	turnFirstOutputAt time.Time

	colorizeMode ColorizeMode

	// colorize cache: avoid re-running chroma/glamour on every streamed token.
	// The cache is invalidated when the transcript grows by ≥ colorizeLineThresh
	// new lines, or when the viewport width changes, or when the caller
	// explicitly sets colorizeForce = true (e.g. after agentDoneMsg, resize).
	colorizeCached    string // last colorized output
	colorizeTransLen  int    // transcript byte length cache was committed to
	colorizeVPWidth   int    // vpWidth when cache was built
	colorizeForce     bool   // if true, bypass cache on next render
	colorizeLinesSeen int    // new lines since last full re-colorize
	// colorizeSeenLen is the display byte length seen by the previous render
	// — the base for counting arriving lines (colorizeLinesSeen). Distinct
	// from colorizeTransLen, which is deliberately capped before an open
	// think block's container and would otherwise re-count it every render.
	colorizeSeenLen int

	// txPartCache/txSepCache/txLinesCache: the transcript part of the
	// viewport content cached as rows (ADR-0088). Content-addressed —
	// rebuilt whenever the wrapped transcript or the separator changes, so
	// keystrokes stop paying O(transcript bytes) per input event (the old
	// path concatenated the whole transcript into a content string and let
	// the viewport re-split and re-measure it on every key).
	txPartCache  string
	txSepCache   string
	txLinesCache []string

	// wrappedTranscript's content-addressed memo (ADR-0088) — see its doc
	// comment in transcript.go for the key semantics.
	wrapCacheKey   wrappedTxKey
	wrapCache      string
	wrapCacheValid bool

	// thinkGen is the display-invalidation generation bumped by
	// invalidateThinkDisplay; part of wrappedTranscript's memo key so fold
	// toggles and thinking-stream re-renders never serve a stale render.
	thinkGen int

	// Per-turn colorization cache: stores colorized output for each completed
	// turn (splitTurns segments ending with "\n\n"). Completed turns never
	// change, so their cached colorized output remains valid across streaming
	// updates. Only the last (incomplete) turn is re-colorized on cache miss.
	// This makes full re-colorization O(last turn) instead of O(full transcript).
	turnColorCache []string // colorized output per completed turn
	turnRawCache   []string // raw text per completed turn (cache key)

	// viewport rebuild throttle: bubbles/viewport.SetContent re-splits and
	// re-measures the *entire* content on every call (including a full
	// findLongestLineWidth scan milk never needs, since it never scrolls
	// horizontally) — appendTranscript/appendThinking used to call it once per
	// streamed chunk, so a long turn with heavy tool output made the TUI
	// visibly lag. viewportDirty defers that rebuild to at most once per
	// viewportRebuildThrottle window; every other setViewportContent/syncLayout
	// call site (turn completion, resize, selection, …) is unaffected and still
	// rebuilds immediately, and spinnerTickMsg (already firing every 80ms while
	// busy) flushes any deferred rebuild so streamed text never lags by more
	// than one tick.
	viewportDirty       bool
	lastViewportRebuild time.Time

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

	// mcpConnectInit, if non-nil, is returned by Init() to run the deferred
	// startup MCP connect (#161) and emit mcpConnectReadyMsg once every
	// client's outcome is known.
	mcpConnectInit tea.Cmd

	// pendingUpdate is non-nil when an update is available but not yet installed.
	pendingUpdate *updater.Release
	// updateInstalling is true while /update install is downloading+applying.
	updateInstalling bool
	// updateProgress tracks download bytes for the progress indicator.
	updateProgress int64
	updateTotal    int64

	// workflow panel
	workflowPanelOpen   bool
	workflowPanelOffset int
	workflowState       *workflow.State
	// workflowRunning is true from the moment a workflow goroutine is
	// launched until its WorkflowDoneMsg lands — distinct from workflowState's
	// mere presence, since workflowResumeCheckMsg also populates workflowState
	// at startup for a dormant, not-yet-resumed checkpoint. launchGenericWorkflow
	// uses this to refuse a second concurrent fresh launch (see its guard):
	// without it, nothing stops start_workflow's tool-driven path (unlike the
	// /workflow slash commands, which all consult workflow.CurrentWorkflowID)
	// from starting a second interp.Runner goroutine that races the first one
	// over the single m.workflowState/m.cancelTurn/m.busy fields.
	workflowRunning bool
	// cancelWorkflow cancels the running workflow's context; nil when none runs.
	// Kept apart from cancelTurn/busy: a workflow runs in the background, so the
	// user can chat (and cancel or finish a turn) without touching it.
	cancelWorkflow context.CancelFunc
	// workflowCancelled marks a user-requested cancel so the done handler
	// reports an interruption rather than an error.
	workflowCancelled bool
	// workflowGen identifies the current run; idle-check ticks carry it so a tick
	// left over from an earlier run cannot start a second watchdog chain.
	workflowGen                  int
	pendingWorkflowWizard        *workflowWizardState
	pendingGenericWorkflowExtend *genericWorkflowExtendState

	// designer disambiguation
	pendingWorkflowQuestions string        // raw questions from designer (non-nil while collecting answers)
	workflowAnswersCh        chan<- string // channel to send user answers back to workflow goroutine

	// workflow idle watchdog: mirrors the turnWatchNoticeMsg mechanism used by
	// normal turns, but reset on every workflow role transition/streamed chunk
	// rather than a single one-shot timer, since a workflow runs many turns.
	lastWorkflowActivity  time.Time
	workflowTimeoutWarned bool

	// loop detection
	loopDetector  *loop.Detector
	loopInterrupt bool // true when an auto-interrupting loop signal fired
	// loopInterruptLabel is the interrupting signal's category ("loop" or
	// "consumption") for the status-bar badge — issue #173.
	loopInterruptLabel string
	loopWarning        string // non-empty when a warn-only signal fired

	// injected dependencies
	ctx    context.Context
	st     *interactiveState
	rtr    *router.Router
	agents dispatchAgents
}

func newModel(ctx context.Context, st *interactiveState, rtr *router.Router, agents dispatchAgents, mem *memory.Store) model {
	ta := buildTextarea()
	m := model{
		histIdx:             -1,
		hintIdx:             -1,
		ctx:                 ctx,
		st:                  st,
		rtr:                 rtr,
		agents:              agents,
		ta:                  ta,
		transcript:          &strings.Builder{},
		currentTurnThinking: &strings.Builder{},
		searchQuery:         &strings.Builder{},
		showTips:            st.cfg.ShowTipsEnabled(),
		thinkFoldOpen:       st.cfg.ThinkFoldExpanded(), // starting fold status (issue #278)
		mem:                 mem,
		// The memory panel is closed by default and opens only when the
		// store has content (ADR-0078) — see applyMemoryContentDefault.
		panelMemory:         false,
		rememberPanelState:  st.cfg.RememberPanelStateEnabled(),
		panelAutoOpen:       st.cfg.PanelAutoOpenEnabled(),
		panelManualOverride: map[panelRegion]bool{},
		panelPrefs:          map[panelRegion]bool{},
		selAnchorLine:       -1,
		selEndLine:          -1,
		panelSelRegion:      regionNone,
		panelSelAnchorLine:  -1,
		panelSelEndLine:     -1,
		attachSelAnchorLine: -1,
		attachSelEndLine:    -1,
		taSelAnchor:         -1,
		taSelEnd:            -1,
		lastUndoValue:       "\x00", // sentinel: never equals real textarea value, so first push always succeeds
		lastTurnPrompt:      map[string]int64{"primary": 0, "escalation": 0},
		lastTurnCompletion:  map[string]int64{"primary": 0, "escalation": 0},
		lastTurnCacheRead:   map[string]int64{"primary": 0, "escalation": 0},
		lastTurnCacheCreate: map[string]int64{"primary": 0, "escalation": 0},
		lastTurnTiming:      map[string]turnTiming{"primary": {}, "escalation": {}},
		seenTiming:          map[string]turnTiming{"primary": {}, "escalation": {}},
		loopDetector:        loop.New(st.cfg.LoopDetectionCfg()),
	}
	m.seedTranscriptFromHistory()
	return m
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
	} else if m.bangMode {
		label = red("!") + " "
	} else {
		label = promptLabel(m.st)
	}
	plain := ansi.Strip(label)
	m.promptWidth = rw.StringWidth(plain)

	m.ta.SetPromptFunc(m.promptWidth, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
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

// isCtrlEnterCSI detects Ctrl+Enter from bubbletea's unknownCSISequenceMsg
// String() representation ("?CSI[<decimal byte codes>]?"). Three known
// terminal encodings: kitty CSI-u (13;5u), xterm modifyOtherKeys (27;5;13~),
// and urxvt-style (13;5~). Ctrl modifier is always parameter 5.
func isCtrlEnterCSI(s string) bool {
	switch s {
	case "?CSI[49 51 59 53 117]?", // ESC [ 13;5 u  (kitty CSI-u)
		"?CSI[50 55 59 53 59 49 51 126]?", // ESC [ 27;5;13 ~  (xterm modifyOtherKeys)
		"?CSI[49 51 59 53 126]?":          // ESC [ 13;5 ~  (urxvt)
		return true
	}
	return false
}

// queuePromptCap bounds the in-memory Enter-while-busy queue (refused with
// a hint at the cap, the typed text kept in the input box).
const queuePromptCap = 20

// queuePrompt captures typed input for the live agent's next turn — the
// Enter disposition of the busy-input feature (Ctrl+Enter forks instead).
// The entry takes the staged attachments and paste provenance with it (they
// were staged for this typed input); the receipt line is the proof the input
// was not dropped and names the /queue review surface.
func (m *model) queuePrompt(input string) {
	if len(m.st.queuedPrompts) >= queuePromptCap {
		m.busyHint = fmt.Sprintf("queue is full (%d) — /queue drop some, or Ctrl+Enter to fork", queuePromptCap)
		return
	}
	q := queuedPrompt{
		text:          input,
		leadingPasted: m.leadingPasted,
		attachments:   m.pendingAttachments,
		queuedAt:      time.Now(),
	}
	m.pendingAttachments = nil
	m.leadingPasted = false
	m.st.queuedPrompts = append(m.st.queuedPrompts, q)

	m.ta.Reset()
	m.tabMatches = nil
	m.tabIdx = -1
	m.tabHints = nil
	m.tabHintsBase = nil
	m.busyHint = ""
	m.syncLayout()

	extra := ""
	if n := strings.Count(input, "\n") + 1; n > 1 {
		extra += fmt.Sprintf(" ⋯ %d lines", n)
	}
	if n := len(q.attachments); n > 0 {
		extra += fmt.Sprintf(" + %d attachments", n)
	}
	m.appendTranscript(milkTag() + fmt.Sprintf("⋯ queued for next turn — %q%s · /queue to review\n", queuePreview(input), extra))
}

// queuePreview returns the first input line clipped to 60 display columns.
func queuePreview(input string) string {
	r := []rune(strings.SplitN(input, "\n", 2)[0])
	if len(r) > 60 {
		return string(r[:59]) + "…"
	}
	return string(r)
}

// drainQueuedPrompt hands the oldest queued prompt to submitInput as the
// next turn (FIFO, one per turn). The flushed turn is the next turn —
// drainBackgroundJobs rides along inside it — so both follow-up flags are
// cleared here (re-armed by any job that finishes later). The entry replays
// its own paste provenance and attachments; whatever is staged at flush time
// joins the flushed turn (the staging field's own contract: staged files are
// context for the next turn).
func (m model) drainQueuedPrompt() (tea.Model, tea.Cmd, bool) {
	if len(m.st.queuedPrompts) == 0 {
		return m, nil, false
	}
	q := m.st.queuedPrompts[0]
	m.st.queuedPrompts = m.st.queuedPrompts[1:]
	m.pendingBackgroundFollowup = false
	m.pendingUserBackgroundFollowup = false
	m.leadingPasted = q.leadingPasted
	m.pendingAttachments = append(q.attachments, m.pendingAttachments...)
	next, cmd := m.submitInput(q.text, dim("[queued]")+" ")
	return next, cmd, true
}

// handleBusyKey handles key events while an agent turn is running.
// It intercepts the three busy-specific cases, then delegates to handleKey
// for all navigation, editing, history, undo/redo, and viewport scroll.
// Safe slash commands (display/read-only) are executed immediately even during a turn.
func (m model) handleBusyKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// The double-Enter queue confirmation only chains across consecutive
	// Enters: any other key (including Ctrl+Enter's fork) disarms it, so an
	// armed press can never queue text the user has since edited.
	if msg.String() != "enter" && msg.String() != "ctrl+m" {
		m.busyEnterArmed = false
	}
	switch msg.String() {
	case "ctrl+c":
		// The Ctrl+C stack is shared with the idle path (handleCtrlC):
		// undo queue entry → clear input → interrupt → quit, one stage per
		// press.
		return m.handleCtrlC()
	case "enter", "ctrl+m":
		// Enter-as-accept (tab completion / arrow-hint selection) is not a
		// submission, so it is not trapped while busy — only Enter below,
		// which would send the buffer as a prompt, is. Completion only
		// edits the input buffer, safe to use mid-turn.
		armed := m.busyEnterArmed
		m.busyEnterArmed = false
		var accepted bool
		m, accepted = m.acceptActiveCompletion()
		if accepted {
			return m, nil
		}
		input := strings.TrimSpace(stripCompletionPlaceholders(m.ta.Value()))
		// Bang mode consumed the "!" at keystroke time (same as handleEnter's
		// idle path); re-prepend it so stripBangPrefix below still recognizes
		// this as a direct-execution command.
		if m.bangMode {
			input = "!" + input
			m.bangMode = false
			m.refreshPrompt()
		}
		// "!" is always available regardless of busy state (issue #128): it's
		// an explicit, agent-bypassing request, so it must not be silently
		// funneled into the "Ctrl+Enter to spawn a background agent"
		// flow below, which would hand an LLM agent the raw "!..." string —
		// the agent has no special handling for milk's own bang syntax.
		if shellCmd, ok := stripBangPrefix(input); ok && !m.leadingPasted {
			m.ta.Reset()
			m.leadingPasted = false
			m.tabMatches = nil
			m.tabIdx = -1
			m.tabHints = nil
			m.tabHintsBase = nil
			m.busyHint = ""
			m.syncLayout()
			m.appendTranscript(promptLabel(m.st) + colorizeTokens(input) + "\n")
			if shellCmd == "" {
				return m, nil
			}
			return m.launchPTYPane(shellCmd)
		}
		if cmd, rest, found := extractSlashCommand(input); found && !m.leadingPasted {
			if busySafeCommands[cmd] || busySafeWorkflowCmd(cmd, rest) {
				// Safe command: execute immediately without clearing busy state.
				m.ta.Reset()
				m.tabMatches = nil
				m.tabIdx = -1
				m.tabHints = nil
				m.tabHintsBase = nil
				m.syncLayout()
				label := promptLabel(m.st)
				m.appendTranscript(userEcho(label, colorizeTokens(input)))
				return m.handleSlashInput(cmd, rest)
			}
			m.busyHint = cmd + " unavailable while agent is responding"
			return m, busyHintClearCmd()
		}
		if input == "" {
			m.busyHint = "agent is responding — Ctrl+C to interrupt"
			return m, busyHintClearCmd()
		}
		// Busy-input disposition, two-step (the queue half; Ctrl+Enter below
		// forks to a background agent). The first Enter announces both
		// choices — queueing silently would make the fork alternative
		// invisible — and the second Enter on the same gesture queues. Slash
		// commands never reach here as queue candidates (non-busy-safe ones
		// were rejected above with their own hint). "queue_while_busy": false
		// keeps the hint-only Enter.
		if !m.st.cfg.QueueWhileBusyEnabled() {
			m.busyHint = "agent is working — Ctrl+Enter to spawn a background agent with this"
			return m, busyHintClearCmd()
		}
		if armed {
			m.queuePrompt(input)
			return m, nil
		}
		m.busyEnterArmed = true
		m.busyHint = "agent is working — Enter again to queue it for the next turn · Ctrl+Enter to spawn a background agent with this"
		return m, busyHintClearCmd()
	case "ctrl+enter", "ctrl+j":
		// Bubble Tea v2 parses the extended Ctrl+Enter encodings (kitty
		// CSI-u, modifyOtherKeys) into "ctrl+enter". Terminals without
		// extended key protocols send \n (Ctrl+J) for it, which stays distinct
		// from Enter (\r). The raw-CSI sniffing in Update is kept as a net for
		// encodings the parser still reports as unknown.
		input := strings.TrimSpace(stripCompletionPlaceholders(m.ta.Value()))
		if input == "" {
			return m, nil
		}
		return m.startUserBackgroundAgent(input, "")
	case "tab", "shift+tab":
		// Completion only edits the input buffer, so Tab cycling and hint
		// commit are not trapped while busy — the same handleTabKey
		// semantics as idle. Only Enter-as-submission is trapped.
		dir := 1
		if msg.String() == "shift+tab" {
			dir = -1
		}
		return m.handleTabKey(dir), nil
	}
	return m.handleKey(msg)
}

// handlePermKey routes key events while a permission prompt is pending.
// Enter submits; "a"/"d" answer every pending prompt and set the session
// allow-all/deny-all mode for what follows (issue #302); anything else is
// passed to the textarea normally.
func (m model) handlePermKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		if m.pendingPerm.freeText {
			// Explicit no-answer for a question prompt (an accidental Enter
			// is ignored below instead of answering empty).
			m.pendingPerm.respCh <- ""
			m.appendTranscript("(no answer)\n")
		} else {
			m.pendingPerm.respCh <- "n"
			m.appendTranscript("n\n")
		}
		m.pendingPerm = nil
		m.dequeueNextPerm()
		m.afterPermResolved()
		return m, nil
	case "enter":
		answer := strings.TrimSpace(m.ta.Value())
		if m.pendingPerm.freeText {
			if answer == "" {
				return m, nil // ignore accidental Enter on a question
			}
			m.ta.Reset()
			m.leadingPasted = false
			m.syncLayout()
			m.appendTranscript(answer + "\n")
			m.pendingPerm.respCh <- answer
			m.pendingPerm = nil
			m.dequeueNextPerm()
			m.afterPermResolved()
			return m, nil
		}
		m.ta.Reset()
		m.leadingPasted = false
		m.syncLayout()
		m.appendTranscript(answer + "\n")
		m.answerPermPrompt(answer)
		return m, nil
	}
	var cmd tea.Cmd
	m.undoPush(true)
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// answerPermPrompt answers a y/n permission prompt's typed answer: y/n (and
// anything else) answer just the active prompt, "a"/"d" answer the active
// prompt plus the whole queue AND set the session allow-all/deny-all mode —
// the same keys doing the same thing whether one ask or a burst is pending
// (issue #302).
func (m *model) answerPermPrompt(answer string) {
	switch strings.ToLower(answer) {
	case "a":
		m.answerAllPerms("y")
		m.setSessionPermMode(sessionPermAllowAll)
	case "d":
		m.answerAllPerms("n")
		m.setSessionPermMode(sessionPermDenyAll)
	default:
		m.pendingPerm.respCh <- answer
		m.pendingPerm = nil
		m.dequeueNextPerm()
		m.afterPermResolved()
	}
}

// setSessionPermMode records the session allow-all/deny-all mode (issue #302)
// and says so once in the transcript: the mode answers every later tool ask
// silently, so setting it must never be silent.
func (m *model) setSessionPermMode(mode sessionPermMode) {
	m.st.permMu.Lock()
	prev := m.st.permOverrides.sessionMode
	m.st.permOverrides.sessionMode = mode
	m.st.permMu.Unlock()
	if prev != mode {
		m.appendTranscript(sessionPermModeNote(mode) + "\n")
	}
}

// answerAllPerms answers the active prompt and the whole queue with answer —
// the "a"/"d" bulk keys.
func (m *model) answerAllPerms(answer string) {
	if m.pendingPerm != nil {
		m.pendingPerm.respCh <- answer
		m.pendingPerm = nil
	}
	for i := range m.permQueue {
		m.permQueue[i].respCh <- answer
	}
	m.permQueue = nil
	m.afterPermResolved()
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
	m.leadingPasted = false
	m.syncLayout()
}

// permQueueMax bounds how many permission prompts may wait behind the active
// one (ADR-0052): a burst of background-job asks beyond it resolves straight
// to the ask's timed default instead of piling up.
const permQueueMax = 5

func (m model) handlePermRequest(msg permRequestMsg) (tea.Model, tea.Cmd) {
	// Session allow-all/deny-all mode (issue #302) answers tool asks as they
	// arrive: never queued, never shown, one line of audit trail. Questions
	// (the ask_user tool) are not permissions and are never auto-answered.
	if !msg.freeText {
		if mode := m.st.permOverridesNow().sessionMode; mode.decided() {
			ans, verb := "n", "denied"
			if mode.allow() {
				ans, verb = "y", "allowed"
			}
			m.appendTranscript(fmt.Sprintf("%s permission %s (session mode %s)\n", milkTag(), verb, mode.label()))
			msg.respCh <- ans
			return m, nil
		}
	}
	if m.pendingPerm != nil {
		if len(m.permQueue) >= permQueueMax && (msg.autoDefault != "" || (msg.freeText && !msg.autoAt.IsZero())) {
			// Background ask overflow: the timed answer resolves it now —
			// the default string for permissions, silence for questions.
			ans := msg.autoDefault
			if msg.freeText {
				ans = ""
			}
			m.appendTranscript(fmt.Sprintf("%s too many pending prompts — auto-answering %q (timed default)\n", milkTag(), ans))
			msg.respCh <- ans
			return m, nil
		}
		m.permQueue = append(m.permQueue, msg)
		return m, maybePermTick(msg)
	}
	// Keep whatever the user was typing — a background ask can land mid-draft
	// and the prompt flow resets the input line.
	if m.permInputStash == nil {
		v := m.ta.Value()
		m.permInputStash = &v
	}
	m.pendingPerm = &msg
	m.appendTranscript(msg.prompt)
	m.ta.Reset()
	m.leadingPasted = false
	m.syncLayout()
	return m, maybePermTick(msg)
}

// maybePermTick schedules the countdown refresh only for timed asks.
func maybePermTick(msg permRequestMsg) tea.Cmd {
	if msg.autoAt.IsZero() {
		return nil
	}
	return permTick()
}

// permTick refreshes the status-bar countdown once a second while a timed
// ask is pending (ADR-0052).
func permTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return permTickMsg{} })
}

type permTickMsg struct{}

// restorePermInput puts back the draft stashed when the first permission
// prompt arrived.
func (m *model) restorePermInput() {
	if m.permInputStash == nil {
		return
	}
	m.ta.SetValue(*m.permInputStash)
	m.permInputStash = nil
	m.leadingPasted = false
	m.syncLayout()
}

// afterPermResolved runs once a prompt is answered and nothing else is
// waiting: restores the stashed draft.
func (m *model) afterPermResolved() {
	if m.pendingPerm == nil && len(m.permQueue) == 0 {
		m.restorePermInput()
	}
}

// handlePermDismiss removes a pending/queued permission prompt whose answer
// came from somewhere else (remote oversight won the race, or the timed
// answer fired — ADR-0052), so the TUI doesn't sit on a question whose
// answer has already been applied. The displayed prompt gets a one-line note
// so the transcript stays honest about what happened.
func (m model) handlePermDismiss(msg permDismissMsg) (tea.Model, tea.Cmd) {
	if m.pendingPerm != nil && m.pendingPerm.respCh == msg.respCh {
		m.appendTranscript(dim("(answered elsewhere)") + "\n")
		m.pendingPerm = nil
		m.dequeueNextPerm()
		m.afterPermResolved()
		return m, nil
	}
	for i := range m.permQueue {
		if m.permQueue[i].respCh == msg.respCh {
			m.permQueue = append(m.permQueue[:i], m.permQueue[i+1:]...)
			m.afterPermResolved()
			return m, nil
		}
	}
	return m, nil
}

func (m model) handleAgentDone(msg agentDoneMsg) (tea.Model, tea.Cmd) {
	// The turn probably created, renamed or removed files: the @-file index
	// refreshes on its next use.
	if m.st != nil && m.st.cwd != "" {
		indexFor(m.st.cwd).invalidate()
	}
	m.closeOpenThinkBlock()
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
		case errors.Is(msg.err, errFallbackDeclined):
			// Not a failure: the user (or nobody) declined the Claude fallback.
			m.appendTranscript(dim("[turn not run — "+strings.TrimPrefix(errText, errFallbackDeclined.Error()+": ")+"]") + "\n")
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
	newPrimaryCacheRead, newPrimaryCacheCreation := obs.SessionCacheByRole("primary")
	// Compute per-role per-turn deltas from the accumulators.
	m.lastTurnPrompt["escalation"] = counterDelta(newEscPrompt, m.escalationPrompt)
	m.lastTurnCompletion["escalation"] = counterDelta(newEscComp, m.escalationComp)
	m.lastTurnPrompt["primary"] = counterDelta(newPrimaryPrompt, m.primaryPrompt)
	m.lastTurnCompletion["primary"] = counterDelta(newPrimaryCompletion, m.primaryCompletion)
	m.lastTurnCacheRead["escalation"] = counterDelta(newEscCacheRead, m.escalationCacheRead)
	m.lastTurnCacheCreate["escalation"] = counterDelta(newEscCacheCreation, m.escalationCacheCreation)
	m.lastTurnCacheRead["primary"] = counterDelta(newPrimaryCacheRead, m.primaryCacheRead)
	m.lastTurnCacheCreate["primary"] = counterDelta(newPrimaryCacheCreation, m.primaryCacheCreation)
	// Throughput timing deltas — exact mirror of the token deltas above.
	escTTFT, escDecode, escReqs := obs.SessionTimingByRolePrefix("escalation")
	priTTFT, priDecode, priReqs := obs.SessionTimingByRole("primary")
	m.lastTurnTiming["escalation"] = subtractTiming(escTTFT, escDecode, escReqs, m.seenTiming["escalation"])
	m.lastTurnTiming["primary"] = subtractTiming(priTTFT, priDecode, priReqs, m.seenTiming["primary"])
	m.seenTiming["escalation"] = turnTiming{ttftSec: escTTFT, decodeSec: escDecode, reqs: escReqs}
	m.seenTiming["primary"] = turnTiming{ttftSec: priTTFT, decodeSec: priDecode, reqs: priReqs}
	m.primaryPrompt, m.primaryCompletion = newPrimaryPrompt, newPrimaryCompletion
	m.escalationPrompt, m.escalationComp = newEscPrompt, newEscComp
	m.primaryCacheRead, m.primaryCacheCreation = newPrimaryCacheRead, newPrimaryCacheCreation
	m.escalationCacheRead, m.escalationCacheCreation = newEscCacheRead, newEscCacheCreation
	m.lastTokenRole = m.activeTokenRole()
	m.currentTurnChars = 0
	m.turnFirstOutputAt = time.Time{}
	m.currentTurnInputChars = 0

	// Loop detection: feed turn summary and check for signals.
	if m.loopDetector != nil {
		turnText := ""
		reasoningText := ""
		if hist := m.st.sess.History; len(hist) > 0 {
			turnText = hist[len(hist)-1].Content
			reasoningText = hist[len(hist)-1].Thinking
		}
		turnDelta := m.lastTurnPrompt[m.activeTokenRole()] + m.lastTurnCompletion[m.activeTokenRole()]
		verdicts := m.loopDetector.Feed(loop.TurnSummary{
			Text:          turnText,
			ReasoningText: reasoningText,
			InputTokens:   m.lastTurnPrompt[m.activeTokenRole()],
			OutputTokens:  m.lastTurnCompletion[m.activeTokenRole()],
			Timestamp:     time.Now(),
			IsUserTurn:    false,
		})
		for _, v := range verdicts {
			// Shared message rules (loopVerdictMsg, issue #188): the
			// token_velocity escalation suggestion (only when the session
			// isn't already headed to escalation) and the auto-interrupt
			// marker. The two tiers below stay the TUI's own surface
			// decision: high-confidence verdicts reach the transcript,
			// lower ones only the status-bar warning.
			msg := loopVerdictMsg(v, m.st.stickyEscalate || m.st.autoStickyEscalate)
			if v.Confidence >= 0.8 {
				m.appendTranscript(yellow(loopSignalLine(v, msg)))
				if m.loopDetector != nil && v.ShouldInterrupt {
					m.loopInterrupt = true
					m.loopInterruptLabel = loopSignalLabel(v)
				}
				if v.Signal == loop.SignalTokenVelocity && m.loopWarning == "" {
					// token_velocity's own severity score is always >= 0.8,
					// so without this it would only ever reach the transcript
					// line above — never the status bar the escalate
					// suggestion is actually meant to surface in.
					m.loopWarning = fmt.Sprintf("⚠ %s: %s", loopSignalLabel(v), msg)
				}
			} else if v.Confidence >= 0.5 && m.loopWarning == "" {
				m.loopWarning = fmt.Sprintf("⚠ %s: %s", loopSignalLabel(v), msg)
			}
		}
		_ = turnDelta // used for future velocity display
	}

	m.appendTranscript("\n")
	m.colorizeForce = true // turn finished — force a clean full re-colorize
	m.refreshPrompt()
	m.syncLayout()

	// A double-Enter confirmation never survives a turn boundary.
	m.busyEnterArmed = false
	// Busy-input queue first (design: one queued prompt per turn, before the
	// background-followup and remote-input chain).
	if next, cmd, ok := m.drainQueuedPrompt(); ok {
		return next, cmd
	}
	if m.pendingUserBackgroundFollowup {
		return m.maybeAutoFollowupBackgroundJobs(false)
	}
	if m.pendingBackgroundFollowup {
		return m.maybeAutoFollowupBackgroundJobs(true)
	}
	if len(m.st.pendingRemoteInputs) > 0 {
		next := m.st.pendingRemoteInputs[0]
		m.st.pendingRemoteInputs = m.st.pendingRemoteInputs[1:]
		return m.submitInput(next, dim("[telegram]")+" ")
	}
	return m, nil
}

func isContextCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

func isContextDeadlineExceeded(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
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
		os.Stdout.WriteString("\x1b[?1002l\x1b[?1000h\x1b[?1006h") //nolint:errcheck
	}
}

// enableMouseTracking (re)asserts the at-rest mouse mode. Milk writes these
// modes itself, so bubbletea cannot restore them: ReleaseTerminal (tea.ExecProcess)
// resets 1002/1003/1006 and RestoreTerminal does not bring them back.
func enableMouseTracking() {
	os.Stdout.WriteString("\x1b[?1000h\x1b[?1006h") //nolint:errcheck
}

func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.ptyPane != nil {
		// The PTY pane's View() branch takes over the whole main area and
		// renders no side panels (see layout.go), but mainWidth()/regionAt
		// don't know that — a click at a panel's column while a PTY pane is
		// up would otherwise silently arm/attach against a panel that isn't
		// even on screen right now. Key routing already ignores everything
		// but handlePTYKey while ptyPane is active; mirror that here.
		return m, nil
	}
	ev := newMouseEvent(msg)
	region, regionX := m.regionAt(ev.X)
	// Drag-outside evidence (see dragResetMsg): a motion/release that lands
	// outside the active drag's own area means the pointer drifted out of the
	// viewport/panel — the situation where terminals commonly drop the release
	// event. An in-area event clears it again. The drag-timeout uses this to
	// tell a genuinely dropped release (reset mouse mode back to 1000) apart
	// from a mid-drag pause (keep mode 1002 so motion keeps updating the
	// highlight while the button is still held).
	switch ev.Action {
	case mousePress:
		m.dragSawOutside = false // a press starts a fresh interaction
	case mouseMotion, mouseRelease:
		// Chrome rows are measured, not assumed: "outside" means outside the
		// rows the viewport actually occupies (see viewportRows).
		vpTop, vpBottom := m.viewportRows()
		switch {
		case m.attachActive() && m.attachSelAnchorLine >= 0 && m.attachSelDragging:
			// Same rule as the transcript: the attach view fills the main
			// area, so "outside" means a panel column or the chrome rows.
			m.dragSawOutside = region != regionNone || ev.Y < vpTop || ev.Y >= vpBottom
		case m.selAnchorLine >= 0 && m.selDragging:
			m.dragSawOutside = region != regionNone || ev.Y < vpTop || ev.Y >= vpBottom
		case m.panelSelAnchorLine >= 0 && m.panelSelDragging:
			// handlePanelMouse applies no row bounds check of its own — only
			// region routing decides whether an event reaches it — so for a
			// panel drag "outside" means a different region, not extra rows.
			m.dragSawOutside = region != m.panelSelRegion
		}
	}
	if cmd, handled := m.handleDragEdgeMouse(ev, region, regionX); handled {
		return m, cmd
	}
	var dragCmd tea.Cmd // set by press/motion; returned at the end
	switch ev.Button {
	case tea.MouseWheelUp:
		if p := m.panelOffsetPtr(region); p != nil {
			if *p > 0 {
				*p--
			}
		} else if m.attached != nil && !m.hasPendingPrompt() {
			dragCmd = m.smoothScroll(&m.attached.vp, -scrollWheelLines)
		} else {
			dragCmd = m.smoothScroll(&m.vp, -scrollWheelLines)
		}
	case tea.MouseWheelDown:
		if p := m.panelOffsetPtr(region); p != nil {
			if *p < m.panelMaxOffset(region, m.viewportHeight()) {
				*p++
			}
		} else if m.attached != nil && !m.hasPendingPrompt() {
			dragCmd = m.smoothScroll(&m.attached.vp, scrollWheelLines)
		} else {
			dragCmd = m.smoothScroll(&m.vp, scrollWheelLines)
		}
	case tea.MouseLeft:
		if region != regionNone {
			return m.handlePanelMouse(region, regionX, ev)
		}
		// Only handle events inside the viewport area — the rows between the
		// measured chrome (header bar above, hints + status bar below).
		vpRowStart, vpRowEnd := m.viewportRows()
		if ev.Y < vpRowStart || ev.Y >= vpRowEnd || region != regionNone {
			break
		}
		// The attach view owns the main area (and therefore selection) with
		// exactly the same condition activeViewport() renders with — selecting
		// must hit the buffer on screen, not the hidden transcript (issue #207).
		// While a pending prompt temporarily re-reveals the transcript, this
		// falls through to the transcript path below.
		if m.attachActive() {
			return m.handleAttachMouse(ev)
		}
		contentLine := m.vp.YOffset() + (ev.Y - vpRowStart)
		switch ev.Action {
		case mousePress:
			if ev.Ctrl && m.selAnchorLine >= 0 {
				// Extend existing selection to clicked position.
				m.selEndLine = contentLine
				m.selEndCol = ev.X
				m.selDragging = true
				m.selText = m.selectionText()
				m.setViewportContent()
				setMouseDragMode(true)
				m.dragResetPending = true
				m.dragResetGen++
				dragCmd = dragResetCmd(m.dragResetGen)
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
			m.dragResetPending = true
			m.dragResetGen++
			dragCmd = dragResetCmd(m.dragResetGen)
		case mouseMotion:
			if m.selAnchorLine >= 0 {
				m.selDragging = true
				m.selEndLine = contentLine
				m.selEndCol = ev.X
				m.setViewportContent()
				m.dragResetGen++
				dragCmd = dragResetCmd(m.dragResetGen) // reschedule: release hasn't arrived yet
			}
		case mouseRelease:
			m.dragResetPending = false
			setMouseDragMode(false)
			if m.selAnchorLine >= 0 {
				if contentLine == m.selAnchorLine && ev.X == m.selAnchorCol {
					// A click without drag on a thinking fold header toggles
					// that block (ADR-0067) — the same arm/act rhythm as panel
					// clicks, and only claimed on exact header lines, so drag
					// selection over reasoning text is untouched.
					if idx := m.thinkBlockAtLine(contentLine); idx >= 0 {
						m.toggleThinkBlock(idx)
					}
					m.clearSelection()
					m.setViewportContent()
					return m, nil
				}
				m.selEndLine = contentLine
				m.selEndCol = ev.X
				m.selText = m.selectionText()
				if cmd := m.copyOnSelectRelease(m.selText); cmd != nil {
					m.clearSelection()
					m.setViewportContent()
					return m, cmd
				}
				m.setViewportContent()
			}
		}
	case tea.MouseRight:
		if ev.Action == mousePress {
			// A drag whose release was dropped can leave mouse mode stuck at 1002;
			// any subsequent click reliably arrives, so reset it defensively here.
			m.dragResetPending = false
			m.dragSawOutside = false
			setMouseDragMode(false)
			// Finalize any in-progress drag selection that lost its release event
			// (release can be dropped when pointer drifts outside viewport bounds).
			if m.selText == "" && m.selAnchorLine >= 0 && m.selDragging {
				m.selText = m.selectionText()
				m.setViewportContent()
			}
			if m.panelSelText == "" && m.panelSelAnchorLine >= 0 && m.panelSelDragging {
				m.panelSelText = selectedText(m.panelSelLines(), m.panelSelAnchorLine, m.panelSelAnchorCol, m.panelSelEndLine, m.panelSelEndCol)
			}
			if m.attachSelText == "" && m.attachActive() && m.attachSelAnchorLine >= 0 && m.attachSelDragging {
				m.attachSelText = m.attachSelectionText()
				m.syncAttachedContent()
			}
			// The view on screen takes priority: attach selection first while
			// attached, then transcript, then panel, then keyboard input —
			// right-clicking must copy what the user can actually see (issue #207).
			if m.attachActive() && m.attachSelText != "" {
				copyToClipboard(m.attachSelText)
				m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(m.attachSelText)))
				m.clearAttachSelection()
				m.syncAttachedContent()
				return m, copyFeedbackClearCmd()
			}
			if !m.attachActive() && m.selText != "" {
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
	return m, dragCmd
}

// welcomeScreen returns a centered welcome message shown when the transcript is empty.
func (m *model) welcomeScreen() string {
	vpH := m.vp.Height()
	if vpH <= 0 {
		vpH = m.viewportHeight()
	}
	localAvail := m.agents.localAvail
	escalationAvail := m.agents.escalationAvail

	lines := []string{
		wordmark(),
		dim(taglineText()),
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
		)
		if m.st.cfg.EscalationAgentIsImplicit() {
			lines = append(lines, dim("no escalation agent configured — you'll be asked before falling back to Claude Code"))
		}
		lines = append(lines, dim("/help for available commands"))
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
			dim("/need — set current goal  ·  F1-F6 — memory/tasks/background/workflow/project/git  ·  /think view — reasoning stream"),
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
		tea.RequestBackgroundColor,
	}
	// Memory panel redraw + content-driven auto-open (ADR-0078) — armed
	// unconditionally (the panel starts closed and the tick is what notices
	// content arriving); cheap next to the 80ms spinner.
	cmds = append(cmds, memoryPollTick())
	// Restored-open data panels (folder-bound state, ADR-0078) populate as
	// if toggled: the /panel toggle is what normally fires the first refresh
	// and arms the 3s poll (commands.go), and loadPanelState never goes
	// through it — without this a restored git/project panel would show its
	// loading row until a focus cycle refreshed it, with no poll behind it.
	if m.st != nil {
		if m.panelProject {
			cmds = append(cmds, projectPollTick(), refreshProjectCmd(m.st.cwd), m.changesetRefreshCmd())
		}
		if m.panelGit {
			cmds = append(cmds, gitPollTick(), refreshGitCmd(m.st.cwd))
		}
	}
	// Idle tip rotation (ADR-0061) — armed unconditionally, cheap at 20s.
	cmds = append(cmds, tipsTick(m.tipsGen))
	if m.credRefreshInit != nil {
		cmds = append(cmds, m.credRefreshInit)
	}
	if m.workflowResumeInit != nil {
		cmds = append(cmds, m.workflowResumeInit)
	}
	if m.mcpConnectInit != nil {
		cmds = append(cmds, m.mcpConnectInit)
	}
	// Pre-warm the @-file candidate index so the first "@" is already served
	// from memory, and redraw hints when a later refresh lands.
	if m.st != nil && m.st.cwd != "" {
		st := m.st
		cmds = append(cmds, func() tea.Msg {
			ix := indexFor(st.cwd)
			ix.setOnReady(func() {
				if st.program != nil {
					st.program.Send(fileIndexReadyMsg{})
				}
			})
			ix.get(0)
			return nil
		})
	}
	if m.st.cfg.ShouldCheckUpdate() {
		cfg := m.st.cfg
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			// updateCheck (not the raw updater call): the skipped-release
			// filter and the last-check write-back apply to the startup
			// check too — a skipped release must not nag again on restart.
			rel, err := updateCheck(ctx, &cfg, activeConfigDir(m.st))
			if err != nil || rel == nil {
				return nil
			}
			return updateAvailableMsg{release: rel, checkedAt: cfg.UpdateLastCheck}
		})
	}
	return tea.Batch(cmds...)
}

// --- Update ---

// Update wraps updateInner to arm the toast-expiry tick whenever toasts are
// visible and no tick is in flight. Emitters (notify) live in helpers that
// return only the model, so the tick is armed centrally here rather than
// threaded through every call site.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, isTick := msg.(animTickMsg); isTick {
		cmd := m.stepAnimations()
		return m, cmd
	}
	next, cmd := m.updateInner(msg)
	nm, ok := next.(model)
	if !ok {
		return next, cmd
	}
	if _, resized := msg.(tea.WindowSizeMsg); !resized {
		// A resize can show or hide a panel without anyone toggling it: no slide.
		if c := nm.trackAnimations(&m); c != nil {
			cmd = tea.Batch(cmd, c)
		}
	}
	if c := nm.armToastTick(); c != nil {
		return nm, tea.Batch(cmd, c)
	}
	return next, cmd
}

func (m model) updateInner(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Ctrl+Enter arrives as bubbletea's unexported unknownCSISequenceMsg
	// ([]byte) in terminals with extended key protocols — detect via its
	// String() representation before the main type switch, which cannot
	// match on the unexported type. The universal fallback (Ctrl+J, which
	// some terminals send for Ctrl+Enter) is handled as tea.KeyCtrlJ inside
	// handleBusyKey.
	if s, ok := msg.(fmt.Stringer); ok && m.inputLocked() && isCtrlEnterCSI(s.String()) {
		input := strings.TrimSpace(stripCompletionPlaceholders(m.ta.Value()))
		if input != "" {
			return m.startUserBackgroundAgent(input, "")
		}
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleResize(msg)

	case tea.KeyPressMsg:
		if m.ptyPane != nil {
			return m.handlePTYKey(msg)
		}
		if m.palette != nil {
			return m.handlePaletteKey(msg)
		}
		if key.Matches(msg, keys.palette) && m.paletteAvailable() {
			return m.openPalette()
		}
		// F1-F6: global panel show/hide shortcuts, available in any other
		// mode (busy, permission prompt, wizards, ...) — none of them
		// otherwise use function keys, and toggling how much of the
		// screen a panel takes doesn't conflict with anything in-progress.
		// Same effect as /panel <name>; having six panels
		// (memory/tasks/background/workflow/project/git)
		// competing for space makes a quick toggle worth more than typing
		// the command out.
		//
		// Alt+Up / Alt+Down: step through the idle tips (same as /tips
		// prev|next) — global for the same reason, and one of the few
		// things still live while a turn streams. bubbles' textarea binds
		// neither key (word motion is alt+left/right and ctrl+left/right),
		// so the browse never costs an editing key.
		if panel := keys.panelFor(msg); panel != "" {
			return m.handlePanelCmd(panel)
		}
		switch {
		case key.Matches(msg, keys.tipPrev):
			cmd := m.tipSeek(-1) // also restarts the cycling timer
			m.tipBrowseEcho()
			return m, cmd
		case key.Matches(msg, keys.tipNext):
			cmd := m.tipSeek(1) // also restarts the cycling timer
			m.tipBrowseEcho()
			return m, cmd
		}
		// Ctrl+G: dismiss all open notification toasts (issue #162) — global
		// like F1-F6 so it works in any mode (busy, prompt, attached…);
		// bubbles' textarea doesn't bind ctrl+g, so nothing else wants it.
		// Falls through untouched when no toast is open.
		if key.Matches(msg, keys.dismissToasts) && m.dismissToasts() {
			return m, nil
		}
		// Adding a pending-state check below? Also add it to
		// hasPendingPrompt() (layout.go) — it mirrors this list so the
		// attach view (ADR-0047) knows to yield the screen to whichever
		// prompt actually needs the user's attention, and nothing else
		// keeps the two lists in sync.
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
		if m.pendingTool != nil {
			return m.handleAgentToolWizardKey(msg)
		}
		if m.pendingBgAgent != nil {
			return m.handleBgAgentWizardKey(msg)
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
		if m.pendingGenericWorkflowExtend != nil {
			return m.handleGenericWorkflowExtendKey(msg)
		}
		if m.attached != nil {
			// Below every pending prompt/wizard check above — attach is a
			// passive viewing state, not a modal one, so a permission prompt
			// or any other decision the user actually needs to make must
			// still reach its own handler rather than being swallowed here.
			return m.handleAttachKey(msg)
		}
		// Pending designer questions take the next Enter even mid-turn: the
		// busy handler would only answer with its "agent is working" hint.
		if m.inputLocked() && m.pendingWorkflowQuestions == "" {
			return m.handleBusyKey(msg)
		}
		return m.handleKey(msg)

	case remoteInputMsg:
		if msg.text == "" {
			return m, nil
		}
		// /interrupt never queues: it exists to act on the turn that would
		// otherwise swallow it (busy → pendingRemoteInputs), and an idle
		// interrupt must not fall through to the model as prompt text.
		// Echoed like any other remote input, then run as the command it is.
		if trimmed := strings.TrimSpace(msg.text); trimmed != "" {
			if cmd, rest, found := extractSlashCommand(trimmed); found && cmd == cmdInterrupt {
				m.appendTranscript(dim("[telegram]") + " " + colorizeTokens(trimmed) + "\n")
				return m.handleSlashInput(cmd, rest)
			}
		}
		if m.busy {
			m.st.pendingRemoteInputs = append(m.st.pendingRemoteInputs, msg.text)
			return m, nil
		}
		return m.submitInput(msg.text, dim("[telegram]")+" ")

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

	case initSmokeMsg:
		m.appendTranscript(msg.text)
		return m, nil

	case claudeLoginDoneMsg:
		if msg.err != nil {
			m.appendTranscript(milkTag() + " login did not finish (" + msg.err.Error() + ") — run `claude auth login` yourself, then /config init again\n")
		} else {
			m.appendTranscript(milkTag() + " " + green("✓ signed in to Claude Code") + "\n")
		}
		if m.pendingInit != nil {
			m.appendTranscript(initWizardRule() + initWizardPrompt(m.pendingInit))
		}
		return m, nil

	case permRequestMsg:
		return m.handlePermRequest(msg)

	case permDismissMsg:
		return m.handlePermDismiss(msg)

	case permTickMsg:
		// Countdown refresh for a timed ask (ADR-0052) — keep ticking only
		// while one is actually pending.
		if m.pendingPerm != nil && !m.pendingPerm.autoAt.IsZero() {
			return m, permTick()
		}
		return m, nil

	case notifyMsg:
		m.notify(msg.text, msg.hint)
		return m, nil

	case oauthRequiredMsg:
		name := "<server-name>"
		if msg.serverName != "" {
			name = msg.serverName
		}
		notice := milkTag() + " MCP OAuth authorization required"
		if msg.authURL != "" {
			notice += "\n" + milkTag() + " authorization URL: " + msg.authURL
		}
		notice += "\n" + milkTag() + " run " + bold("/mcp auth "+name) + " to authorize"
		m.appendTranscript(notice + "\n")
		m.syncLayout()
		return m, nil

	case mcpConnectReadyMsg:
		// Startup MCP connect finished (#161). Toasts only — the transcript
		// stays clean of turn-unrelated startup noise (ADR-0048).
		ok := 0
		for _, r := range msg.results {
			if r.err == nil {
				ok++
				continue
			}
			obs.Info("mcp.connect.ready.failed", "server", r.server, "error", r.err.Error())
			if mcpauth.IsAuthRequired(r.err) {
				m.notify(fmt.Sprintf("MCP %q: authorization required", r.server),
					"/mcp auth "+r.server)
			} else {
				m.notify(fmt.Sprintf("MCP %q: unavailable, will retry on first use", r.server),
					"/mcp reconnect "+r.server)
			}
		}
		if ok > 0 {
			if ok == len(msg.results) {
				m.notify(fmt.Sprintf("MCP: %d server(s) connected", ok), "/mcp list")
			} else {
				m.notify(fmt.Sprintf("MCP: %d of %d servers connected", ok, len(msg.results)), "/mcp list")
			}
		}
		return m, nil

	case mcpOAuthStartedMsg:
		notice := fmt.Sprintf("%s starting OAuth flow for MCP server %q\n%s authorization URL: %s\n%s opening your browser — if it doesn't open, paste the URL above into one\n",
			milkTag(), msg.serverName, milkTag(), msg.authURL, milkTag())
		m.appendTranscript(notice)
		m.syncLayout()
		return m, nil

	case toolUseMsg:
		m.activeToolUse = msg.name
		return m, nil

	case clipboardAttachMsg:
		defer os.Remove(msg.path)
		m = m.handleAttachFile(msg.path, msg.mime)
		return m, nil

	case clipboardNoToolMsg:
		m.appendTranscript(milkTag() + " clipboard paste: no non-text content found — on WSL2 powershell.exe is used automatically; on X11 install " + bold("xclip") + "; on Wayland install " + bold("wl-paste") + "\n")
		return m, nil

	case requestSizeMsg:
		m.currentTurnInputChars = msg.bytes
		return m, nil

	case prefixChunkMsg:
		// The agent-name label is milk's, not the model's: it must neither
		// count as output tokens nor start the tok/s clock (it is written
		// before the request even goes out).
		m.appendTranscriptStreamed(msg.text)
		return m, nil

	case chunkMsg:
		m.markOutput()
		m.currentTurnChars += visibleChars(msg.text)
		m.appendTranscriptStreamed(msg.text)
		// Intra-turn loop detection: feed chunk and check for repetition.
		if m.loopDetector != nil {
			for _, v := range m.loopDetector.FeedChunk(msg.text) {
				m.appendTranscript(yellow("\n" + loopSignalLine(v, loopVerdictMsg(v, m.st.stickyEscalate || m.st.autoStickyEscalate))))
				if v.ShouldInterrupt {
					m.loopInterrupt = true
					m.loopInterruptLabel = loopSignalLabel(v)
					if m.cancelTurn != nil {
						m.cancelTurn()
					}
				}
			}
		}
		return m, nil

	case retractMsg:
		m.retractStreamed(msg.from, msg.to)
		return m, nil

	case thinkChunkMsg:
		m.markOutput()
		m.currentTurnChars += int64(len(msg.text))
		m.currentTurnThinking.WriteString(msg.text)
		m.appendThinkingStreamed(msg.text)
		// Intra-turn loop detection: reasoning chunks get their own (higher)
		// repetition threshold — see internal/loop.
		if m.loopDetector != nil {
			for _, v := range m.loopDetector.FeedReasoningChunk(msg.text) {
				m.appendTranscript(yellow("\n" + loopSignalLine(v, loopVerdictMsg(v, m.st.stickyEscalate || m.st.autoStickyEscalate))))
				if v.ShouldInterrupt {
					m.loopInterrupt = true
					m.loopInterruptLabel = loopSignalLabel(v)
					if m.cancelTurn != nil {
						m.cancelTurn()
					}
				}
			}
		}
		return m, nil

	case reasoningPromotedMsg:
		m.currentTurnThinking.Reset()
		// The streamed reasoning became the turn's answer (it is re-streamed
		// through the content path) — drop this turn's blocks so it isn't
		// rendered twice.
		m.dropThinkBlocksFrom(m.thinkBlocksTurnStart)
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
		concurrentTurn := m.directBashConcurrentTurn
		m.directBashConcurrentTurn = false
		if !concurrentTurn {
			// The PTY/fallback was the only reason busy was true — safe to
			// clear. If it ran alongside an already-in-progress agent turn,
			// that turn's own completion handler owns busy/cancelTurn.
			m.busy = false
			m.cancelTurn = nil
			m.busyHint = ""
		}
		if m.ptyPane != nil {
			// Snapshot the VT screen and append cleaned output to the transcript.
			out := m.ptySnapshot()
			if out != "" {
				m.appendTranscript(dimLines(out))
				m.currentTurnChars += int64(len(out))
				if concurrentTurn && m.st != nil && m.st.sess != nil {
					// Queue for the next dispatch prompt (issue #128) — the
					// turn already in flight when this ran can't see it, and
					// the agent has no special handling for milk's bang
					// syntax, so the output (not the raw "!..." input) is
					// what gets surfaced next turn.
					m.st.sess.PendingBangOutput = append(m.st.sess.PendingBangOutput,
						fmt.Sprintf("[direct command %q completed while agent was busy — output:\n%s]\n", m.ptyPane.shellCmd, out))
				}
			}
			_ = m.ptyPane.ptm.Close()
			m.ptyPane = nil
		}
		if msg.err != nil {
			if _, ok := msg.err.(*exec.ExitError); !ok && !isEOFOrClosed(msg.err) {
				m.appendTranscript(fmt.Sprintf("%s direct-bash error: %v\n", milkTag(), msg.err))
			}
		}
		m.appendTranscript("\n")
		m.refreshPrompt()
		m.syncLayout()
		return m, nil

	case turnWatchNoticeMsg:
		if m.busy {
			m.appendTranscript(dim(msg.text) + "\n")
			m.syncLayout()
		}
		return m, nil

	case workflow.ProgressMsg:
		// Update fields in place rather than replacing m.workflowState
		// wholesale, so AgentMap and the full StageTree (set once at launch) survive every
		// subsequent progress update.
		if m.workflowState == nil {
			m.workflowState = &workflow.State{}
		}
		m.workflowState.ApplyProgress(msg)
		m.autoOpenPanel(regionWorkflow)
		m.lastWorkflowActivity = time.Now()
		m.workflowTimeoutWarned = false
		m.syncLayout()
		return m, nil

	case workflowThinkChunkMsg:
		// Reasoning is consumed into the workflow's own live buffer like every
		// other workflow output — the fold state of transcript think blocks and
		// the ACP thought-relay toggle are both independent of this (ADR-0067).
		if m.workflowState != nil {
			m.workflowState.LiveBuffer().Append([]byte(dim(msg.text)))
			if m.attached != nil && m.attached.kind == attachWorkflow {
				m.syncAttachedContent()
			}
		}
		// Reasoning is activity too: without this a long think trips the idle watchdog.
		m.lastWorkflowActivity = time.Now()
		m.workflowTimeoutWarned = false
		return m, nil

	case workflow.WorkflowChunkMsg:
		// Stage output goes to the workflow's own live buffer, not the main
		// transcript (ADR-0047) — attach via the workflow panel to watch it.
		if m.workflowState != nil {
			m.workflowState.LiveBuffer().Append([]byte(msg.Text))
		}
		if m.attached != nil && m.attached.kind == attachWorkflow {
			m.syncAttachedContent()
		}
		m.lastWorkflowActivity = time.Now()
		m.workflowTimeoutWarned = false
		m.syncLayout()
		return m, nil

	case workflowIdleCheckMsg:
		if msg.gen != m.workflowGen || !m.workflowRunning || m.workflowState == nil {
			return m, nil // workflow finished, was cancelled, or this tick is from an older run — stop rescheduling
		}
		if m.pendingWorkflowQuestions != "" {
			// Waiting on the user, not on an agent: nothing to warn about.
			return m, workflowIdleCheck(m.workflowGen)
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
		return m, workflowIdleCheck(m.workflowGen)

	case workflow.WorkflowQuestionsMsg:
		// Designer identified ambiguities — present questions to user.
		// The next Enter answers them (see the key routing in Update). The
		// workflow goroutine is blocked on cfg.AnswersCh; we forward that exact
		// channel here so the user's reply reaches it.
		m.pendingWorkflowQuestions = msg.Questions
		m.workflowAnswersCh = msg.AnswersCh // use the workflow goroutine's channel
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

	case startWorkflowFromToolMsg:
		reg, regErrs := workflow.LoadRegistry()
		for _, e := range regErrs {
			obs.Info("workflow.registry.load_error", "error", e.Error())
		}
		def, ok := reg.Lookup(msg.ws.Name)
		if !ok {
			// Shouldn't happen — dispatchOneTool already validated the name
			// against the same registry before returning the signal — but
			// the registry re-reads ~/.milk/workflows/ on every LoadRegistry
			// call, so a file removed between the tool call and here would
			// land here instead of failing earlier.
			m.appendTranscript(milkTag() + fmt.Sprintf(" workflow %q no longer found — not starting\n", msg.ws.Name))
			m.refreshPrompt()
			return m, nil
		}
		w := &workflowWizardState{
			name:       msg.ws.Name,
			task:       msg.ws.Task,
			def:        def,
			roles:      def.Roles,
			roleValues: msg.ws.Roles,
		}
		return m.launchGenericWorkflow(w)

	case workflow.WorkflowDoneMsg:
		// The workflow ran in the background: any agent turn in flight owns
		// busy/cancelTurn and its per-turn counters, so none of those are
		// touched here.
		m.workflowRunning = false
		m.cancelWorkflow = nil
		cancelled := m.workflowCancelled
		m.workflowCancelled = false
		m.autoOpenPanel(regionWorkflow)
		if m.workflowState != nil {
			m.workflowState.Role = "done"
			m.workflowState.ActiveStageTree = nil
			runErr := msg.Err
			if runErr == nil && cancelled {
				runErr = context.Canceled
			}
			noteWorkflowFinished(m.st.sess, m.workflowState.WorkflowName, m.workflowState.Task, m.workflowState.WorkflowID, runErr)
		}
		// The run is over (Role "done" just set, workflowRunning false):
		// re-arm auto-open so the *next* workflow launch opens the panel
		// again. The autoOpenPanel call above still ran with the override
		// in place — completion itself never force-opens a panel the user
		// closed mid-run; only future content does (ADR-0059).
		m.resetManualOverrideIfFinalized(regionWorkflow)
		if cancelled || isContextCanceled(msg.Err) {
			m.appendTranscript(dim("[workflow interrupted]") + "\n")
		} else if msg.Err != nil {
			if genericExhausted := (*interp.ExhaustedError)(nil); errors.As(msg.Err, &genericExhausted) && m.workflowState != nil && m.pendingWorkflowWizard == nil {
				// Offer to continue with a doubled iteration limit.
				reg, regErrs := workflow.LoadRegistry()
				for _, e := range regErrs {
					obs.Info("workflow.registry.load_error", "error", e.Error())
				}
				def, defOK := reg.Lookup(m.workflowState.WorkflowName)
				if defOK {
					w := &workflowWizardState{
						name:       m.workflowState.WorkflowName,
						task:       m.workflowState.Task,
						def:        def,
						roles:      def.Roles,
						roleValues: m.workflowState.AgentMap,
						resuming:   true,
						workflowID: m.workflowState.WorkflowID,
					}
					m.pendingGenericWorkflowExtend = &genericWorkflowExtendState{
						wizard: w, stageID: genericExhausted.StageID, maxIterations: genericExhausted.MaxIterations,
					}
					m.appendTranscript(fmt.Sprintf(
						"%s workflow: stage %q exceeded %d iterations — continue with %d? [y/n] ",
						milkTag(), genericExhausted.StageID, genericExhausted.MaxIterations, genericExhausted.MaxIterations*2,
					))
				} else {
					m.appendTranscript(milkTag() + " workflow error: " + msg.Err.Error() + "\n")
				}
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
			m.autoOpenPanel(regionWorkflow)
			if st.Role != "done" {
				m.appendTranscript(fmt.Sprintf(
					"%s workflow %s in progress (sprint %d pass %d) — /workflow resume to continue, or ignore\n",
					milkTag(), st.WorkflowName, st.Sprint, st.Pass,
				))
			}
			m.syncLayout()
		} else if msg.genericName != "" {
			m.workflowState = &workflow.State{WorkflowName: msg.genericName, Task: msg.genericTask, Role: "interrupted"}
			m.autoOpenPanel(regionWorkflow)
			m.appendTranscript(fmt.Sprintf(
				"%s workflow %s in progress (%s) — /workflow resume to continue, or ignore\n",
				milkTag(), msg.genericName, msg.genericTask,
			))
			m.syncLayout()
		}
		return m, nil

	case spinnerTickMsg:
		if m.busy {
			m.spinnerFrame++
			m.flushViewportIfDirty()
			return m, spinnerTick()
		}
		return m, nil

	case tipsTickMsg:
		// A tick scheduled before a manual browse restarted the cycling
		// timer (restartTipsTick) carries an older gen — dropped without
		// advancing or re-arming; the restarted tick is already scheduled.
		if msg.gen != m.tipsGen {
			return m, nil
		}
		// The rotation freezes while a turn runs or a prompt is pending —
		// the tip line's row stays reserved (no layout jump) but the user's
		// attention is elsewhere, so the tip only advances while they are
		// actually idle (ADR-0061). An advance also ends the manual-browse
		// pin (tipPinned), restoring contextual-hint precedence.
		if !m.busy && !m.hasPendingPrompt() && !m.blurred {
			m.tipIdx++
			m.tipPinned = false
		}
		return m, tipsTick(m.tipsGen)

	case copyFeedbackClearMsg:
		m.copyFeedback = ""
		return m, nil

	case busyHintClearMsg:
		m.busyHint = ""
		return m, nil

	case toastTickMsg:
		// Generation check (issue #168's lesson): a tick scheduled before a
		// dismissal or a re-arm carries an older gen and must not clear the
		// armed flag that now belongs to the newer tick.
		if msg.gen != m.toastGen {
			return m, nil
		}
		m.toastTickArmed = false
		m.expireToasts(time.Now())
		// The Update wrapper re-arms the tick if toasts remain visible.
		return m, nil

	case dragResetMsg:
		if !m.dragResetPending {
			return m, nil // already cancelled by a release event
		}
		if msg.gen != m.dragResetGen {
			return m, nil // stale timeout from an earlier scheduling generation
		}
		// An interaction is live (press seen, no release yet) and the pointer
		// never left the drag's own area: the user paused mid-drag, the button
		// is still held. Keep mode 1002 so motion keeps updating the highlight
		// when the pointer resumes, and reschedule the timeout — finalizing
		// here would freeze the selection until release (#168).
		if (m.selAnchorLine >= 0 || m.panelSelAnchorLine >= 0 || m.attachSelAnchorLine >= 0) && (!m.dragSawOutside || m.dragScrollArmed) {
			m.dragResetGen++
			return m, dragResetCmd(m.dragResetGen)
		}
		// The release was dropped — finalize any in-progress selection so the
		// model state stays consistent, then reset the terminal to basic
		// mouse tracking (mode 1000) where wheel-scroll works reliably.
		return m, m.finalizeDroppedDrag()

	case dragScrollMsg:
		return m, m.handleDragScrollTick(msg.gen)

	case credRefreshReadyMsg:
		m.credRefreshing = false
		m.credLabel = msg.label
		if serverName, isMCPOAuth := strings.CutPrefix(msg.label, "MCP OAuth: "); isMCPOAuth {
			if msg.err != nil {
				m.credStatus = msg.err.Error()
				m.credOK = false
				m.appendTranscript(fmt.Sprintf("%s OAuth authorization for MCP server %q failed: %v\n", milkTag(), serverName, msg.err))
			} else {
				m.credStatus = "ok"
				m.credOK = true
				// Informational completion (issue #162): the follow-up command
				// rides along as the toast hint instead of prose in the text.
				m.notify(fmt.Sprintf("OAuth authorization for MCP server %q completed", serverName), "/mcp reconnect "+serverName)
			}
			m.syncLayout()
			return m, nil
		}
		if msg.err != nil {
			m.credStatus = msg.err.Error()
			m.credOK = false
		} else {
			m.credStatus = "ok"
			m.credOK = true
			// Turn-unrelated async event (issue #162): timestamped toast, so
			// periodic Bedrock/token refreshes are visible in history instead
			// of only flickering in the status bar.
			m.notify(msg.label+" credentials refreshed", "")
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
				newAgent.WithOnTokens(func(model, role string, prompt, completion, cacheRead, cacheCreation int64, timing local.StreamTiming) {
					ist.sess.AddTokensFull(model, role, prompt, completion, cacheRead, cacheCreation)
					ist.sess.AddTiming(model, role, timing.TTFT, timing.Decode)
				})
				newAgent, mcpErr := attachMCPToolSet(m.ctx, m.st.cfg, activeLocalAgentConfig(m.st.cfg).Name, newAgent)
				if mcpErr != nil {
					m.appendTranscript(fmt.Sprintf("%s MCP reconnect error: %v\n", milkTag(), mcpErr))
				}
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

	case fileIndexReadyMsg:
		if m.onAtToken() && len(m.tabMatches) == 0 {
			m.rebuildInlineHints()
			m.syncLayout()
		}
		return m, nil

	case memoryRefreshMsg:
		// Content presence is what opens the memory panel (ADR-0078): when
		// the user hasn't managed it and the store gained content, open it
		// like any other panel's content event.
		if !m.panelMemory && !m.panelContentFinalized(regionMemory) {
			m.autoOpenPanel(regionMemory)
		}
		return m, memoryPollTick()

	case projectRefreshMsg:
		if !m.panelProject {
			return m, nil
		}
		if m.blurred {
			return m, projectPollTick() // keep the timer alive, skip the work
		}
		// The changeset's git enrichment rides the same 3s tick: a burst of
		// tool calls between ticks schedules exactly one status refresh
		// (the issue #331 coalescing), off the UI goroutine.
		return m, tea.Batch(projectPollTick(), refreshProjectCmd(m.st.cwd), m.changesetRefreshCmd())

	case gitRefreshMsg:
		if !m.panelGit {
			return m, nil
		}
		if m.blurred {
			return m, gitPollTick()
		}
		return m, tea.Batch(gitPollTick(), refreshGitCmd(m.st.cwd))

	case projectDataMsg:
		m.projectFiles = msg.files
		m.projectTruncated = msg.truncated
		m.rebuildProjectTree()
		// The session trace may have moved since the last snapshot (tool
		// writes between polls) — resync the changeset with this refresh.
		m.syncChangeset()
		return m, nil
	case changesetMsg:
		switch {
		case msg.noRepo:
			m.changesetNoGit = true
			m.changesetGitOK = false
			m.changesetGit = nil
		case msg.gitOK:
			m.changesetNoGit = false
			m.changesetGitOK = true
			m.changesetGit = msg.files
		default: // transient git failure: keep the last snapshot
			return m, nil
		}
		m.syncChangeset()
		return m, nil

	case gitDataMsg:
		m.gitRows = msg.rows
		m.gitBranch = msg.branch
		m.gitDirty = msg.dirty
		m.gitErr = msg.err
		m.gitLines, m.gitIDs = buildGitPanelLines(m.gitRows, m.gitBranch, m.gitDirty, m.gitErr, gitPanelInner)
		return m, nil

	case attachRefreshMsg:
		if m.attached == nil {
			return m, nil
		}
		m.syncAttachedContent()
		return m, attachRefreshTick()

	case taskStoreChangedMsg:
		m.autoOpenPanel(regionTasks)
		// Auto-open first (a "last task completed" change must not force
		// a user-closed panel back open), then the finalization reset so
		// the *next* task change auto-opens again (ADR-0059).
		m.resetManualOverrideIfFinalized(regionTasks)
		m.syncLayout()
		return m, nil

	case backgroundJobStartedMsg:
		m.autoOpenPanel(regionBackground)
		m.syncLayout()
		return m, nil

	case backgroundSpawnedMsg:
		m.notify(fmt.Sprintf("spawned background agent %s on %s (%q)", msg.jobID, msg.model, msg.label), "/bg list")
		m.autoOpenPanel(regionBackground)
		m.syncLayout()
		return m, nil

	case backgroundJobDoneMsg:
		j := msg.job
		// The toast (ADR-0048) stays the transient lifecycle notice; the
		// job's *result* also lands in the transcript now, so it can't pass
		// by unseen when the model never reports it (see
		// renderBackgroundJobDoneBlock).
		m.appendTranscript(renderBackgroundJobDoneBlock(j))
		if j.Err != nil {
			m.notify(fmt.Sprintf("background agent %q failed: %v", j.Label, j.Err), "/bg list")
		} else {
			m.notify(fmt.Sprintf("background agent %q completed", j.Label), "/bg list")
		}
		// The last running job finishing finalizes the background panel's
		// content — re-arm auto-open so the next spawn opens it again
		// (no-op while other jobs are still running; ADR-0059).
		m.resetManualOverrideIfFinalized(regionBackground)
		return m, nil

	case backgroundShellStartedMsg:
		m.autoOpenPanel(regionBackground)
		m.syncLayout()
		return m, nil

	case backgroundShellDoneMsg:
		m.notify(fmt.Sprintf("background shell %s %s", msg.info.ID, msg.info.StatusLabel()), "/bg list")
		// The last running shell (or job) exiting finalizes the background
		// panel's content — re-arm auto-open so the next one opens it again
		// (no-op while other background work is still live; ADR-0059).
		m.resetManualOverrideIfFinalized(regionBackground)
		m.syncLayout()
		return m, nil

	case backgroundBatchDoneMsg:
		return m.maybeAutoFollowupBackgroundJobs(true)

	case backgroundUserJobDoneMsg:
		return m.maybeAutoFollowupBackgroundJobs(false)

	case configReloadMsg:
		if msg.err != nil {
			m.appendTranscript(fmt.Sprintf("%s config reload error: %v\n", milkTag(), msg.err))
			return m, nil
		}
		// Detect whether anything user-visible actually changed. Metadata-only
		// writes (e.g. update_last_check saved on startup) must not show a banner
		// that hides the splash screen.
		oldCfg, newCfg := m.st.cfg, msg.cfg
		oldCfg.UpdateLastCheck, newCfg.UpdateLastCheck = "", ""
		oldCfg.UpdateSkippedVersion, newCfg.UpdateSkippedVersion = "", ""
		contentChanged := !reflect.DeepEqual(oldCfg, newCfg)
		agentChanged := agentConfigChanged(oldCfg, newCfg)
		m.st.cfg = msg.cfg
		if contentChanged {
			m.notify("config reloaded", "/config")
		}
		if agentChanged {
			// The constructed runners pin the old agent configs — rebuild
			// them so agent edits take effect on the next turn without a
			// restart (the same live-rebuild path /agent switch uses).
			var cmd tea.Cmd
			m, cmd = m.rebuildRunners()
			m.appendTranscript(milkTag() + " agents rebuilt from config\n")
			return m, cmd
		}
		m = m.refreshMCPToolSets()
		return m, nil

	case openFileMsg:
		return m.handleOpenFileMsg(msg)

	case updateAvailableMsg:
		m.pendingUpdate = msg.release
		// Last-check bookkeeping was already written (and saved) by
		// updateCheck — which also records it when up to date, so the shared
		// 24h debounce holds. Just sync the in-memory copy here.
		m.st.cfg.UpdateLastCheck = msg.checkedAt
		return m, nil

	case updateProgressMsg:
		m.updateProgress = msg.done
		m.updateTotal = msg.total
		return m, nil

	case updateDoneMsg:
		m.updateInstalling = false
		m.updateProgress = 0
		m.updateTotal = 0
		switch {
		case errors.Is(msg.err, updater.ErrWindowsStaged):
			// Deferred success (Windows rename denied): the download is
			// staged and StartupUpdate swaps it in at the next start —
			// clear the pending notice like a completed install.
			m.pendingUpdate = nil
			m.appendTranscript(milkTag() + " update staged — it will be applied on next start (restart milk to move forward)\n")
		case msg.err != nil:
			m.appendTranscript(fmt.Sprintf("%s update failed: %v\n", milkTag(), msg.err))
		default:
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

	case tea.FocusMsg:
		m.blurred = false
		return m, m.refreshOpenPanelsCmd()

	case tea.BlurMsg:
		m.blurred = true
		return m, nil

	case tea.BackgroundColorMsg:
		if setDarkBackground(msg.IsDark()) {
			refreshThemeColors()
			m.colorizeForce = true
			m.turnColorCache = nil
			m.turnRawCache = nil
			m.viewportDirty = true
		}
		return m, nil

	case tea.PasteMsg:
		return m.routePaste(msg)

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

// handlePaste routes a bracketed paste. File paths become an attach prompt or
// an @-mention; everything else goes to the textarea as one undo step.
// refreshOpenPanelsCmd refreshes the project/git panels right away, used when
// the window regains focus after polling was paused.
func (m model) refreshOpenPanelsCmd() tea.Cmd {
	if m.st == nil {
		return nil
	}
	var cmds []tea.Cmd
	if m.panelProject {
		cmds = append(cmds, refreshProjectCmd(m.st.cwd), m.changesetRefreshCmd())
	}
	if m.panelGit {
		cmds = append(cmds, refreshGitCmd(m.st.cwd))
	}
	return tea.Batch(cmds...)
}

func (m model) handlePaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	// Pre-expand to terminal height so repositionView() inside ta.Update never
	// scrolls on a large paste (updateTA's +1 is insufficient for multi-line pastes).
	// Check whether the pasted content looks like a file path that exists.
	// If so, ask the user whether to attach it rather than insert it as text.
	pasted := strings.TrimSpace(msg.Content)
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
	// The paste lands at the very start of the buffer: the leading token
	// (what extractSlashCommand/stripBangPrefix/shelldetect inspect) is
	// about to become pasted content rather than typed content.
	if m.taCursorOffset() == 0 {
		m.leadingPasted = true
	}
	m.undoPush(false) // paste is always its own undo step; updateTA will skip (same value)
	m.ta.SetHeight(m.height)
	var cmd tea.Cmd
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Any key other than ctrl+c cancels a pending quit confirmation.
	if m.quitPending && msg.String() != "ctrl+c" {
		m.quitPending = false
	}

	// ctrl+r search mode: intercept most keys.
	if m.searching {
		return m.handleSearchKey(msg)
	}

	// "!" as the first character of an empty textarea enters bang mode:
	// consume the leading "!" (don't insert it) and switch the prompt label.
	// Checked against msg.Runes rather than msg.String() == "!" because fast
	// typing (or scripted input) can deliver several characters in a single
	// KeyMsg — e.g. "!echo hi" arriving as one tea.KeyRunes burst — and a
	// plain string match on "!" alone would miss that and insert it literally.
	if !m.bangMode && m.ta.Value() == "" && strings.HasPrefix(msg.Text, "!") {
		m.bangMode = true
		if rest := msg.Text[1:]; rest != "" {
			m.ta.InsertString(rest)
		}
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	}

	switch msg.String() {
	case "backspace":
		// Backspacing an empty buffer while in bang mode exits the mode
		// instead of being a no-op.
		if m.bangMode && m.ta.Value() == "" {
			m.bangMode = false
			m.refreshPrompt()
			return m, nil
		}
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
		// Tab-completion accept and arrow-hint commit live in
		// acceptActiveCompletion so handleBusyKey uses the exact same
		// semantics; only submission (handleEnter) is state-specific.
		var accepted bool
		m, accepted = m.acceptActiveCompletion()
		if accepted {
			return m, nil
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
	case "tab", "shift+tab":
		dir := 1
		if msg.String() == "shift+tab" {
			dir = -1
		}
		return m.handleTabKey(dir), nil
	case "ctrl+t":
		return m.toggleLastThinkBlock(), nil
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
			if msg.Text != "" {
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
	// Buffer emptied out (e.g. backspacing away a paste): nothing pasted
	// remains, so any further typing starts a clean, untainted leading token.
	if m.ta.Value() == "" {
		m.leadingPasted = false
	}
	m.syncLayout()
	return m, tea.Batch(cmd, m.scheduleHintRebuild())
}

// handleShiftArrow manages keyboard selection in the input textarea.
// Shift+Arrow keys extend the selection; the anchor is set on the first shift press.
// The bare direction key is forwarded to the textarea to move the cursor.
// routePaste sends a paste to the right consumer: the PTY pane, the path-aware
// idle handler, or (in any modal state) straight into the textarea.
func (m model) routePaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	if m.ptyPane != nil {
		m.ptyPane.ptm.Write([]byte(msg.Content)) //nolint:errcheck
		return m, nil
	}
	if m.hasPendingPrompt() || m.attached != nil || m.searching || (m.inputLocked() && m.pendingWorkflowQuestions == "") {
		cmd := m.updateTA(msg)
		m.syncLayout()
		return m, cmd
	}
	return m.handlePaste(msg)
}

func (m model) handleShiftArrow(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Set anchor at current cursor position before moving.
	if m.taSelAnchor < 0 {
		m.taSelAnchor = m.taCursorOffset()
	}

	// Map shift+arrow → bare direction for the textarea.
	var bareKey tea.KeyPressMsg
	switch msg.String() {
	case "shift+left":
		bareKey = tea.KeyPressMsg{Code: tea.KeyLeft}
	case "shift+right":
		bareKey = tea.KeyPressMsg{Code: tea.KeyRight}
	case "shift+up":
		bareKey = tea.KeyPressMsg{Code: tea.KeyUp}
	case "shift+down":
		bareKey = tea.KeyPressMsg{Code: tea.KeyDown}
	case "shift+home":
		bareKey = tea.KeyPressMsg{Code: tea.KeyHome}
	case "shift+end":
		bareKey = tea.KeyPressMsg{Code: tea.KeyEnd}
	case "shift+ctrl+left", "shift+alt+left", "ctrl+shift+left":
		bareKey = tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModAlt}
	case "shift+ctrl+right", "shift+alt+right", "ctrl+shift+right":
		bareKey = tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModAlt}
	default:
		bareKey = tea.KeyPressMsg{Code: tea.KeyRight}
	}

	cmd := m.updateTA(bareKey)
	m.taSelEnd = m.taCursorOffset()
	m.syncLayout()
	return m, cmd
}

// extendTranscriptSel extends or reduces the current transcript selection using
// Shift+Arrow keys. The anchor stays fixed; only selEndLine/selEndCol moves.
func (m model) extendTranscriptSel(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
		visible0 := m.vp.YOffset()
		visible1 := m.vp.YOffset() + m.vp.Height() - 1
		if endLine < visible0 {
			m.vp.SetYOffset(endLine)
		} else if endLine > visible1 {
			m.vp.SetYOffset(endLine - m.vp.Height() + 1)
		}
		m.setViewportContent()
	}
	return m, nil
}

func (m model) handleCtrlC() (tea.Model, tea.Cmd) {
	// Stage 0 — undo the most recent queueing (the busy-input disposition's
	// counterpart to the receipt line): one press per entry, before any of
	// the stages below. The chain is therefore: drop last queued prompt →
	// clear input → interrupt the turn → quit (each press advances one).
	if m.st != nil && len(m.st.queuedPrompts) > 0 {
		m.st.queuedPrompts = m.st.queuedPrompts[:len(m.st.queuedPrompts)-1]
		m.busyEnterArmed = false
		m.appendTranscript(milkTag() + fmt.Sprintf("⋯ removed the last queued prompt (%d left) · /queue to review\n", len(m.st.queuedPrompts)))
		m.syncLayout()
		return m, nil
	}
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
	if m.bangMode || m.ta.Value() != "" {
		m.ta.Reset()
		m.bangMode = false
		m.leadingPasted = false

		m.tabMatches = nil
		m.tabIdx = -1
		m.tabCmdIdx = 0
		m.tabVarIdx = 0
		m.tabBeforeCursor = ""
		m.tabAfterCursor = ""
		m.tabPrefix = ""
		m.tabSubcmdMode = false
		m.tabValueMode = false
		m.tabNsLabel = ""
		m.tabSegPrefix = ""
		m.tabHints = nil
		m.tabHintsBase = nil
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	}
	// If designer questions are pending, Ctrl+C cancels the workflow.
	if m.pendingWorkflowQuestions != "" {
		m.pendingWorkflowQuestions = ""
		m.workflowAnswersCh = nil
		m.cancelRunningWorkflow()
		m.appendTranscript("\n" + milkTag() + " workflow cancelled\n")
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	}
	// Interrupt a running turn (busy only — cancelTurn may linger past a
	// finished turn, so gate on m.busy exactly like the busy-key routing).
	if m.busy && m.cancelTurn != nil {
		m.cancelTurn()
		m.cancelTurn = nil
		m.interrupted = true
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
	// Bang mode consumed the "!" at keystroke time so it never lived in the
	// textarea; re-prepend it here so submitInput's stripBangPrefix still
	// recognizes this as a direct-execution command.
	if m.bangMode {
		input = "!" + input
		m.bangMode = false
		m.refreshPrompt()
	}
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
		// This branch never reaches submitInput (the one place that normally
		// consumes and clears leadingPasted), so clear it here.
		m.leadingPasted = false
		// Don't send empty answers — they create empty checkpoint entries
		// that replay badly on resume.  Treat bare Enter as "continue".
		answer := input
		if strings.TrimSpace(answer) == "" {
			answer = "continue"
		}
		m.appendTranscript(promptLabel(m.st) + answer + "\n")
		if m.workflowAnswersCh != nil {
			m.workflowAnswersCh <- answer
			// Don't close the channel — it's reused for subsequent
			// user_checkpoint stages in the same workflow run.
			m.workflowAnswersCh = nil
		}
		m.pendingWorkflowQuestions = ""
		m.lastWorkflowActivity = time.Now()
		m.workflowTimeoutWarned = false
		if m.workflowState != nil {
			// Keep Role as the canonical "designer" (matches AgentMap's key) so
			// the idle watchdog can still resolve the agent's turn timeout.
			m.workflowState.Role = "designer"
		}
		m.appendTranscript(milkTag() + " Answers submitted. Designer finalizing plan...\n")
		m.syncLayout()
		return m, nil
	}

	return m.submitInput(input, promptLabel(m.st))
}

// backgroundFollowupPrompt is the synthetic input used to trigger a real
// follow-up turn once a whole wave of spawn_background_agent jobs has
// finished (ADR-0043). It carries no content of its own — drainBackgroundJobs
// (dispatch.go) prepends the actual drained results to whatever turn picks
// it up; this text only needs to point the model at reacting to them.
//
// Aliases local.BackgroundFollowupPrompt rather than defining its own text:
// Run's isRepeatedPrompt whitelist exempts that exact string from the
// repeated-prompt escalation signal (a fixed synthetic prompt recurring
// across multiple completed waves looks identical to a human repeating
// themselves out of frustration, and would otherwise trigger a bogus
// self-escalation). Defining a second, textually-identical constant here
// would risk the two drifting apart if either ever gets edited alone.
const backgroundFollowupPrompt = local.BackgroundFollowupPrompt

// spawnUserBackgroundAgent spawns a background research job (ADR-0043) from
// text the user typed while the model was busy — Ctrl+Enter (or Ctrl+J) while
// busy with text in the input (see handleBusyKey). Unlike an
// agent-initiated spawn_background_agent tool call, there is no
// requirement that the currently-busy role itself be local-backed (the
// in-flight turn could be running on claude-cli, which has no Manager at
// all) — the job just needs any inference-server-backed agent to fork from,
// so the default prefers the escalation agent's local backend (generally the
// more capable one) and falls back to primary's. spec picks another agent
// (name, "primary" or "escalation"); blank means config's background_agent,
// then that default. startUserBackgroundAgent is the entry that asks for it.
func (m model) spawnUserBackgroundAgent(task, spec string) (tea.Model, tea.Cmd) {
	m.ta.Reset()
	m.leadingPasted = false
	m.busyHint = ""
	m.syncLayout()

	// Staged attachments (/attach, clipboard paste) ride the job instead of
	// stranding when the turn they were staged for never comes: images go as
	// per-call vision parts (the job's vision gate decides from there), text
	// as inline context blocks ahead of the task.
	var jobImages []local.ContentPart
	if len(m.pendingAttachments) > 0 {
		var textBlocks strings.Builder
		for _, a := range m.pendingAttachments {
			if a.isImage() {
				jobImages = append(jobImages, local.ContentPart{
					Type:     "image_url",
					ImageURL: &local.ImageURLPart{URL: attachmentDataURI(a)},
				})
			} else {
				textBlocks.WriteString(attachmentContextBlock(a))
			}
		}
		if textBlocks.Len() > 0 {
			task = textBlocks.String() + "\n" + task
		}
		m.appendTranscript(milkTag() + fmt.Sprintf(" attached %d staged file(s) to the background job\n", len(m.pendingAttachments)))
		m.pendingAttachments = nil
	}

	mgr := m.agents.backgroundMgr
	// Build the job's agent through buildTUIAgents — the same live callback
	// wiring every turn gets (permission asks incl. the background timed
	// answer, tool hints, token accounting). m.agents alone can hold a
	// hand-built copy with those callbacks missing (commitSwitchAgent
	// rebuilds it from config directly), whose unwired permAsk would silently
	// deny every tool the job tries — no prompt, just an error status.
	send := func(msg tea.Msg) { m.st.program.Send(msg) }
	ir0 := &tuiInputReader{send: send}
	if m.agents.toolRunners == nil {
		m.agents.toolRunners = map[string]TurnRunner{}
	}
	tuiAgents, _ := m.buildTUIAgents(send, ir0)
	// Agent choice, model-name fallback, label and spawn body are the shared
	// /bg-start core (userJobPlanFor/userJobRunner, issue #188) — the same
	// plan ACP's spawnUserJob makes. An agent other than the session's own
	// primary/escalation is built like an agent-as-tool runner.
	build := func(name string) (*local.Agent, error) {
		return buildLocalPeerAgent(m.ctx, name, m.st.cfg, &tuiAgents, m.mem, memory.ConsumerAll)
	}
	agent, modelName, label, err := userJobPlanFor(m.st.cfg, tuiAgents.escalationLocal, tuiAgents.local, build, spec, task)
	if mgr == nil || err != nil {
		msg := userJobUnavailableMsg
		if err != nil {
			msg = err.Error()
		}
		m.appendTranscript("\n" + dimWrap("» "+msg) + "\n")
		return m, nil
	}
	cwd := m.st.cwd
	// Perform the spawn inside a tea.Cmd so that the Manager's onStart
	// callback (which calls p.Send on bubbletea's unbuffered msgs channel)
	// runs outside the current Update() call — calling p.Send from within
	// Update deadlocks because the event loop goroutine is the only reader
	// of that channel and it is blocked waiting for Update to return.
	return m, func() tea.Msg {
		job := mgr.Spawn(label, task, "user", modelName, userJobRunner(agent, mgr, cwd, task, m.mem, jobImages))
		return backgroundSpawnedMsg{jobID: job.ID, label: label, model: modelName}
	}
}

// maybeAutoFollowupBackgroundJobs delivers currently-drainable
// spawn_background_agent results as a real follow-up turn once the TUI is
// idle, so the agent actually produces the consolidated response it usually
// promises — rather than leaving results sitting in the Manager's queue
// until the user happens to send another message.
//
// waitForWholeWave distinguishes the two callers:
//   - true (agent-initiated, via the spawn_background_agent tool call and
//     backgroundBatchDoneMsg): the calling agent designed a consolidated,
//     multi-part wave meant to be reported together, so this only fires
//     once every currently-outstanding job has finished (ActiveCount == 0).
//   - false (user-initiated, via the busy-key "Ctrl+Enter to spawn"
//     flow and backgroundUserJobDoneMsg): there is no "wave" to
//     consolidate — the user forked off one specific side-question while
//     waiting on something else, so this fires as soon as the model is
//     idle regardless of whether other jobs (agent- or user-initiated) are
//     still running. Delivering it promptly matters more than batching it
//     with unrelated work the user never asked this request to wait for.
//
// Called both when a job/wave first finishes and again after any turn
// completes (handleAgentDone), in case it finished while busy.
func (m model) maybeAutoFollowupBackgroundJobs(waitForWholeWave bool) (tea.Model, tea.Cmd) {
	mgr := m.agents.backgroundMgr
	if mgr == nil {
		m.pendingBackgroundFollowup = false
		m.pendingUserBackgroundFollowup = false
		return m, nil
	}
	busy := m.busy || m.pendingPerm != nil || m.pendingDirectBash != nil || m.ptyPane != nil
	d := decideFollowup(waitForWholeWave, mgr.ActiveCount(), mgr.PendingCount(), busy)
	applyFollowupDecision(waitForWholeWave, d, &m.pendingBackgroundFollowup, &m.pendingUserBackgroundFollowup)
	if d != followupNow {
		return m, nil
	}
	return m.submitInput(backgroundFollowupPrompt, dim("[background]")+" ")
}

// submitInput handles a finalised user input string from any source (keyboard,
// Telegram, etc.). label is the transcript echo prefix.
func (m model) submitInput(input, label string) (tea.Model, tea.Cmd) {
	if input == "" {
		return m, nil
	}

	// Snapshot and clear: this input cycle's provenance is fully consumed by
	// the gate below, and the field must not leak into whatever the caller
	// (e.g. handleEnter, which resets the textarea before calling submitInput)
	// puts in the buffer next.
	leadingPasted := m.leadingPasted
	m.leadingPasted = false

	// Feed user turn to loop detector (resets turn-flood streak).
	if m.loopDetector != nil {
		m.loopDetector.Feed(loop.TurnSummary{
			Text:       input,
			Timestamp:  time.Now(),
			IsUserTurn: true,
		})
		m.loopInterrupt = false
		m.loopInterruptLabel = ""
		m.loopWarning = ""
	}

	m.appendTranscript(userEcho(label, colorizeTokens(input)))

	// Append to both histories (deduped)
	m.sessionHistory = appendDeduped(m.sessionHistory, input, maxPersistedHistory)
	m.globalHistory = appendDeduped(m.globalHistory, input, maxPersistedHistory)

	// All command/bang/direct-bash triggers below are gated on !leadingPasted:
	// if the leading content of this input arrived via a real paste event
	// rather than being typed, it's treated as inert prompt text (issue #151) —
	// a pasted transcript that happens to start with "/learn" or "!rm -rf" must
	// not execute anything.
	if !leadingPasted {
		if input == cmdPaste {
			// /paste is also the manual trigger for clipboard binary attachment:
			// terminals (especially Windows Terminal / WSL2) never send a bracketed-paste
			// event when the clipboard holds only binary data (image, PDF, etc.), so the
			// automatic empty-paste hook cannot fire. /paste covers that gap.
			return m, m.probeClipboardCmd()
		}

		// "!" prefix: explicit direct-execution mode (Claude Code style). Always
		// available regardless of the direct_bash config, and skips confirmation —
		// the leading "!" is itself an unambiguous request to run a shell command.
		if shellCmd, ok := stripBangPrefix(input); ok {
			if shellCmd == "" {
				return m, nil
			}
			return m.launchPTYPane(shellCmd)
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
	}

	return m.dispatchAgent(input)
}

func (m model) dispatchAgent(input string) (tea.Model, tea.Cmd) {
	m.busy = true
	m.spinnerFrame = 0
	m.currentTurnChars = 0
	m.turnFirstOutputAt = time.Time{}
	m.currentTurnInputChars = 0
	m.currentTurnThinking.Reset()
	m.closeOpenThinkBlock()
	// Turn-boundary tidy: the new turn collapses the previous turns' blocks —
	// only while the fold status is folded (/think expand defines the current
	// default and opts out). Display-only — thinkFoldOpen is untouched.
	m.collapseThinkBlocksBefore(len(m.thinkBlocks))
	m.thinkBlocksTurnStart = len(m.thinkBlocks)
	m.loopInterrupt = false
	m.loopInterruptLabel = ""
	m.loopWarning = ""
	if m.loopDetector != nil {
		m.loopDetector.ResetTurn()
	}

	// Apply pending attachments.
	// Text attachments: prepend context blocks to the agent prompt.
	// Image attachments: multipart vision content parts on the turn ctx
	//   (local.WithImageParts below) — a CLI runner stages its own
	//   @temp-file references from those same parts when it handles the turn
	//   (stageCLIImageRefs), so the prompt carries no @path noise and the
	//   files are guaranteed alive for the whole turn.
	// Session history uses the original input plus compact [attached: name] placeholders.
	attachments := m.pendingAttachments
	m.pendingAttachments = nil // clear before dispatch

	// agentInput: what gets sent to the agent (may include full attachment content).
	// st.pendingSessionContent: compact version stored in session history (placeholders).
	agentInput := input
	var imageParts []local.ContentPart
	if len(attachments) > 0 {
		var textBlocks strings.Builder
		var ph strings.Builder
		for _, a := range attachments {
			fmt.Fprintf(&ph, " %s", attachmentPlaceholder(a))
			if a.isImage() {
				// Multipart image_url content part (base64 data URI) — wired
				// below into whichever *local.Agent(s) handle the turn via the
				// per-call ctx (issue #297). A local escalation agent used to
				// only get this same image dumped as a raw base64 text blob
				// (see the removed inline-text branch this replaced) — which
				// it can't actually see as an image, so it confabulated a
				// plausible-sounding description instead of reading it.
				imageParts = append(imageParts, local.ContentPart{
					Type:     "image_url",
					ImageURL: &local.ImageURLPart{URL: attachmentDataURI(a)},
				})
			} else {
				textBlocks.WriteString(attachmentContextBlock(a))
			}
		}
		var sb strings.Builder
		if textBlocks.Len() > 0 {
			sb.WriteString(textBlocks.String())
			sb.WriteByte('\n')
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

	// Wire image parts into THIS turn's context so whichever local agent the
	// router picks sends a multipart vision payload — routing isn't decided
	// until runTurn below. Per-call ctx, not the agents' pending-image field
	// (issue #297): both local agents are shared with any background
	// workflow running concurrently, and writing the field would race their
	// concurrent reads. local.Run prefers ctx parts; the vision gate and
	// @path CLI path behave exactly as before.
	if len(imageParts) > 0 {
		turnCtx = local.WithImageParts(turnCtx, imageParts)
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
			WithSkipPermissions(st.skipPermissions.Load()).
			WithOAuthRequiredHandler(func(serverName, authURL string) {
				send(oauthRequiredMsg{serverName: serverName, authURL: authURL})
			}).
			WithOnToolUse(func(id, name string) {
				send(toolUseMsg{name: name})
			}).
			WithOnToolUseReady(func(id, name string, input map[string]any) {
				// AskUserQuestion is handled entirely by milk's selection prompt —
				// suppress the » hint here to keep the transcript clean.
				if name == "AskUserQuestion" {
					return
				}
				var hint string
				summary := truncateToolSummary(name, cliToolArgSummary(input), termWidth)
				if summary != "" {
					hint = "\n" + dimWrap("» "+name+": "+summary) + "\n"
				} else {
					hint = "\n" + dimWrap("» "+name) + "\n"
				}
				if st.cfg.RemoteOversight.NotifyToolsEnabled() {
					st.notifier.NotifyToolUse(context.Background(), name, cliToolArgSummary(input))
				}
				if d := cliToolDiff(name, input); d != "" {
					hint += d
				}
				send(chunkMsg{text: hint})
			}).
			WithOnToolResult(func(id, name, result string, isError bool) {
				if st.cfg.RemoteOversight.NotifyToolsEnabled() {
					st.notifier.NotifyToolResult(context.Background(), name, result, isError)
				}
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
	// Both the primary and escalation-local agents share the same store and
	// ask callbacks — they operate in the same cwd and grants should be shared.
	localPermStore := st.localPerms
	localHost := newTUIHost(ir0)
	localPermAsk := makeLocalPermAsk(localHost, localPermStore, st.notifier, st)
	// Background asks (job and workflow-step tool asks) use the full
	// three-source race with the timed answer (ADR-0052): the TUI prompt
	// (countdown + bulk keys), remote oversight, and the configured default
	// at the deadline — never a silent denial.
	localBgPermAsk := func(jobID, tool, summary string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), st.cfg.EffectiveBackgroundAgentTimeout())
		defer cancel()
		p := bgAskPolicy(st.cfg, st.permOverridesNow())
		p.sticky = stickySetter(st.setSessionPermMode, func(note string) {
			localHost.Notify(events.Notification{Text: note})
		})
		sum := jobPermSummary(summary, jobID)
		allow, _ := askPermission(ctx, st.notifier,
			oversight.PermRequest{ToolName: tool, Input: sum},
			func(c context.Context) bool {
				outcome, err := localHost.RequestPermission(c, events.PermissionRequest{
					Prompt:      permAskPromptAuto(tool, sum, autoNote(p.def, p.deadline)),
					Tool:        tool,
					Summary:     sum,
					AutoDefault: autoDefaultAnswer(p.def),
					AutoAt:      time.Now().Add(p.deadline),
				})
				return err == nil && outcome.Allow
			}, p)
		return allow
	}
	// Unattended questions (the ask_user tool from background jobs and
	// workflow steps) use the question race with the timed "no answer" at
	// the bg deadline (ADR-0052) — bounded like the background permission
	// ask so a silent question can never hold a job's concurrency slot.
	localBgUserAsk := func(ctx context.Context, jobID, question string, options []string) (string, bool) {
		ctx, cancel := context.WithTimeout(ctx, st.cfg.EffectiveBackgroundAgentTimeout())
		defer cancel()
		p := bgAskPolicy(st.cfg, st.permOverridesNow())
		note := autoNoteQuestion(p.deadline)
		if jobID != "" {
			note = "job " + jobID + " — " + note
		}
		answer, ok, _ := askUserQuestion(ctx, st.notifier, oversight.InputRequest{Question: question, Options: options},
			func(c context.Context) (string, bool) {
				res, err := localHost.Elicit(c, events.ElicitationRequest{
					Prompt: userAskPrompt(question, options, note),
					Label:  "ask_user",
					AutoAt: time.Now().Add(p.deadline),
				})
				if err != nil || res.Cancelled || strings.TrimSpace(res.Value) == "" {
					return "", false
				}
				return res.Value, true
			}, p)
		return answer, ok
	}
	// Unattended safety confirmations (the doom-loop gate) use the same flow
	// with the safety policy — its own default knob, so one "allow" can't
	// silently re-enable unattended runaway loops.
	localSafetyAsk := func(jobID, tool, summary string) local.SafetyOutcome {
		p := safetyAskPolicy(st.cfg, st.permOverridesNow())
		return safetyAskRun(context.Background(), st.notifier, jobID, tool, summary,
			func(c context.Context) bool {
				sum := jobPermSummary(summary, jobID)
				outcome, err := localHost.RequestPermission(c, events.PermissionRequest{
					Prompt:      permAskPromptAuto(tool, sum, autoNote(p.def, p.deadline)),
					Tool:        tool,
					Summary:     sum,
					AutoDefault: autoDefaultAnswer(p.def),
					AutoAt:      time.Now().Add(p.deadline),
				})
				return err == nil && outcome.Allow
			}, p)
	}
	localOpenFile := func(path string) error {
		respCh := make(chan error, 1)
		send(openFileMsg{path: path, respCh: respCh})
		return <-respCh
	}
	localOnRetract := func(from, to string) {
		send(retractMsg{from: from, to: to})
	}
	localOnToolUse := func(id, name, summary string, rawInput map[string]any) {
		if m.loopDetector != nil {
			// A committed tool call is observable progress: the reasoning-
			// chunk flood signal counts reasoning *since* the last content
			// output or tool call, so a tool-heavy turn never accumulates
			// toward its limit (FeedChunk covers the content half).
			m.loopDetector.FeedProgress()
		}
		if st.cfg.RemoteOversight.NotifyToolsEnabled() {
			st.notifier.NotifyToolUse(context.Background(), name, summary)
		}
	}
	localOnToolResult := func(id, name, result string, isError bool) {
		if st.cfg.RemoteOversight.NotifyToolsEnabled() {
			st.notifier.NotifyToolResult(context.Background(), name, result, isError)
		}
	}
	// The host wiring — one struct applied by wireLocalAgent (issue #188).
	// It also goes onto dispatchAgents so lazily-built agent-as-tool runners
	// ask exactly like the session's own agents instead of never asking.
	wiring := localAgentWiring{
		permStore:           localPermStore,
		permAsk:             localPermAsk,
		bgPermAsk:           localBgPermAsk,
		safetyAsk:           localSafetyAsk,
		userAsk:             makeLocalUserAsk(localHost, st.notifier),
		bgUserAsk:           localBgUserAsk,
		skipPerms:           st.skipPermissions.Load,
		permMode:            sessionPermModeGetter(st.permOverridesNow),
		onOpenFile:          localOpenFile,
		onRetract:           localOnRetract,
		onToolUse:           localOnToolUse,
		onToolResult:        localOnToolResult,
		onThinking:          func(text string) { send(thinkChunkMsg{text: text}) },
		onReasoningPromoted: func() { send(reasoningPromotedMsg{}) },
		taskStore:           m.taskStore,
		mgr:                 agents.backgroundMgr,
	}
	if agents.local != nil {
		tuiLocalAgent := wireLocalAgent(agents.local, wiring)
		tuiAgents.local = tuiLocalAgent
		tuiAgents.primary = newLocalRunner(tuiLocalAgent, agents.primary.Name())
	}
	if agents.escalationLocal != nil {
		tuiEscLocal := wireLocalAgent(agents.escalationLocal, wiring)
		tuiAgents.escalationLocal = tuiEscLocal
		// agents.escalation can legitimately be nil in minimal embeddings
		// (tests, single-agent setups) — fall back to the configured name
		// instead of assuming the old runner exists.
		escName := m.st.cfg.EscalationAgentConfig().Name
		if agents.escalation != nil {
			escName = agents.escalation.Name()
		}
		tuiAgents.escalation = newLocalRunner(tuiEscLocal, escName)
	}
	tuiAgents.agentWiring = wiring
	return tuiAgents, cliPC
}

// renderScrollbar returns a single-column string of h lines showing a dim │
// track with a bright ▌ thumb proportional to scroll position.
// --- Memory panel poll ---

// applyMemoryContentDefault seeds the memory panel's startup state from
// content presence (ADR-0078): the panel is closed by default and opens
// only when the percept store actually has content — any scope, session
// or global. A remembered manual choice wins over this check (loading it
// sets the panel's state and its manual override); quiet by design — at
// startup the panel appearing is the screen itself, not a surprise event
// needing a toast.
func (m *model) applyMemoryContentDefault() {
	// Master switch (ADR-0087, /panel autoopen): off leaves the panel
	// closed at startup too — the stored content is not "new content" the
	// user asked to be moved by.
	if !m.panelAutoOpen {
		return
	}
	if m.panelManualOverride[regionMemory] {
		return
	}
	if !m.panelContentFinalized(regionMemory) {
		m.panelMemory = true
	}
}

func memoryPollTick() tea.Cmd {
	return tea.Tick(memoryPollInterval, func(time.Time) tea.Msg {
		return memoryRefreshMsg{}
	})
}

// workflowIdleCheckInterval is how often the workflow idle watchdog checks
// elapsed time against the current role's agent turn timeout.
const workflowIdleCheckInterval = 15 * time.Second

func workflowIdleCheck(gen int) tea.Cmd {
	return tea.Tick(workflowIdleCheckInterval, func(time.Time) tea.Msg {
		return workflowIdleCheckMsg{gen: gen}
	})
}

// --- Spinner ---

func spinnerTick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// --- Agent dispatch ---

// firstInputReader returns the first of the optional input readers, or nil.
func firstInputReader(ir []inputReader) inputReader {
	if len(ir) == 0 {
		return nil
	}
	return ir[0]
}

// runTurn routes a prompt to the appropriate agent, writing output to out.
func runTurn(ctx context.Context, st *interactiveState, rtr *router.Router, agents *dispatchAgents, input string, out io.Writer, ir ...inputReader) error {
	// sessionContent is the compact version stored in history (with
	// attachment placeholders); it falls back to input when no override is
	// set. Consume the override up front: everything below can early-return
	// (routing error, the Claude gate), and a leftover value would otherwise
	// be recorded as the NEXT turn's history content.
	sessionContent := input
	if st.pendingSessionContent != "" {
		sessionContent = st.pendingSessionContent
		st.pendingSessionContent = ""
	}
	// An agent that read unavailable when the runners were built may just not
	// have been up yet — re-check instead of freezing the fallback until a
	// restart (see refreshTurnAvailability).
	localAvail, escalationAvail := refreshTurnAvailability(ctx, rtr, agents)

	// Route on sessionContent (the user's text with attachment placeholders),
	// not the full agent input: inlined attachment content inflates the
	// prompt past the router's token-threshold rule and used to flip turns to
	// escalation just because a big file was attached.
	rt, routeErr := routeTurn(ctx, st, rtr, sessionContent, localAvail, escalationAvail)
	if routeErr != nil {
		return routeErr
	}
	// Before anything of the turn shows up: running on the implicit Claude
	// fallback needs the user's approval, and a turn with nowhere to run fails
	// here with a clear message instead of an exec error.
	var fallbackHost events.Host
	if tir, ok := firstInputReader(ir).(*tuiInputReader); ok {
		fallbackHost = newTUIHost(tir)
	}
	if err := gateClaude(ctx, st, rt, fallbackHost, st.notifier); err != nil {
		return err
	}
	ctx = withClaudeConsent(ctx, selfEscalationConsent(st, fallbackHost, st.notifier))
	target := rt.Target
	st.activeFallbackTarget = rt.Fallback
	defer func() { st.activeFallbackTarget = "" }()

	targetName := "local"
	agentName := st.cfg.ActiveAgent().Name
	agentCfg := st.cfg.ActiveAgent()
	if target == router.TargetEscalation {
		targetName = "escalation"
		agentCfg = st.cfg.EscalationAgentConfig()
		agentName = agentCfg.Name
	}

	// Idle watchdog (issue #305), replacing the old fixed one-shot timer:
	// the warning measures inactivity — the clock resets on real activity
	// (streamed output via the writers below, tool start/end and stream
	// retries via local.WithActivityNotify) — and re-arms per idle stretch.
	// The turn context still has no hard deadline by default; only the
	// opt-in limits.turn_deadline_secs ends a turn (wrap-up summary first,
	// hard stop only if it will not end).
	timeout := st.cfg.AgentTurnTimeout(agentCfg)
	deadline := st.cfg.AgentTurnDeadline(agentCfg)
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wrapUp := make(chan struct{})
	turnCtx = local.WithTurnWrapUp(turnCtx, wrapUp)
	var runner TurnRunner = agents.primary
	if target == router.TargetEscalation {
		runner = agents.escalation
	}
	// Steerable = milk's own tool loop (local.Agent), which can be told to
	// wrap up with a forced closing summary; CLI/subprocess runners cannot.
	_, steerable := runner.(*localRunner)
	send := func(text string) {
		if st.program != nil {
			st.program.Send(turnWatchNoticeMsg{text: text})
		}
	}
	watch := newTurnIdleWatch(agentName, timeout, deadline, steerable,
		func() { send(turnWatchWarnText(agentName, timeout)) },
		func() {
			close(wrapUp)
			send(turnWatchWrapUpText(agentName, deadline))
		},
		func() {
			send(turnWatchStopText(agentName))
			cancel()
		})
	watch.Touch()
	defer watch.Close()
	turnCtx = local.WithActivityNotify(turnCtx, watch.Touch)
	go watch.Start(turnCtx)
	st.notifier.NotifyTurnStart(turnCtx, agentName, targetName, input)

	sourceLabel := turnSource(st)
	turnStart := time.Now()
	var turnErr error
	var pw io.Writer
	if sw, ok := out.(*sendWriter); ok {
		pw = &prefixWriter{send: sw.send}
	} else {
		pw = out
	}
	// Every byte the agent streams to the host is liveness (issue #305).
	out = &touchWriter{w: out, touch: watch.Touch}
	pw = &touchWriter{w: pw, touch: watch.Touch}
	// onResponse is called by the dispatch layer with the agent's final text,
	// giving us the exact text from this turn without re-scanning history.
	var lastResponseText string
	onResponse := func(text string) { lastResponseText = text }

	// onSegment forwards each completed text chunk to remote oversight as it
	// happens (interleaved with tool-call notifications), instead of waiting
	// for the whole turn to finish. Only cliRunner/localRunner call it; other
	// runners leave segmentsFired false and the final NotifyResponse below
	// (using the full lastResponseText) covers them as before.
	var segmentsFired bool
	onSegment := func(text string) {
		segmentsFired = true
		st.notifier.NotifyResponse(turnCtx, agentName, text)
	}

	// onWorkflowStart runs on this turn's own goroutine, not the bubbletea
	// Update() loop — it can't mutate model state or launch the workflow
	// itself (see local.WorkflowStartSignal's doc comment). Sending a
	// tea.Msg is the only safe way to hand this back to Update(), the same
	// way every other cross-goroutine turn event already reaches the model.
	onWorkflowStart := func(ws *local.WorkflowStartSignal) {
		if st.program != nil {
			st.program.Send(startWorkflowFromToolMsg{ws: ws})
		}
	}

	switch target {
	case router.TargetLocal:
		if mem := st.mem; mem != nil {
			defer func() {
				_ = mem.Consolidate()
				_ = mem.PruneGlobal(st.cfg.PerceptStoreSizeLimit())
			}()
		}
		turnErr = runPrimaryWithSession(turnCtx, st.cfg, st.sess, agents.primary, agents.escalation, st.mem, input, sessionContent, out, agents, onResponse, onSegment, onWorkflowStart, pw)
	case router.TargetEscalation:
		turnErr = runEscalationWithSession(turnCtx, st.cfg, st.sess, agents.escalation, "", st.mem, input, sessionContent, out, agents, onResponse, onSegment, onWorkflowStart, pw)
	}
	recordTurn(turnCtx, target, sourceLabel, turnStart, turnErr)
	st.notifier.NotifyTurnDone(turnCtx, agentName, turnErr)
	if turnErr == nil {
		if !segmentsFired && lastResponseText != "" {
			st.notifier.NotifyResponse(turnCtx, agentName, lastResponseText)
		}
		noteTurnSucceeded(st, rt)
	}
	return turnErr
}

// --- runREPL entry point ---

func runREPL(cfg config.Config, cwd string, initialFlagNew bool, initialFlagSession string, startupWarning string) error {
	// Capture the launch state before anything can switch directories
	// (/resume into another project) — /restart replays exactly this
	// (issue #258).
	capturedLaunch = captureLaunchInfo()
	sess, err := openSession(cwd, initialFlagNew, initialFlagSession)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	// A session resolved by id can live under another directory than the
	// launch cwd — reload so the currently selected dir's project-local
	// config merges (and the agents built below see it), not the launch
	// dir's.
	if sess.CWD != "" && sess.CWD != cwd {
		if fresh, ferr := config.LoadMergedIn(sess.CWD); ferr == nil {
			cfg = fresh
		}
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
		if m, err := memory.NewStoreParams(dir, sess.ID, memParams(cfg)); err == nil {
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
	// (subprocess, aider-cli), bypass the HTTP local agent. When it's
	// claude-cli, bypass both — no cheap classifier is available for it (the
	// router already treats a nil *local.Agent as "skip step 4, attempt
	// primary directly", same as for subprocess primaries).
	tuiPrimaryAC := cfg.ActiveAgent()
	var localAgent *local.Agent
	var tuiSubprocessPrimaryAgent *subprocess.Agent
	var tuiPrimaryCLIAgent *claude.Agent
	if tuiPrimaryAC.IsCLI() {
		tuiPrimaryCLIAgent = newCLIAgent(tuiPrimaryAC)
		tuiPrimaryCLIAgent = applyAWSCreds(cfg, tuiPrimaryCLIAgent)
		tuiPrimaryCLIAgent = tuiPrimaryCLIAgent.WithLogContext(cfg.Otel.LogContext)
		if dbg, err := openCLIDebugLog(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "%s warning: cannot open claude debug log: %v\n", milkTag(), err)
		} else if dbg != nil {
			defer dbg.Close()
			tuiPrimaryCLIAgent = tuiPrimaryCLIAgent.WithDebugLog(dbg)
		}
	} else if tuiPrimaryAC.IsExternalProcess() {
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
			escalationLocalAgent = escalationLocalAgent.WithSkipPermissions(skipPermissionsFor(cfg, escAC))
			if lp, err := local.OpenPermStore(cwd); err == nil {
				escalationLocalAgent.WithPermissions(lp, nil)
			}
			if dbg, err := openLocalDebugLog(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "%s warning: cannot open local debug log: %v\n", milkTag(), err)
			} else if dbg != nil {
				defer dbg.Close()
				escalationLocalAgent = escalationLocalAgent.WithDebugLog(dbg)
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

	// ctx scopes everything owned by *this* process instance: the
	// local-server launcher, MCP connects, the background-job manager
	// (newBackgroundManager derives its base context from it — its
	// cancellation is "milk shutting down", see local.NewManager) and the
	// oversight poller (StartPolling runs until ctx is cancelled). It is
	// cancelled when runREPL returns — and explicitly right before
	// /restart relaunches: the Windows handover (restart_exec_windows.go)
	// keeps this process alive as a waiter until the relaunched instance
	// exits, so these subsystems must stop first — otherwise two oversight
	// pollers fight over the bot's getUpdates stream (HTTP 409) and jobs
	// would outlive the session they belonged to.
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

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
	if tuiPrimaryCLIAgent != nil {
		localAvail = tuiPrimaryCLIAgent.Ping() == nil
		escalationAvail = true // CLI/local escalation checked lazily
		if !localAvail {
			fmt.Fprintln(os.Stderr, milkTag()+" warning: "+tuiPrimaryAC.Name+" primary agent unreachable")
		}
	} else if tuiSubprocessPrimaryAgent != nil {
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
		localAvail, escalationAvail, _ = checkAgentAvailability(ctx, cfg, localAgent, cliAgent)
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

	st := &interactiveState{sess: sess, cwd: cwd, cfg: cfg, mem: mem, cs: cs, localPerms: localPerms, toolFutures: map[string]chan string{}, notifier: newNotifier(cfg)}
	st.skipPermissions.Store(skipPermissionsFor(cfg, cfg.ActiveAgent()))

	// Wire token persistence callbacks now that st is available; closures reference
	// st.sess so they always write to the current session even after /new.
	if localAgent != nil {
		localAgent.WithOnTokens(func(model, role string, prompt, completion, cacheRead, cacheCreation int64, timing local.StreamTiming) {
			st.sess.AddTokensFull(model, role, prompt, completion, cacheRead, cacheCreation)
			st.sess.AddTiming(model, role, timing.TTFT, timing.Decode)
		})
		localAgent.WithOnRequestSize(func(bytes int64) {
			if st.program != nil {
				st.program.Send(requestSizeMsg{bytes: bytes})
			}
		})
	}
	if escalationLocalAgent != nil {
		escalationLocalAgent.WithOnTokens(func(model, role string, prompt, completion, cacheRead, cacheCreation int64, timing local.StreamTiming) {
			st.sess.AddTokensFull(model, role, prompt, completion, cacheRead, cacheCreation)
			st.sess.AddTiming(model, role, timing.TTFT, timing.Decode)
		})
		escalationLocalAgent.WithOnRequestSize(func(bytes int64) {
			if st.program != nil {
				st.program.Send(requestSizeMsg{bytes: bytes})
			}
		})
	}

	// Build TurnRunner instances for dispatch.
	mcpToolSets := map[string]*mcp.ToolSet{}
	mcpServersSeen := map[string][]config.MCPServerConfig{}
	// mcpConnectTSS collects every toolset built below (primary + escalation);
	// the deferred startup connect (#161, m.mcpConnectInit) connects all of
	// them in parallel once the TUI event loop is running, instead of
	// blocking here before the TUI ever renders.
	var mcpConnectTSS []*mcp.ToolSet
	var primaryRunner TurnRunner
	switch {
	case tuiPrimaryCLIAgent != nil:
		r := newCLIRunner(tuiPrimaryCLIAgent, tuiPrimaryAC.Name,
			permContext{cs: cs, cwd: cwd}, func() inputReader { return newStdinInputReader() })
		if servers := cfg.EffectiveMCPServers(tuiPrimaryAC.Name); len(servers) > 0 {
			r = r.withMCPServers(servers)
			mcpServersSeen[tuiPrimaryAC.Name] = servers
		}
		primaryRunner = r
	case tuiSubprocessPrimaryAgent != nil:
		r := newSubprocessRunner(tuiSubprocessPrimaryAgent, tuiPrimaryAC.Name)
		if tuiSubprocessPrimaryAgent.SupportsNativeMCP() {
			// Native MCP (smolagent, issue #122): the script connects to the
			// assigned servers itself each turn — milk only carries the
			// resolved server list, and deliberately builds no ToolSet (no
			// competing connection, no context-injection block).
			if servers := cfg.EffectiveMCPServers(tuiPrimaryAC.Name); len(servers) > 0 {
				r = r.withMCPServers(servers)
				mcpServersSeen[tuiPrimaryAC.Name] = servers
			}
		} else if servers, ts := newMCPToolSet(cfg, tuiPrimaryAC.Name); ts != nil {
			r = r.withMCPToolSet(servers, ts)
			mcpToolSets[tuiPrimaryAC.Name] = ts
			mcpServersSeen[tuiPrimaryAC.Name] = servers
			mcpConnectTSS = append(mcpConnectTSS, ts)
			defer ts.Close(context.Background())
		}
		primaryRunner = r
	case localAgent != nil:
		if servers, ts := newMCPToolSet(cfg, tuiPrimaryAC.Name); ts != nil {
			localAgent = localAgent.WithMCPToolSet(ts)
			mcpToolSets[tuiPrimaryAC.Name] = ts
			mcpServersSeen[tuiPrimaryAC.Name] = servers
			defer ts.Close(context.Background())
		}
		primaryRunner = newLocalRunner(localAgent, tuiPrimaryAC.Name)
	}
	var escalationRunner TurnRunner
	switch {
	case tuiSubprocessAgent != nil:
		r := newSubprocessRunner(tuiSubprocessAgent, tuiEscAC.Name)
		if tuiSubprocessAgent.SupportsNativeMCP() {
			// Native MCP (smolagent, issue #122) — see the primary branch.
			if servers := cfg.EffectiveMCPServers(tuiEscAC.Name); len(servers) > 0 {
				r = r.withMCPServers(servers)
				mcpServersSeen[tuiEscAC.Name] = servers
			}
		} else if servers, ts := newMCPToolSet(cfg, tuiEscAC.Name); ts != nil {
			r = r.withMCPToolSet(servers, ts)
			mcpToolSets[tuiEscAC.Name] = ts
			mcpServersSeen[tuiEscAC.Name] = servers
			mcpConnectTSS = append(mcpConnectTSS, ts)
			defer ts.Close(context.Background())
		}
		escalationRunner = r
	case escalationLocalAgent != nil:
		if servers, ts := newMCPToolSet(cfg, tuiEscAC.Name); ts != nil {
			escalationLocalAgent = escalationLocalAgent.WithMCPToolSet(ts)
			mcpToolSets[tuiEscAC.Name] = ts
			mcpServersSeen[tuiEscAC.Name] = servers
			mcpConnectTSS = append(mcpConnectTSS, ts)
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
			mcpServersSeen[cliAC.Name] = servers
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
		mcpServersSeen:    mcpServersSeen,
		// Constructed once per session, not per turn (ADR-0043) — shared
		// construction in newBackgroundManager (issue #188).
		backgroundMgr: newBackgroundManager(ctx, cfg, sess.ID),
	}

	m := newModel(ctx, st, rtr, agents, mem)
	if m.rememberPanelState {
		if dir, err := config.Dir(); err == nil {
			// Panel state is folder-bound (ADR-0078 amendment): the
			// session's folder — the same scoping rule config views use
			// (activeConfigDir) — picks the file, so preferences follow the
			// project and never leak between folders. Bound once at startup:
			// /new //resume //clear change the conversation, not the folder
			// the layout belongs to.
			folder := activeConfigDir(st)
			m.panelStateFolder = folder
			m.loadPanelState(adoptLegacyPanelState(dir, panelStatePathFor(dir, folder)))
		}
	}
	m.applyMemoryContentDefault()
	m.taskStore = taskStore
	m.hasInferenceAgent = cfg.HasInferenceAgent()
	if startupWarning != "" {
		m.startupWarnings = append(m.startupWarnings, startupWarning)
	}
	for _, w := range config.Validate(cfg) {
		m.startupWarnings = append(m.startupWarnings, w.String())
	}
	// Session-start advisory: a milk-loop agent with no known context window
	// and no forced tool-iteration budget runs on the built-in fallback —
	// say so once, with the two config keys that define it (the status bar
	// has no room for the how-to).
	if n := cfg.AgentIterationBudgetNotice(agentConfigForRole(cfg, RolePrimary), "primary"); n != "" {
		m.startupWarnings = append(m.startupWarnings, n)
	}
	if n := cfg.AgentIterationBudgetNotice(agentConfigForRole(cfg, RoleEscalation), "escalation"); n != "" {
		m.startupWarnings = append(m.startupWarnings, n)
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
			id, kind, err := workflow.CurrentWorkflowID(stateDir, sessID)
			if err != nil || kind == workflow.WorkflowKindNone {
				return workflowResumeCheckMsg{}
			}
			if kind == workflow.WorkflowKindInterp {
				cp, err := interp.LoadCheckpoint(workflow.InterpCheckpointPath(stateDir, sessID, id))
				if err != nil || cp == nil || cp.Done {
					return workflowResumeCheckMsg{}
				}
				return workflowResumeCheckMsg{genericName: cp.DefinitionName, genericTask: cp.Task}
			}
			st, err := workflow.LoadState(workflow.StatePath(stateDir, sessID, id))
			if err != nil || st == nil {
				return workflowResumeCheckMsg{}
			}
			return workflowResumeCheckMsg{state: st}
		}
	}

	// Startup MCP connect (#161) is deferred to Init() for the same reason
	// as credRefreshInit/workflowResumeInit above: connecting can block on
	// OAuth token resolution, token_cmd execution, or network, and must not
	// gate the TUI's first render. tss is captured here (not read from
	// mcpConnectTSS lazily) so the cmd closure doesn't depend on this
	// function's local variables outliving it.
	if tss := mcpConnectTSS; len(tss) > 0 {
		m.mcpConnectInit = func() tea.Msg {
			return mcpConnectReadyMsg{results: connectToolSets(ctx, tss)}
		}
	}

	setTheme(st.cfg.Theme)
	// The panel state and the memory default above were applied before the
	// theme was known; tabs are exclusive (ADR-0079), so a sibling session's
	// multi-open state collapses to its front panel now that the tabbed
	// theme (if chosen) is active. No-op on theme "off".
	m.enforceTabbedPanelExclusivity()
	animationsEnabled = isTTY && st.cfg.AnimationsEnabled()
	defer applyThemeTerminal()()
	p := tea.NewProgram(m)
	clipboardProgram = p
	st.program = p

	// Notify the TUI the first time a *later* (post-startup) tool call hits
	// a server that needs the interactive /mcp auth flow (#161) — the
	// startup connect above only covers the initial attempt; this covers
	// every lazy reconnect after that. No authURL yet (the user still has
	// to run /mcp auth themselves to get one); oauthRequiredMsg already
	// renders fine with authURL empty.
	for _, ts := range mcpConnectTSS {
		ts.WithOnAuthRequired(func(serverName string) {
			p.Send(oauthRequiredMsg{serverName: serverName})
		})
	}

	// Notify the TUI as soon as each background job (ADR-0043) completes,
	// independent of the turn-boundary drain path.
	if agents.backgroundMgr != nil {
		agents.backgroundMgr.SetOnStart(func(*local.Job) {
			p.Send(backgroundJobStartedMsg{})
		})
		agents.backgroundMgr.SetOnDone(func(j *local.Job) {
			p.Send(backgroundJobDoneMsg{job: j})
			// User-initiated jobs (Role == "user", tagged by the busy-key
			// spawn flow) have no "wave" to consolidate — deliver as soon
			// as the model is free rather than waiting for
			// SetOnBatchDone, which only fires once every job (agent- or
			// user-initiated) is done.
			if j.Role == "user" {
				p.Send(backgroundUserJobDoneMsg{})
			}
		})
		// And, once the whole wave has finished, actually trigger a
		// follow-up turn (see maybeAutoFollowupBackgroundJobs) — without
		// this, "I'll follow up automatically" is never true: results just
		// sit in the Manager's queue until some other turn happens to be
		// dispatched.
		agents.backgroundMgr.SetOnBatchDone(func() {
			p.Send(backgroundBatchDoneMsg{})
		})
	}
	// Background shells (ADR-0063/0064) are process-global — surfaced here
	// the same way so a shell's start/end reaches the TUI regardless of
	// which agent's tool loop started it.
	local.AddShellListener(
		func(info local.ShellInfo) { p.Send(backgroundShellStartedMsg{info: info}) },
		func(info local.ShellInfo) { p.Send(backgroundShellDoneMsg{info: info}) },
	)

	// Wire task store redraw: when tasks change, send a tick to trigger View()
	// and (unless the user has manually closed it) auto-open the tasks panel.
	if taskStore != nil {
		taskStore.SetOnChange(func() {
			p.Send(taskStoreChangedMsg{})
		})
	}

	// Start a config watcher so the TUI updates automatically when config.json
	// changes on disk (e.g. the user edits it in another terminal). The dual
	// watcher monitors both the global config and the startup session's local
	// .milk/config.json (the selected dir — sess.CWD), deep-merging them on
	// every change. A later /resume into another directory re-merges inline
	// (see handleSlash's session-switch block); the watcher keeps watching the
	// startup binding's file.
	if cfgDir, cfgDirErr := config.Dir(); cfgDirErr == nil {
		cfgPath := cfgDir + "/config.json"
		localPath, _ := config.LocalConfigPathIn(sess.CWD)
		watcher, watchErr := config.NewDualWatcher(cfgPath, localPath, func(newCfg config.Config, err error) {
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
	enableMouseTracking()
	finalModel, err := p.Run()
	os.Stdout.WriteString("\x1b[?1006l\x1b[?1002l\x1b[?1000l") //nolint:errcheck

	if fm, ok := finalModel.(model); ok {
		if gp, err := globalHistoryPath(); err == nil {
			writeHistoryFile(gp, fm.globalHistory)
		}
		sessID := sess.ID
		if fm.st != nil && fm.st.sess != nil {
			sessID = fm.st.sess.ID
		}
		if sp, err := sessionHistoryPath(sessID); err == nil {
			writeHistoryFile(sp, fm.sessionHistory)
		}
	}
	if mem != nil {
		_ = mem.Consolidate()
		_ = mem.PruneGlobal(cfg.PerceptStoreSizeLimit())
	}
	// /restart (issue #258): the TUI quit with a relaunch plan armed. The
	// terminal is restored above (p.Run teardown + mouse-mode reset), so the
	// exec happens here — on Unix it never returns on success; on Windows
	// (restart_exec_windows.go) this process returns only once the
	// relaunched instance has exited.
	if fm, ok := finalModel.(model); ok && fm.restartPlan != nil {
		// Quiesce process-global subsystems before handing over: the
		// Windows waiter path keeps this process alive for the whole
		// next session, and a still-running oversight poller would race
		// the relaunched instance for the bot's update stream. Same
		// order on Unix (the exec below discards them a moment later —
		// and cancelling first lets a running job persist its state as
		// stopped instead of leaving a stale heartbeat behind).
		cancelCtx()
		if rerr := execRestart(fm.restartPlan.Argv, fm.restartPlan.Dir); rerr != nil {
			return fmt.Errorf("restart failed: %w", rerr)
		}
	}
	return err
}

// loopSignalLabel returns the UI category label for a loop.Verdict:
// "consumption" for consumption/volume threshold crossings (not evidence of
// repetition) and "loop" for genuine repetition-based loop evidence — issue
// #173.
func loopSignalLabel(v loop.Verdict) string {
	if v.Signal.IsConsumption() {
		return "consumption"
	}
	return "loop"
}

// loopSignalLine renders a loop.Verdict line for the transcript. The numeric
// confidence is deliberately never rendered — it is a per-signal constant (or
// a rough heuristic), not a measured probability (issue #173).
func loopSignalLine(v loop.Verdict, msg string) string {
	cat := "loop detected"
	if v.Signal.IsConsumption() {
		cat = "consumption"
	}
	return fmt.Sprintf("[⚠ %s: %s]\n", cat, msg)
}

// turnTiming is the throughput timing accumulated across the measured
// completion requests of one turn (or observed cumulatively for a role):
// summed time-to-first-token, summed generation windows (decode seconds),
// and the count of requests that reported timing.
type turnTiming struct {
	ttftSec   float64
	decodeSec float64
	reqs      int64
}

// counterDelta is the change of a session token counter since the last turn.
// The counters restart from zero when the session is swapped or cleared
// (obs.ResetSessionTokens), leaving the remembered base above the new value;
// that is a reset, so the new value itself is this turn's delta.
func counterDelta(now, base int64) int64 {
	if now < base {
		return now
	}
	return now - base
}

// subtractTiming diffs a newly observed cumulative timing total against the
// previously seen one, clamping at zero — obs.ResetSessionTokens zeroes the
// accumulators on a session swap, and a stale delta base must not produce
// negative per-turn values.
func subtractTiming(ttftSec, decodeSec float64, reqs int64, seen turnTiming) turnTiming {
	return turnTiming{
		ttftSec:   max(ttftSec-seen.ttftSec, 0),
		decodeSec: max(decodeSec-seen.decodeSec, 0),
		reqs:      max(reqs-seen.reqs, 0),
	}
}

// markOutput stamps the first streamed output token of the current turn
// (content or thinking) so statusTokens can show a live decode-rate estimate.
// Subsequent calls within the same turn are no-ops.
// visibleChars is the length of streamed text without ANSI styling codes,
// which the output-token estimate would otherwise count as model output.
func visibleChars(s string) int64 {
	if !strings.Contains(s, "\x1b") {
		return int64(len(s))
	}
	return int64(len(ansi.Strip(s)))
}

func (m *model) markOutput() {
	if m.turnFirstOutputAt.IsZero() {
		m.turnFirstOutputAt = time.Now()
	}
}
