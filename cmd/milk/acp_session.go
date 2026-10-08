package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/events"
	"github.com/scoutme/milk/internal/loop"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/tasks"
	"github.com/scoutme/milk/internal/transport/acp"
)

// acpNoReplyNotice is sent when a turn completes without any message text.
const acpNoReplyNotice = "(milk finished this turn without a text reply — see the tool calls above, if any.)"

// acpSession holds one ACP session's state: a fresh buildPrimaryRunner/
// buildEscalationRunner pair, a fresh *session.Session/*memory.Store, and
// the agent/tool-call event wiring onto conn — never shared with any other
// session (see docs/machine-readable-output-design.md's ACP status note on
// why local.Manager+dispatchAgents are NOT shared across sessions here,
// unlike the design doc's own literal wording).
type acpSession struct {
	cfg  config.Config
	sess *session.Session
	mem  *memory.Store
	conn acp.Conn
	id   acp.SessionID

	primaryRunner    TurnRunner
	escalationRunner TurnRunner

	msgCounter atomic.Int64

	// st carries the slash-command/routing state (sticky and forced routing,
	// cwd, cfg copy) shared with the TUI's handlers; showThinking gates
	// agent_thought_chunk forwarding (/think).
	st           *interactiveState
	showThinking atomic.Bool

	// skipPerms is /skip-permissions (seeded from dangerously_skip_permissions,
	// like the TUI); openCalls maps a tool name to its in-flight tool-call
	// IDs so a permission prompt can point at the call it is about.
	skipPerms atomic.Bool
	callsMu   sync.Mutex
	openCalls map[string][]acp.ToolCallID
	host      *acpHost

	loop       *loop.Detector // TUI-level loop/consumption signals; see acp_signals.go
	loopMu     sync.Mutex     // guards loop, lastTarget and reads of st routing pins
	lastTarget router.Target  // target of the previous model turn

	// turnMu serializes turns: client prompts and automatic background
	// follow-ups (see acp_followup.go).
	turnMu sync.Mutex
	// pendingWaveFollowup / pendingUserFollowup remember a follow-up that was
	// requested while a turn was running, to be retried when it ends.
	pendingWaveFollowup bool
	pendingUserFollowup bool

	taskStore       *tasks.Store
	workflowRunning atomic.Bool
	cliPC           permContext    // for CLI-agent roles inside workflows
	mgr             *local.Manager // background jobs; nil without an inference-server agent
	da              *dispatchAgents

	// localAgent / escLocalAgent are the wired local-provider agents behind
	// the runners (nil when that role isn't local), kept for /bg start.
	localAgent    *local.Agent
	escLocalAgent *local.Agent

	// pendingInit is the /config init (/init) setup wizard: while set, the
	// next client prompt is consumed as the wizard's next answer instead of
	// being routed to a model (see runTurn and initwizard_core.go). Only
	// touched under turnMu, like the rest of prompt handling.
	pendingInit *initWizardState

	// formElicit: the client advertised form-mode elicitation
	// (clientCapabilities.elicitation.form at initialize), so the setup
	// wizard can ask its steps as elicitation form dialogs (titled choices,
	// pre-populated defaults) instead of typed answers — see
	// acp_initwizard.go. Set once at session creation; read-only after.
	formElicit bool

	// noChoice: this client's session/request_permission responses carry no
	// recognizable outcome (or fail), so the setup wizard skips its clickable
	// choice prompts and asks steps as typed input directly. Set on the
	// first such response; read-only after.
	noChoice bool

	// updateAnnounced: the "update available" notice has been sent to this
	// session (once per session; see acpServer.announceUpdate). Guarded by
	// srv.mu, not by any session lock.
	updateAnnounced bool

	// srv is the owning server: the shared update state (/update …) and the
	// startup check live one level above sessions. Set once at session
	// creation under srv.mu; read-only after.
	srv *acpServer

	// v1Client: the client speaks ACP v1, which lacks v2's plan_update and
	// tool_call_content_chunk (see notifyPlan, streamLive).
	v1Client  bool
	planMu    sync.Mutex
	plans     map[acp.PlanID][]acp.PlanEntry
	planOrder []acp.PlanID

	mu      sync.Mutex
	cancel  context.CancelFunc // set only while a turn is running
	turnCtx context.Context    // the running turn's context (cancelled with cancel)
}

// newACPSession builds one session's runners and wires tool-call/thinking
// events + local-provider permission handling onto conn — the ACP-flavored
// analogue of buildTUIAgents (repl.go), swapping every tea.Msg send for a
// conn.Notify/Mapper call.
func newACPSession(cfg config.Config, sess *session.Session, conn acp.Conn, id acp.SessionID) (*acpSession, error) {
	cwd := sess.CWD

	memDir, err := memoryDir()
	if err != nil {
		return nil, fmt.Errorf("resolving memory dir: %w", err)
	}
	mem, err := memory.NewStore(memDir, sess.ID)
	if err != nil {
		mem = nil // best-effort, same as main.go's one-shot path
	}

	as := &acpSession{cfg: cfg, sess: sess, mem: mem, conn: conn, id: id}
	as.st = &interactiveState{sess: sess, cwd: cwd, cfg: cfg, mem: mem, toolFutures: map[string]chan string{}}
	// Thoughts have always been forwarded over ACP; keep that unless the
	// config explicitly opts out.
	as.showThinking.Store(cfg.ShowReasoning == nil || *cfg.ShowReasoning)
	host := newACPHost(conn, id)
	as.host = host
	host.callID = as.pendingCallID
	host.failed = as.permissionFailed
	as.skipPerms.Store(cliAgentConfig(cfg).DangerouslySkipPermissions)

	as.loop = loop.New(cfg.LoopDetectionCfg())
	as.cliPC = permContext{cwd: cwd, toolFutures: map[string]chan string{}}
	as.setupTasks()
	if err := as.buildRunners(cfg); err != nil {
		return nil, err
	}
	return as, nil
}

// buildRunners (re)builds the session's primary/escalation runners from cfg
// and (re)wires everything that hangs off them — the ACP analogue of the TUI's
// buildTUIAgents/commitSwitchAgent live-rebuild path. newACPSession calls it
// once at session creation; acpConfig's /config init commit calls it again so
// an ACP-only setup (an editor driving milk with no TUI in the loop) switches
// to the configured agent without restarting the session. cfg/st.cfg are only
// swapped after the new runners build, so a failure leaves the session on its
// previous working config.
func (as *acpSession) buildRunners(cfg config.Config) error {
	cwd := as.st.cwd
	ctx := context.Background()

	primaryRunner, localAgent, err := buildPrimaryRunner(ctx, cfg, cwd, as.sess)
	if err != nil {
		return fmt.Errorf("building primary agent: %w", err)
	}
	escalationRunner, err := buildEscalationRunner(ctx, cfg, cwd, as.sess)
	if err != nil {
		return fmt.Errorf("building escalation agent: %w", err)
	}

	// Publish the new config before the steps below (setupBackground reads it).
	as.cfg = cfg
	as.st.cfg = cfg

	// The background-job manager is created once per session (ADR-0043);
	// a rebuild only needs to create it if this is the first config with a
	// local agent that could run jobs at all.
	if as.mgr == nil {
		as.setupBackground(localAgent != nil || isLocalRunner(escalationRunner))
	}

	// wireLocal applies everything an ACP session adds to a local-provider agent.
	wireLocal := func(a *local.Agent) *local.Agent {
		permStore, _ := local.OpenPermStore(cwd) //nolint:errcheck // nil disables persistent grants, same as every other best-effort call site
		wired := a.
			WithPermissions(permStore, makeLocalPermAsk(as.host, permStore)).
			WithSkipPermissionsFunc(as.skipPerms.Load).
			WithBackgroundPermissionAsk(as.backgroundPermissionAsk).
			WithOnToolUse(as.onLocalToolUse).
			WithOnToolResult(as.onLocalToolResult).
			WithOnThinking(as.onThinking)
		if as.taskStore != nil {
			wired = wired.WithTaskStore(tasks.NewAdapter(as.taskStore))
		}
		if as.mgr != nil {
			wired.SetBackgroundManager(as.mgr)
		}
		return wired
	}

	if localAgent != nil {
		as.localAgent = wireLocal(localAgent)
		primaryRunner = newLocalRunner(as.localAgent, primaryRunner.Name())
	} else {
		as.localAgent = nil
	}

	as.escLocalAgent = nil
	switch er := escalationRunner.(type) {
	case *cliRunner:
		// No WithPermissionHandler call, and a non-nil empty toolFutures so
		// cliRunner.Execute's guard (pc.cs != nil && pc.toolFutures == nil)
		// never fires makePermissionHandler's direct os.Stdin read — which
		// would race the JSON-RPC reader goroutine for stdin. claude-cli-as-
		// escalation defaults to denyAllHandler; see the design doc's ACP
		// status note on why that's not wired further this round.
		er.pc.toolFutures = map[string]chan string{}
		as.cliPC = er.pc
		er.agent = er.agent.
			WithOnToolUse(as.onClaudeToolUse).
			WithOnToolUseReady(as.onClaudeToolUseReady).
			WithOnToolResult(as.onClaudeToolResult).
			WithOnThinking(as.onThinking)
	case *localRunner:
		as.escLocalAgent = wireLocal(er.agent)
		escalationRunner = newLocalRunner(as.escLocalAgent, er.name)
	}

	as.primaryRunner = primaryRunner
	as.escalationRunner = escalationRunner
	if as.da == nil {
		as.da = &dispatchAgents{}
	}
	// Update the dispatch set in place so lazily-built per-session maps on it
	// (toolRunners, MCP toolsets) survive a config reload.
	as.da.primary = primaryRunner
	as.da.escalation = escalationRunner
	as.da.local = as.localAgent
	as.da.escalationLocal = as.escLocalAgent
	as.da.localAvail = primaryRunner != nil
	as.da.escalationAvail = escalationRunner != nil
	as.da.backgroundMgr = as.mgr
	return nil
}

// notify sends a session/update notification for this session.
func (as *acpSession) notify(u acp.SessionUpdate) {
	n := (&acp.Mapper{Session: as.id}).Update(u)
	as.conn.Notify(n.Method, n.Params) //nolint:errcheck // stdout write; nothing meaningful to do with the error
}

// emitToolCall sends a tool_call_update, shared by both providers' callback
// wiring below. status/rawInput/rawOutput are zero-valued (omitted on the
// wire) for whichever half of the use/result pair hasn't happened yet.
func (as *acpSession) emitToolCall(id, name string, status acp.ToolCallStatus, rawInput, rawOutput any) {
	as.notify(acp.ToolCallUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    acp.ToolCallID(id),
		Name:          name,
		Title:         name,
		Kind:          acp.ToolKindFor(name),
		Status:        status,
		RawInput:      rawInput,
		RawOutput:     rawOutput,
	})
}

func (as *acpSession) onLocalToolUse(id, name, _ string, rawInput map[string]any) {
	as.callsMu.Lock()
	if as.openCalls == nil {
		as.openCalls = map[string][]acp.ToolCallID{}
	}
	as.openCalls[name] = append(as.openCalls[name], acp.ToolCallID(id))
	as.callsMu.Unlock()
	as.emitToolCall(id, name, acp.ToolCallInProgress, rawInput, nil)
}

func (as *acpSession) onLocalToolResult(id, name, result string, isError bool) {
	as.callsMu.Lock()
	if as.openCalls == nil {
		as.openCalls = map[string][]acp.ToolCallID{}
	}
	calls := as.openCalls[name][:0:0]
	for _, c := range as.openCalls[name] {
		if string(c) != id {
			calls = append(calls, c)
		}
	}
	as.openCalls[name] = calls
	as.callsMu.Unlock()
	status := acp.ToolCallCompleted
	if isError {
		status = acp.ToolCallFailed
	}
	as.emitToolCall(id, name, status, nil, result)
}

func (as *acpSession) onClaudeToolUse(id, name string) {
	as.emitToolCall(id, name, acp.ToolCallInProgress, nil, nil)
}

func (as *acpSession) onClaudeToolUseReady(id, name string, input map[string]any) {
	as.emitToolCall(id, name, acp.ToolCallInProgress, input, nil)
}

func (as *acpSession) onClaudeToolResult(id, name, result string, isError bool) {
	status := acp.ToolCallCompleted
	if isError {
		status = acp.ToolCallFailed
	}
	as.emitToolCall(id, name, status, nil, result)
}

func (as *acpSession) onThinking(text string) {
	as.feedThinking(text) // detection runs whether or not the client is shown the reasoning
	if !as.showThinking.Load() {
		return
	}
	msgID := acp.MessageID(fmt.Sprintf("thought-%d", as.msgCounter.Load()))
	as.notify(acp.AgentThoughtChunk(msgID, text))
}

// currentCtx returns the running turn's context (session/cancel aware) for
// work a slash-command handler drives inside that turn — e.g. the setup
// wizard's form dialogs. Returns a plain background context when no turn is
// running.
func (as *acpSession) currentCtx() context.Context {
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.turnCtx != nil {
		return as.turnCtx
	}
	return context.Background()
}

// prompt runs one turn for a session/prompt request. Blocks for the whole
// turn — per the upstream schema, session/update notifications stream
// throughout, a terminal state_update notification fires, and only then
// does this return (see acp.PromptResponse's doc comment in lifecycle.go).
func (as *acpSession) prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
	var text strings.Builder
	for _, block := range req.Prompt {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	// One turn at a time per session. A prompt that arrives while an
	// automatic background follow-up is running waits for it rather than
	// cancelling it: the follow-up has already drained the finished jobs'
	// results, so cancelling would lose them.
	as.turnMu.Lock()
	defer func() {
		as.turnMu.Unlock()
		as.flushPendingFollowup()
	}()
	return as.runTurn(ctx, strings.TrimSpace(text.String()))
}

// runTurn runs one turn for prompt text, with as.turnMu held by the caller.
func (as *acpSession) runTurn(ctx context.Context, prompt string) (acp.PromptResponse, error) {

	as.notify(acp.RunningState())

	msgID := acp.MessageID(fmt.Sprintf("msg-%d", as.msgCounter.Add(1)))

	turnCtx, cancel := context.WithCancel(ctx)
	as.mu.Lock()
	as.cancel = cancel
	as.turnCtx = turnCtx
	as.mu.Unlock()
	defer func() {
		as.mu.Lock()
		as.cancel = nil
		as.turnCtx = nil
		as.mu.Unlock()
		cancel()
	}()

	// replied tracks whether the client got any message text this turn, so a
	// turn that ends on tool calls alone (or an empty completion) doesn't look
	// to the client like milk never answered.
	var replied atomic.Bool
	say := func(text string) {
		replied.Store(true)
		as.notify(acp.AgentMessageChunk(msgID, text))
	}
	onResponse := say
	turn := &acpTurn{ctx: turnCtx, as: as, say: say}

	// A pending /config init wizard consumes this prompt as its next answer
	// (ACP's handleInitWizardKey analogue — one answer per session/prompt).
	// Escape hatches, since ACP has no esc key: any recognized slash command
	// cancels the wizard and runs instead, and a plain cancel word aborts it.
	// Background follow-ups are synthetic and never wizard input.
	wizardCancelNote := ""
	if as.pendingInit != nil && prompt != backgroundFollowupPrompt {
		if _, _, found := extractSlashCommand(prompt); found {
			as.pendingInit = nil
			if !initWizardRestartPrompt(prompt) {
				wizardCancelNote = stripANSI(milkTag()) + " setup wizard cancelled — restart with /config init\n\n"
			}
		} else if initWizardCancelWord(prompt) {
			as.pendingInit = nil
			say(stripANSI(milkTag() + " setup wizard cancelled\n"))
			as.notify(acp.IdleState(acp.StopReasonEndTurn))
			return acp.PromptResponse{MessageID: msgID}, nil
		} else {
			// initWizardApply maps the 'default' word onto the blank answer
			// itself (the empty turn ACP chat can't send), on every input
			// surface.
			res := initWizardApply(as.pendingInit, prompt, as.initWizardOptions())
			out := res.Output + as.commitInitWizard(res)
			if res.Done {
				as.pendingInit = nil
			} else {
				// Keep going on the best surface the client has (form
				// dialogs / clickable choice prompts), or hand the next
				// question back to typed input — its text comes back with
				// it, so res.Prompt is never echoed twice.
				more, done := as.runInitDialogs(turnCtx, as.pendingInit)
				out += more
				if done {
					as.pendingInit = nil
				}
			}
			if out != "" {
				say(stripANSI(out))
			}
			as.notify(acp.IdleState(acp.StopReasonEndTurn))
			return acp.PromptResponse{MessageID: msgID}, nil
		}
	}

	if handled, output, dispatch := as.runSlashCommand(turn, prompt); handled {
		if wizardCancelNote != "" {
			output = wizardCancelNote + output
		}
		if output != "" {
			say(output)
		}
		if dispatch == "" {
			as.notify(acp.IdleState(acp.StopReasonEndTurn))
			return acp.PromptResponse{MessageID: msgID}, nil
		}
		prompt = dispatch
	}

	as.loopMu.Lock()
	as.loop.ResetTurn()
	as.loopMu.Unlock()
	promptBefore, completionBefore := as.sumTokens()

	rt, err := routeTurn(turnCtx, as.st, router.New(as.cfg, nil), prompt, as.primaryRunner != nil, as.escalationRunner != nil)
	if err != nil {
		as.notify(acp.IdleState(acp.StopReasonError))
		return acp.PromptResponse{}, err
	}
	decision, target := rt.Decision, rt.Target
	source, turnStart := turnSource(as.st), time.Now()
	var routeMeta map[string]any
	if runner := map[router.Target]TurnRunner{router.TargetLocal: as.primaryRunner, router.TargetEscalation: as.escalationRunner}[target]; runner != nil {
		routeMeta = as.announceRoute(decision, target, runner.Name())
	}

	onWorkflowStart := func(ws *local.WorkflowStartSignal) { as.startWorkflowFromTool(turn, ws) }

	var turnErr error
	switch target {
	case router.TargetLocal:
		turnErr = runPrimary(turnCtx, as.cfg, as.sess, as.primaryRunner, as.escalationRunner, as.mem, prompt, io.Discard, as.da, onResponse, nil, onWorkflowStart)
	case router.TargetEscalation:
		turnErr = runEscalation(turnCtx, as.cfg, as.sess, as.escalationRunner, "", as.mem, prompt, io.Discard, as.da, onResponse, nil, onWorkflowStart)
	default:
		turnErr = fmt.Errorf("unknown routing target: %s", target)
	}

	recordTurn(turnCtx, target, source, turnStart, turnErr)
	if turnErr == nil {
		noteTurnSucceeded(as.st, target)
	}

	stopReason := acp.StopReasonEndTurn
	switch {
	case turnErr != nil && turnCtx.Err() != nil:
		stopReason = acp.StopReasonCancelled
	case turnErr != nil:
		stopReason = acp.StopReasonError
	}
	if stopReason == acp.StopReasonEndTurn && !replied.Load() {
		say(acpNoReplyNotice)
	}
	if stopReason == acp.StopReasonEndTurn {
		as.endTurnSignals(promptBefore, completionBefore)
	}
	idle := acp.IdleState(stopReason)
	idle.Meta = routeMeta
	as.notify(idle)

	if turnErr != nil && stopReason != acp.StopReasonCancelled {
		return acp.PromptResponse{MessageID: msgID}, turnErr
	}
	return acp.PromptResponse{MessageID: msgID}, nil
}

func isLocalRunner(r TurnRunner) bool {
	_, ok := r.(*localRunner)
	return ok
}

// pendingCallID is the most recent in-flight tool call named tool: the local
// agent reports a call (onToolUse) before it checks permission for it.
func (as *acpSession) pendingCallID(tool string) acp.ToolCallID {
	as.callsMu.Lock()
	defer as.callsMu.Unlock()
	if calls := as.openCalls[tool]; len(calls) > 0 {
		return calls[len(calls)-1]
	}
	return ""
}

// permissionFailed tells the client why a tool is about to be denied when the
// permission request itself could not be answered. Without this the agent
// only reports "denied by user", which hides that nobody was ever asked.
func (as *acpSession) permissionFailed(tool string, err error) {
	as.notify(acp.AgentMessageChunk(acp.MessageID(fmt.Sprintf("perm-%d", as.msgCounter.Add(1))),
		fmt.Sprintf("Could not ask for permission to run %s (%v), so it was denied. "+
			"The client must answer session/request_permission; /skip-permissions on approves tools without asking.", tool, err)))
}

// backgroundPermissionAsk asks the client whether a background job may use a
// tool, attributed to the job's own tool-call row. The wait is bounded by the
// job timeout, so an unanswered question denies the tool instead of holding
// the job's concurrency slot forever.
func (as *acpSession) backgroundPermissionAsk(jobID, tool, summary string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), as.cfg.EffectiveBackgroundAgentTimeout())
	defer cancel()
	out, err := as.host.RequestPermission(ctx, events.PermissionRequest{
		Tool:       tool,
		Summary:    strings.TrimSpace(summary + " — requested by background agent " + jobID),
		ToolCallID: string(acp.JobToolCallID(jobID)),
	})
	return err == nil && out.Allow
}
