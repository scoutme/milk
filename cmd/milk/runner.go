package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/agent/subprocess"
	"github.com/scoutme/milk/internal/agentprompt"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/mcp"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
)

// permissionManagementInstruction is appended to the claude-cli static context
// when ExperimentalPermissionManagement is enabled. It instructs Claude to
// terminate gracefully on "Stream closed" pre-flight denials so milk can grant
// the permission and respawn the turn, rather than attempting workarounds.
const permissionManagementInstruction = `

## Permission management (milk-managed)
If a tool call returns an error with content that includes "Stream closed", it
means milk has not yet granted the required permission for that tool or directory.
Do NOT retry the tool, attempt workarounds, or continue the task.
Instead, output exactly one short message such as:
  "Pausing — milk needs to grant permission for [tool name] before I can continue."
then end your turn immediately with no further output or tool calls.
Milk will automatically prompt the user for the permission and resume your task.`

// AgentRole distinguishes primary from escalation so runners can apply
// role-specific behaviour (e.g. history scoping, context builder choice).
type AgentRole int

const (
	RolePrimary    AgentRole = iota
	RoleEscalation AgentRole = iota
	// RoleWorkflow is used for agents acting as workflow step executors (designer,
	// generator, evaluator). They receive a clean system prompt — no escalation
	// framing, no session orientation, no repeated-prompt guard.
	RoleWorkflow AgentRole = iota
)

// workflowJournalPollInterval is how often we stat the workflow journal file
// while waiting for a background workflow to complete. Pure local I/O — no API cost.
const workflowJournalPollInterval = 5 * time.Second

// workflowResumeTimeout is the deadline applied to the single --resume API call
// issued after the workflow journal signals completion. Independent of the turn
// deadline, which may have already expired while waiting for the workflow.
const workflowResumeTimeout = 10 * time.Minute

// TurnResult is the normalised output of a single agent inference turn.
type TurnResult struct {
	Text             string
	NewSessionID     string // non-empty when runner opened a new session; caller must persist
	EscalationReason string // non-empty when subprocess primary requested hand-off
	EndsWithQ        bool
	InputTokens      int64
	OutputTokens     int64
	CacheRead        int64
	CacheCreate      int64
	CostUSD          float64
}

// TurnCallbacks carries the tag-intercept callbacks wired per-turn by the dispatcher.
type TurnCallbacks struct {
	OnNeed     func(body string)
	OnPercept  func(body, consumerHint string)
	OnEscalate func(reason string) // nil for escalation runners (they never self-escalate)
	OnResponse func(text string)   // called with the agent's final text after a successful turn
	// ImageContextFile is an optional path to a temp file containing image data-URI
	// blocks. CLI runners append it via --append-system-prompt-file to avoid
	// inlining large base64 strings in the prompt argument (ARG_MAX).
	ImageContextFile string
}

// TurnRunner abstracts provider-specific inference for one agent turn.
// It is responsible only for invoking the underlying agent and returning a
// TurnResult. All session bookkeeping (state transitions, turn recording,
// token accounting) is done by the dispatch layer (runPrimary / runEscalation).
type TurnRunner interface {
	// Name returns the display name used in logs and status lines.
	Name() string
	// IsCLI reports whether this runner invokes the claude CLI subprocess.
	// Used to decide how to pass image attachments (@path vs inline data URI).
	IsCLI() bool
	// Ping checks whether the underlying agent binary or endpoint is reachable.
	Ping() error
	// Execute runs one inference turn. sessionID is the existing session to
	// resume (empty → first turn). ctxMode, nonce, percepts, and injectInstructions
	// are pre-computed by the dispatcher. cbs carries tag callbacks wired for this turn.
	Execute(
		ctx context.Context,
		cfg config.Config,
		sess *session.Session,
		mem *memory.Store,
		role AgentRole,
		ctxMode escalation.ContextMode,
		sessionID, nonce string,
		percepts []string,
		injectInstructions bool,
		prompt string,
		cbs TurnCallbacks,
		out io.Writer,
	) (TurnResult, error)
	// RunToolCall executes a single lightweight inference call with no session
	// bookkeeping. Returns the agent's text response or an error.
	RunToolCall(ctx context.Context, cfg config.Config, prompt string, out io.Writer) (string, error)
}

// ── localRunner ──────────────────────────────────────────────────────────────

type localRunner struct {
	agent *local.Agent
	name  string
}

func newLocalRunner(agent *local.Agent, name string) *localRunner {
	return &localRunner{agent: agent, name: name}
}

func (r *localRunner) Name() string { return r.name }
func (r *localRunner) IsCLI() bool  { return false }
func (r *localRunner) Ping() error  { return r.agent.Ping(context.Background()) }

func (r *localRunner) Execute(
	ctx context.Context,
	cfg config.Config,
	sess *session.Session,
	mem *memory.Store,
	role AgentRole,
	ctxMode escalation.ContextMode,
	sessionID, nonce string,
	percepts []string,
	injectInstructions bool,
	prompt string,
	cbs TurnCallbacks,
	out io.Writer,
) (TurnResult, error) {
	ac := agentConfigForRole(cfg, role)

	agent := r.agent.WithMemConfig(local.MemConfig{
		ResultMaxBytes:       cfg.AgentMemoryResultMaxByteCount(ac),
		ReinjectionTurns:     cfg.AgentMemoryReinjectionTurnThreshold(ac, role == RolePrimary),
		ReinjectionBytes:     cfg.AgentMemoryReinjectionByteThreshold(ac, role == RolePrimary),
		RelevanceGateEnabled: cfg.AgentPerceptRelevanceGateEnabled(ac),
		MaxToolIterations:    cfg.AgentMaxToolIterations(ac),
	}).WithToolTimeout(cfg.AgentToolTimeout(ac))

	// Apply custom prompt (prompt / prompt_file in agent config) when set.
	if ac.Prompt != "" || ac.PromptFile != "" {
		vars := buildPromptVars(sess, percepts, cfg)
		if rendered, err := agentprompt.Render(ac, vars); err != nil {
			fmt.Fprintf(os.Stderr, "%s warning: custom prompt render failed for agent %q: %v\n", milkTag(), ac.Name, err)
		} else if rendered != "" {
			agent = agent.WithCustomPrompt(rendered)
		}
	}

	if role == RoleWorkflow {
		agent = agent.AsWorkflowExecutor()
	}

	primaryName := cfg.ActiveAgent().Name
	escalationName := cfg.EscalationAgentConfig().Name

	agent = agent.WithTagCallbacks(nonce, primaryName, escalationName,
		cbs.OnNeed,
		func(body, hint string) {
			if cbs.OnPercept != nil {
				cbs.OnPercept(body, hint)
			}
		},
	)

	// Build history depending on role and context mode.
	var history []local.Message
	switch role {
	case RoleWorkflow:
		// Workflow executors get a fresh empty history — their context comes
		// entirely from the workflow prompt, not from the REPL session.
		history = nil

	case RoleEscalation:
		// Inject orientation as a system message, build appropriately-scoped history.
		orientationText := escalation.BuildDynamicContext(sess, ctxMode)
		perceptsText := escalation.FormatPercepts(percepts)
		skipOther := cfg.ExperimentalLazyHistoryManagement

		isFirst := !session.EscalationEverActive(sess) || ctxMode == escalation.ContextModeFirst
		if isFirst && session.EscalationEverActive(sess) {
			history = escalationLocalHistoryFresh(sess, ac.Name, skipOther)
		} else {
			history = escalationLocalHistory(sess, ac.Name, skipOther)
		}
		if perceptsText != "" {
			history = append([]local.Message{{Role: "system", Content: perceptsText}}, history...)
		}
		if orientationText != "" {
			history = append([]local.Message{{Role: "system", Content: orientationText}}, history...)
		}

		msgBudget := cfg.AgentMessageBudget(ac)
		if msgBudget > 0 {
			overhead := agent.SystemOverheadChars(sess) + len(orientationText)
			msgBudget = max(1, msgBudget-overhead)
		}
		if trimmed, ok := trimLocalMessages(history, msgBudget); ok {
			history = trimmed
		}

	default: // RolePrimary
		history = sessionToUnifiedMessages(sess, ac.Name, cfg.ExperimentalLazyHistoryManagement)

		// Prepend orientation: percepts and current need as system messages,
		// mirroring the context the escalation agent always receives.
		if perceptsText := escalation.FormatPercepts(percepts); perceptsText != "" {
			history = append([]local.Message{{Role: "system", Content: perceptsText}}, history...)
		}
		if sess.CurrentNeed != "" {
			label := "[Current user goal]\n"
			if sess.CurrentNeedSetAt > 0 {
				turnsAgo := (len(sess.History) + 1) - (sess.CurrentNeedSetAt - 1)
				if turnsAgo >= 4 {
					label = "[Last known user goal — may already be fulfilled; verify from conversation history before acting]\n"
				}
			}
			history = append([]local.Message{{Role: "system", Content: label + sess.CurrentNeed}}, history...)
		}

		msgBudget := cfg.AgentMessageBudget(ac)
		if msgBudget > 0 {
			overhead := agent.SystemOverheadChars(sess)
			msgBudget = max(1, msgBudget-overhead)
		}
		if trimmed, ok := trimLocalMessages(history, msgBudget); ok {
			history = trimmed
		}
	}

	updatedHistory, err := agent.Run(ctx, history, prompt, out, sess, mem)
	if err != nil {
		if esc, ok := err.(*local.EscalationSignal); ok {
			return TurnResult{EscalationReason: esc.Reason}, nil
		}
		// On connection failure, auto-start the server if run_cmd is configured
		// and retry once. This handles the case where the server was stopped
		// (manually or between sessions) but milk is already running.
		if ac.RunCmd != "" && isConnectionRefused(err) {
			if startErr := ensureServerRunning(ctx, ac.URL, ac.RunCmd, ac.Name); startErr == nil {
				updatedHistory, err = agent.Run(ctx, history, prompt, out, sess, mem)
			}
		}
		if err != nil {
			if esc, ok := err.(*local.EscalationSignal); ok {
				return TurnResult{EscalationReason: esc.Reason}, nil
			}
			return TurnResult{}, err
		}
	}

	text := ""
	if len(updatedHistory) > 0 {
		last := updatedHistory[len(updatedHistory)-1]
		if last.Role == "assistant" {
			text = last.Content
		}
	}
	return TurnResult{Text: text}, nil
}

func (r *localRunner) RunToolCall(ctx context.Context, _ config.Config, prompt string, out io.Writer) (string, error) {
	msgs := []local.Message{{Role: "user", Content: prompt}}
	updatedMsgs, err := r.agent.Run(ctx, msgs, prompt, out, nil, nil)
	if err != nil {
		return "", err
	}
	for i := len(updatedMsgs) - 1; i >= 0; i-- {
		if updatedMsgs[i].Role == "assistant" {
			return updatedMsgs[i].Content, nil
		}
	}
	return "", nil
}

// switchWriter is an io.Writer whose target can be swapped atomically mid-stream.
// It is used to redirect Claude's output into a side buffer when AskUserQuestion
// is announced, so the question text can be suppressed if milk handles it itself.
type switchWriter struct {
	mu     sync.Mutex
	target io.Writer
}

func (w *switchWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	t := w.target
	w.mu.Unlock()
	return t.Write(p)
}

func (w *switchWriter) redirectTo(t io.Writer) {
	w.mu.Lock()
	w.target = t
	w.mu.Unlock()
}

// ── cliRunner ─────────────────────────────────────────────────────────────────

type cliRunner struct {
	agent      *claude.Agent
	name       string
	pc         permContext
	newInput   func() inputReader // produces an inputReader for permission prompts
	mcpServers []config.MCPServerConfig
}

func newCLIRunner(agent *claude.Agent, name string, pc permContext, newInput func() inputReader) *cliRunner {
	return &cliRunner{agent: agent, name: name, pc: pc, newInput: newInput}
}

// withMCPServers returns a copy of the cliRunner that passes the given MCP servers
// to the Claude CLI subprocess via --mcp-config.
func (r *cliRunner) withMCPServers(servers []config.MCPServerConfig) *cliRunner {
	c := *r
	c.mcpServers = servers
	return &c
}

func (r *cliRunner) Name() string { return r.name }
func (r *cliRunner) IsCLI() bool  { return true }
func (r *cliRunner) Ping() error  { return r.agent.Ping() }

func (r *cliRunner) Execute(
	ctx context.Context,
	cfg config.Config,
	sess *session.Session,
	mem *memory.Store,
	role AgentRole,
	ctxMode escalation.ContextMode,
	sessionID, nonce string,
	percepts []string,
	injectInstructions bool,
	prompt string,
	cbs TurnCallbacks,
	out io.Writer,
) (TurnResult, error) {
	primaryName := cfg.ActiveAgent().Name
	escalationName := cfg.EscalationAgentConfig().Name

	agent := applyPersistedGrants(r.agent, r.pc)
	if r.pc.cs != nil && r.pc.toolFutures == nil {
		agent = agent.WithPermissionHandler(makePermissionHandler(r.newInput(), out, r.pc.cs))
	}
	if mem != nil && cbs.OnPercept != nil {
		agent = agent.WithOnPercept(cbs.OnPercept, nonce, primaryName, escalationName)
	}
	if cbs.OnNeed != nil {
		agent = agent.WithOnNeed(cbs.OnNeed, nonce)
	}
	if len(r.mcpServers) > 0 {
		agent = agent.WithMCPServers(r.mcpServers)
	}

	staticCtx := escalation.BuildStaticContext(nonce, percepts, ctxMode, injectInstructions, primaryName, escalationName)
	if cfg.ExperimentalPermissionManagement {
		staticCtx += permissionManagementInstruction
	}

	// Prepend custom prompt from the escalation agent config when set.
	escAC := cfg.EscalationAgentConfig()
	if escAC.Prompt != "" || escAC.PromptFile != "" {
		vars := buildPromptVars(sess, percepts, cfg)
		if rendered, err := agentprompt.Render(escAC, vars); err != nil {
			fmt.Fprintf(os.Stderr, "%s warning: custom prompt render failed for agent %q: %v\n", milkTag(), escAC.Name, err)
		} else if rendered != "" {
			staticCtx = rendered + "\n\n" + staticCtx
		}
	}

	dynamicCtx := escalation.BuildDynamicContext(sess, ctxMode)

	// Append image context (data-URI blocks) to dynamicCtx so large base64 strings
	// reach Claude via --append-system-prompt-file (temp file) rather than the
	// prompt argument, avoiding ARG_MAX failures for large images.
	if cbs.ImageContextFile != "" {
		if data, err := os.ReadFile(cbs.ImageContextFile); err == nil && len(data) > 0 {
			if dynamicCtx != "" {
				dynamicCtx += "\n"
			}
			dynamicCtx += string(data)
		}
		os.Remove(cbs.ImageContextFile) //nolint:errcheck
		cbs.ImageContextFile = ""
	}

	// Suppress duplicate dynamic context on resume turns (cache preservation).
	// Re-sending an identical file still shifts the cache suffix and causes a miss.
	if r.pc.contextHash != nil {
		h := fmt.Sprintf("%x", sha256.Sum256([]byte(dynamicCtx)))[:16]
		if ctxMode == escalation.ContextModeResume && h == *r.pc.contextHash {
			dynamicCtx = ""
		} else {
			*r.pc.contextHash = h
		}
	}

	// Stream live to the TUI; redirect into askBuf only if AskUserQuestion is
	// announced mid-stream — that text must be suppressed so milk's own selection
	// prompt is the only UI shown to the user.
	var askBuf bytes.Buffer
	sw := &switchWriter{target: out}
	prevOnToolUse := agent.OnToolUseCallback()
	agent = agent.WithOnToolUse(func(name string) {
		if name == "AskUserQuestion" {
			sw.redirectTo(&askBuf)
		}
		if prevOnToolUse != nil {
			prevOnToolUse(name)
		}
	})

	var (
		res    claude.ParseResult
		runErr error
	)
	if ctxMode == escalation.ContextModeResume || (ctxMode == escalation.ContextModeReturning && sessionID != "") {
		res, runErr = agent.RunResume(ctx, sessionID, staticCtx, dynamicCtx, prompt, sw)
		if runErr != nil && claude.IsInvalidSession(runErr) {
			// Stale session ID — Claude's store no longer has this session (evicted,
			// CLI upgrade, machine restart, etc.). Restore live output, notify the
			// user inline, and fall back to a fresh RunFirst with full context.
			askBuf.Reset()
			sw.redirectTo(out)
			fmt.Fprintf(out, "\n\033[2m[Claude session refreshed — previous session no longer available]\033[0m\n\n")
			staticCtx = escalation.BuildStaticContext(nonce, percepts, escalation.ContextModeFirst, injectInstructions, primaryName, escalationName)
			dynamicCtx = escalation.BuildDynamicContext(sess, escalation.ContextModeFirst)
			var newID string
			newID, res, runErr = agent.RunFirst(ctx, staticCtx, dynamicCtx, prompt, sw)
			if runErr == nil {
				sessionID = newID
			}
		}
	} else {
		var newID string
		newID, res, runErr = agent.RunFirst(ctx, staticCtx, dynamicCtx, prompt, sw)
		if runErr == nil {
			sessionID = newID
		}
	}
	if runErr != nil {
		return TurnResult{}, runErr
	}

	hasAskDenial := false
	for _, d := range res.PermissionDenials {
		if d.ToolName == "AskUserQuestion" {
			hasAskDenial = true
			break
		}
	}
	// If AskUserQuestion was not actually denied, flush whatever landed in askBuf
	// (normally empty; non-empty only if the tool was announced but not denied).
	if !hasAskDenial && askBuf.Len() > 0 {
		io.Copy(out, &askBuf) //nolint:errcheck
	}

	// Permission-denial retry (Claude CLI only). handlePermissionDenials reads
	// sess.EscalationSessionID to call RunResume; set it to the live session ID
	// so it works whether this is a first turn (new ID) or a resume.
	if sessionID != "" {
		orig := sess.EscalationSessionID
		sess.EscalationSessionID = sessionID
		res = handlePermissionDenials(ctx, sess, agent, res, r.newInput(), out, r.pc, nonce, primaryName, escalationName)
		// Handle pre-flight "Stream closed" denials (directory-trust check fires before
		// the permission-prompt-tool stdio handler). These are separate from structured
		// PermissionDenials and need their own grant + retry flow.
		if len(res.StreamClosedDenials) > 0 {
			res = handleStreamClosedDenials(ctx, sess, agent, res, r.newInput(), out, r.pc, nonce, primaryName, escalationName)
		}
		sess.EscalationSessionID = orig
	}

	// Background-workflow auto-resume: the Workflow tool always runs in background
	// and --print exits as soon as Claude responds. Wait for the workflow journal
	// to record a result (local file poll — no API cost), then issue exactly one
	// --resume so Claude receives the task-notification and reports completion.
	// If a newly resumed turn launches another workflow, the loop repeats.
	//
	// ctx may already be deadline-expired by the time the workflow finishes, so
	// the poll and the resume both use a fresh context derived from the session
	// root (no deadline) that only cancels on explicit user interruption (Ctrl+C).
	for res.HasPendingWorkflow && sessionID != "" {
		fmt.Fprintf(out, "\n\033[2m[workflow running]\033[0m\n")
		pollCtx, pollCancel := withoutDeadline(ctx)
		err := waitForWorkflowResult(pollCtx, res.PendingWorkflowDir)
		pollCancel()
		if err != nil {
			return TurnResult{}, err
		}
		baseCtx, baseCancel := withoutDeadline(ctx)
		resumeCtx, resumeCancel := context.WithTimeout(baseCtx, workflowResumeTimeout)
		var resumeRes claude.ParseResult
		var resumeErr error
		// A non-empty prompt is required — empty prompt triggers a "no deferred
		// tool marker" error. This sentinel causes Claude to process the pending
		// task-notification and report workflow completion.
		resumeRes, resumeErr = agent.RunResume(resumeCtx, sessionID, "", "", "workflow status?", out)
		resumeCancel()
		baseCancel()
		if resumeErr != nil {
			return TurnResult{}, resumeErr
		}
		res.InputTokens += resumeRes.InputTokens
		res.OutputTokens += resumeRes.OutputTokens
		res.CacheReadInputTokens += resumeRes.CacheReadInputTokens
		res.CacheCreationInputTokens += resumeRes.CacheCreationInputTokens
		res.TotalCostUSD += resumeRes.TotalCostUSD
		if resumeRes.Text != "" {
			res.Text = resumeRes.Text
			res.EndsWithQ = resumeRes.EndsWithQ
		}
		res.HasPendingWorkflow = resumeRes.HasPendingWorkflow
		res.PendingWorkflowDir = resumeRes.PendingWorkflowDir
	}

	return TurnResult{
		Text:         res.Text,
		NewSessionID: sessionID,
		EndsWithQ:    res.EndsWithQ,
		InputTokens:  res.InputTokens,
		OutputTokens: res.OutputTokens,
		CacheRead:    res.CacheReadInputTokens,
		CacheCreate:  res.CacheCreationInputTokens,
		CostUSD:      res.TotalCostUSD,
	}, nil
}

func (r *cliRunner) RunToolCall(ctx context.Context, _ config.Config, prompt string, out io.Writer) (string, error) {
	if !r.agent.SkipPermissions() {
		return "", errors.New("claude-cli tool-agent requires dangerously_skip_permissions: true")
	}
	// Run a fresh first-turn (no session resume) with empty context — tool calls
	// are stateless one-shot requests, not continuation of an escalation session.
	_, res, err := r.agent.RunFirst(ctx, "", "", prompt, out)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// ── subprocessRunner ─────────────────────────────────────────────────────────

type subprocessRunner struct {
	agent      *subprocess.Agent
	name       string
	mcpServers []config.MCPServerConfig
	mcpToolSet *mcp.ToolSet
}

func newSubprocessRunner(agent *subprocess.Agent, name string) *subprocessRunner {
	return &subprocessRunner{agent: agent, name: name}
}

// withMCPToolSet returns a copy of the subprocessRunner with an attached ToolSet.
// The tool schemas are injected as a context block so the subprocess model knows
// what MCP tools are available (option 3 — context injection).
func (r *subprocessRunner) withMCPToolSet(servers []config.MCPServerConfig, ts *mcp.ToolSet) *subprocessRunner {
	c := *r
	c.mcpServers = servers
	c.mcpToolSet = ts
	return &c
}

func (r *subprocessRunner) Name() string { return r.name }
func (r *subprocessRunner) IsCLI() bool  { return false }
func (r *subprocessRunner) Ping() error  { return r.agent.Ping() }

func (r *subprocessRunner) Execute(
	ctx context.Context,
	cfg config.Config,
	sess *session.Session,
	mem *memory.Store,
	role AgentRole,
	ctxMode escalation.ContextMode,
	sessionID, nonce string,
	percepts []string,
	injectInstructions bool,
	prompt string,
	cbs TurnCallbacks,
	out io.Writer,
) (TurnResult, error) {
	primaryName := cfg.ActiveAgent().Name
	escalationName := cfg.EscalationAgentConfig().Name

	agent := r.agent
	if mem != nil && cbs.OnPercept != nil {
		agent = agent.WithOnPercept(cbs.OnPercept, nonce, primaryName, escalationName)
	}
	if cbs.OnNeed != nil {
		agent = agent.WithOnNeed(cbs.OnNeed, nonce)
	}

	var escalationReason string
	if cbs.OnEscalate != nil {
		agent = agent.WithOnEscalate(func(reason string) {
			escalationReason = reason
			cbs.OnEscalate(reason)
		}, nonce)
	}

	var staticCtx, dynamicCtx string
	switch role {
	case RoleWorkflow:
		// Workflow executors receive no session orientation — their context comes
		// entirely from the workflow prompt injected by the caller.
	case RolePrimary:
		staticCtx = escalation.BuildPrimaryStaticContext(nonce, percepts, ctxMode, injectInstructions, primaryName, escalationName)
		dynamicCtx = escalation.BuildPrimaryDynamicContext(sess, ctxMode)
	default: // RoleEscalation
		staticCtx = escalation.BuildStaticContext(nonce, percepts, ctxMode, injectInstructions, primaryName, escalationName)
		dynamicCtx = escalation.BuildDynamicContext(sess, ctxMode)
	}
	if r.mcpToolSet != nil {
		mcpBlock := escalation.BuildMCPContextBlock(r.mcpServers, r.mcpToolSet.Schemas(ctx))
		if staticCtx != "" {
			staticCtx += mcpBlock
		} else {
			dynamicCtx += mcpBlock
		}
	}

	var (
		res    subprocess.ParseResult
		runErr error
	)
	if ctxMode == escalation.ContextModeResume || (ctxMode == escalation.ContextModeReturning && sessionID != "") {
		res, runErr = agent.RunResume(ctx, sessionID, staticCtx, dynamicCtx, prompt, out)
	} else {
		var newID string
		newID, res, runErr = agent.RunFirst(ctx, staticCtx, dynamicCtx, prompt, out)
		if runErr == nil {
			sessionID = newID
		}
	}
	if runErr != nil {
		return TurnResult{}, runErr
	}

	return TurnResult{
		Text:             res.Text,
		NewSessionID:     sessionID,
		EscalationReason: escalationReason,
		EndsWithQ:        res.EndsWithQ,
		InputTokens:      res.InputTokens,
		OutputTokens:     res.OutputTokens,
		CacheRead:        res.CacheReadInputTokens,
		CacheCreate:      res.CacheCreationInputTokens,
		CostUSD:          res.TotalCostUSD,
	}, nil
}

func (r *subprocessRunner) RunToolCall(ctx context.Context, _ config.Config, prompt string, out io.Writer) (string, error) {
	// Subprocess agents are stateless per-call; RunFirst with empty context works directly.
	_, res, err := r.agent.RunFirst(ctx, "", "", prompt, out)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// waitForWorkflowResult blocks until the workflow's journal.jsonl contains a
// {"type":"result"} entry, or ctx is cancelled. ctx must be deadline-free
// (callers should use withoutDeadline before calling). Returns immediately
// when transcriptDir is empty.
func waitForWorkflowResult(ctx context.Context, transcriptDir string) error {
	if transcriptDir == "" {
		return nil
	}
	journalPath := transcriptDir + "/journal.jsonl"
	for {
		if workflowJournalHasResult(journalPath) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(workflowJournalPollInterval):
		}
	}
}

// workflowJournalHasResult returns true if journal.jsonl contains at least one
// {"type":"result"} entry, indicating the workflow has produced a final result.
func workflowJournalHasResult(journalPath string) bool {
	f, err := os.Open(journalPath)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var entry struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(sc.Bytes(), &entry) == nil && entry.Type == "result" {
			return true
		}
	}
	return false
}

// withoutDeadline returns a cancel func and a context that has no deadline but
// cancels when ctx is cancelled for any reason other than a turn timeout.
// Callers must call the returned cancel to release resources.
func withoutDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	child, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		select {
		case <-ctx.Done():
			if context.Cause(ctx) != nil && context.Cause(ctx).Error() != "turn timeout" {
				cancel()
			}
			// turn timeout: leave child alive so the caller can continue.
		case <-child.Done():
		}
	}()
	return child, cancel
}

func agentConfigForRole(cfg config.Config, role AgentRole) config.AgentConfig {
	if role == RolePrimary {
		return cfg.ActiveAgent()
	}
	return cfg.EscalationAgentConfig()
}

// localBuiltinTools is a human-readable list of milk's built-in local-agent tools,
// used to populate the {{milk:tools}} placeholder in custom prompts.
const localBuiltinTools = "bash, find_files, grep, read_file, write_file, edit_file, open_file, delete_file, move_file, http_request, list_dir, http_get, get_session_context, escalate, current_need, record_memory, get_memory, export_session"

// buildPromptVars assembles the PlaceholderVars for agentprompt.Render from the
// current session state and percept list.
func buildPromptVars(sess *session.Session, percepts []string, _ config.Config) agentprompt.PlaceholderVars {
	var need string
	if sess != nil {
		need = sess.CurrentNeed
	}

	var escSummary string
	if sess != nil {
		escSummary = sess.LastEscalationSummary
	}

	var memText string
	if len(percepts) > 0 {
		memText = escalation.FormatPercepts(percepts)
	}

	return agentprompt.PlaceholderVars{
		Memory:     memText,
		Need:       need,
		Escalation: escSummary,
		Tools:      localBuiltinTools,
	}
}
