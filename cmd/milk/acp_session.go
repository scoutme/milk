package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/acp"
)

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

	mu     sync.Mutex
	cancel context.CancelFunc // set only while a turn is running
}

// newACPSession builds one session's runners and wires tool-call/thinking
// events + local-provider permission handling onto conn — the ACP-flavored
// analogue of buildTUIAgents (repl.go), swapping every tea.Msg send for a
// conn.Notify/Mapper call.
func newACPSession(cfg config.Config, sess *session.Session, conn acp.Conn, id acp.SessionID) (*acpSession, error) {
	cwd := sess.CWD
	ctx := context.Background()

	primaryRunner, localAgent, err := buildPrimaryRunner(ctx, cfg, cwd, sess)
	if err != nil {
		return nil, fmt.Errorf("building primary agent: %w", err)
	}
	escalationRunner, err := buildEscalationRunner(ctx, cfg, cwd, sess)
	if err != nil {
		return nil, fmt.Errorf("building escalation agent: %w", err)
	}

	memDir, err := memoryDir()
	if err != nil {
		return nil, fmt.Errorf("resolving memory dir: %w", err)
	}
	mem, err := memory.NewStore(memDir, sess.ID)
	if err != nil {
		mem = nil // best-effort, same as main.go's one-shot path
	}

	as := &acpSession{cfg: cfg, sess: sess, mem: mem, conn: conn, id: id}
	host := newACPHost(conn, id)

	if localAgent != nil {
		permStore, _ := local.OpenPermStore(cwd) //nolint:errcheck // nil disables persistent grants, same as every other best-effort call site
		wired := localAgent.
			WithPermissions(permStore, makeLocalPermAsk(host, permStore)).
			WithOnToolUse(as.onLocalToolUse).
			WithOnToolResult(as.onLocalToolResult).
			WithOnThinking(as.onThinking)
		primaryRunner = newLocalRunner(wired, primaryRunner.Name())
	}

	switch er := escalationRunner.(type) {
	case *cliRunner:
		// No WithPermissionHandler call, and a non-nil empty toolFutures so
		// cliRunner.Execute's guard (pc.cs != nil && pc.toolFutures == nil)
		// never fires makePermissionHandler's direct os.Stdin read — which
		// would race the JSON-RPC reader goroutine for stdin. claude-cli-as-
		// escalation defaults to denyAllHandler; see the design doc's ACP
		// status note on why that's not wired further this round.
		er.pc.toolFutures = map[string]chan string{}
		er.agent = er.agent.
			WithOnToolUse(as.onClaudeToolUse).
			WithOnToolUseReady(as.onClaudeToolUseReady).
			WithOnToolResult(as.onClaudeToolResult).
			WithOnThinking(as.onThinking)
	case *localRunner:
		permStore, _ := local.OpenPermStore(cwd) //nolint:errcheck
		wired := er.agent.
			WithPermissions(permStore, makeLocalPermAsk(host, permStore)).
			WithOnToolUse(as.onLocalToolUse).
			WithOnToolResult(as.onLocalToolResult).
			WithOnThinking(as.onThinking)
		escalationRunner = newLocalRunner(wired, er.name)
	}

	as.primaryRunner = primaryRunner
	as.escalationRunner = escalationRunner
	return as, nil
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
		Kind:          acp.ToolKindFor(name),
		Status:        status,
		RawInput:      rawInput,
		RawOutput:     rawOutput,
	})
}

func (as *acpSession) onLocalToolUse(id, name, _ string, rawInput map[string]any) {
	as.emitToolCall(id, name, acp.ToolCallInProgress, rawInput, nil)
}

func (as *acpSession) onLocalToolResult(id, name, result string, isError bool) {
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
	msgID := acp.MessageID(fmt.Sprintf("thought-%d", as.msgCounter.Load()))
	as.notify(acp.AgentThoughtChunk(msgID, text))
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
	prompt := strings.TrimSpace(text.String())

	msgID := acp.MessageID(fmt.Sprintf("msg-%d", as.msgCounter.Add(1)))

	turnCtx, cancel := context.WithCancel(ctx)
	as.mu.Lock()
	as.cancel = cancel
	as.mu.Unlock()
	defer func() {
		as.mu.Lock()
		as.cancel = nil
		as.mu.Unlock()
		cancel()
	}()

	as.notify(acp.RunningState())

	onResponse := func(responseText string) {
		as.notify(acp.AgentMessageChunk(msgID, responseText))
	}

	rtr := router.New(as.cfg, nil)
	decision, err := rtr.Route(turnCtx, as.sess, prompt, false, false)
	if err != nil {
		as.notify(acp.IdleState(acp.StopReasonError))
		return acp.PromptResponse{}, err
	}
	target := resolveTarget(decision.Target, as.primaryRunner != nil, as.escalationRunner != nil)

	var turnErr error
	switch target {
	case router.TargetLocal:
		turnErr = runPrimary(turnCtx, as.cfg, as.sess, as.primaryRunner, as.escalationRunner, as.mem, prompt, io.Discard, nil, onResponse, nil, nil)
	case router.TargetEscalation:
		turnErr = runEscalation(turnCtx, as.cfg, as.sess, as.escalationRunner, "", as.mem, prompt, io.Discard, nil, onResponse, nil, nil)
	default:
		turnErr = fmt.Errorf("unknown routing target: %s", target)
	}

	stopReason := acp.StopReasonEndTurn
	switch {
	case turnErr != nil && turnCtx.Err() != nil:
		stopReason = acp.StopReasonCancelled
	case turnErr != nil:
		stopReason = acp.StopReasonError
	}
	as.notify(acp.IdleState(stopReason))

	if turnErr != nil && stopReason != acp.StopReasonCancelled {
		return acp.PromptResponse{MessageID: msgID}, turnErr
	}
	return acp.PromptResponse{MessageID: msgID}, nil
}
