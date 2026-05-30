package local

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/tags"
)

const maxToolIterations = 10

// EscalationSignal is returned when the local model requests escalation to the escalation agent.
type EscalationSignal struct {
	Reason string
}

func (e *EscalationSignal) Error() string {
	return "escalate: " + e.Reason
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Index    int              `json:"index,omitempty"` // streaming-only; omitted when serialising history
	Type     string           `json:"type"`
	Function toolCallFunction `json:"function"`
}

type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatRequest struct {
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Tools       []map[string]any `json:"tools,omitempty"`
	Stream      bool             `json:"stream"`
	Temperature float64          `json:"temperature"`
	Seed        int64            `json:"seed,omitempty"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// Agent is a local LLM agent backed by any OpenAI-compatible inference server,
// or the AWS Bedrock Converse API when useBedrockNative is true.
type Agent struct {
	baseURL          string
	model            string
	chatPath         string // inference path; defaults to "/v1/chat/completions"
	otelDir          string
	skipHealthCheck  bool   // true for remote providers that have no /health endpoint (e.g. Bedrock)
	useBedrockNative bool   // true when provider = "bedrock"; uses Converse API instead of /v1/chat/completions
	skipRepeatCheck  bool   // true when acting as the escalation target: the repeated-prompt check must not fire
	escalationName   string // non-empty when acting as escalation target; used in the role-aware system prompt
	skipPerms        bool   // true when dangerously_skip_permissions is on: bypass all tool prompts
	permStore        *PermStore
	permAsk          func(tool, summary string) bool // returns true if user allows; nil = deny all (non-TUI)
	client           *http.Client
	detectedFormat   ToolFormat         // confirmed format from last tool-bearing turn
	tokenCmd         *tokenCmdTransport // non-nil when token_cmd is configured; used for eager pre-fetch
	sigv4            *sigv4Transport    // non-nil for Bedrock; used to wire the onRefresh callback
	tagNonce         string
	agentNames       []string // [primaryName, escalationName] for consumer-hint parsing
	onNeed           func(string)
	onPercept        func(content, consumerHint string)
}

// AsEscalationTarget returns a shallow copy of the agent configured for the
// escalation role: skipRepeatCheck is set (the escalation agent must handle
// prompts the primary already tried) and escalationName is stored for the
// role-aware system prompt. name is the human-readable escalation agent name
// shown in the TUI (e.g. "haiku-aws").
func (a *Agent) AsEscalationTarget(name string) *Agent {
	copy := *a
	copy.skipRepeatCheck = true
	copy.escalationName = name
	return &copy
}

// WithTagCallbacks returns a shallow copy of the agent configured to intercept
// <milk:need:NONCE> and <milk:percept:NONCE> tags in the response stream.
// nonce must match the value injected into the system prompt via NeedInstruction/MemoryInstruction.
// primaryName and escalationName are used to parse @<name>: consumer-hint prefixes in percept bodies.
func (a *Agent) WithTagCallbacks(nonce, primaryName, escalationName string, onNeed func(string), onPercept func(content, consumerHint string)) *Agent {
	copy := *a
	copy.tagNonce = nonce
	copy.agentNames = []string{primaryName, escalationName}
	copy.onNeed = onNeed
	copy.onPercept = onPercept
	return &copy
}

// HasTokenCmd reports whether this agent uses a token_cmd for authentication.
func (a *Agent) HasTokenCmd() bool { return a.tokenCmd != nil }

// WarmToken eagerly pre-fetches the Bearer token when token_cmd is configured.
// It returns an error if the command fails, nil otherwise (including when no
// token_cmd is set). Calling it in a background goroutine lets the TUI start
// immediately while the token is fetched concurrently.
func (a *Agent) WarmToken() error {
	if a.tokenCmd == nil {
		return nil
	}
	_, err := a.tokenCmd.getToken()
	return err
}

// WithOnSigV4Refresh registers a callback that is called after each automatic
// mid-turn Bedrock credential renewal attempt. err is nil on success.
// No-op when the agent is not a Bedrock/SigV4 agent.
func (a *Agent) WithOnSigV4Refresh(fn func(err error)) {
	if a.sigv4 != nil {
		a.sigv4.onRefresh = fn
	}
}

func New(baseURL, model string) *Agent {
	return &Agent{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		client:  &http.Client{Timeout: 5 * time.Minute},
	}
}

// inferenceURL returns the full URL for the chat/completions endpoint.
// chatPath defaults to "/v1/chat/completions" when empty.
func (a *Agent) inferenceURL() string {
	p := a.chatPath
	if p == "" {
		p = "/v1/chat/completions"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return a.baseURL + p
}

// NewFromConfig creates an Agent from an AgentConfig.
func NewFromConfig(ac config.AgentConfig) *Agent {
	inner := buildBaseTransport(ac)
	var transport http.RoundTripper = inner

	provider := strings.ToLower(strings.TrimSpace(ac.Provider))
	switch provider {
	case "bedrock":
		service := ac.AWSService
		if service == "" {
			service = "bedrock"
		}
		// Credentials: explicit config takes precedence, then env vars.
		keyID := ac.AWSKeyID
		if keyID == "" {
			keyID = os.Getenv("AWS_ACCESS_KEY_ID")
		}
		secret := ac.AWSSecret
		if secret == "" {
			secret = os.Getenv("AWS_SECRET_ACCESS_KEY")
		}
		token := ac.AWSToken
		if token == "" {
			token = os.Getenv("AWS_SESSION_TOKEN")
		}
		region := ac.AWSRegion
		if region == "" {
			region = os.Getenv("AWS_REGION")
		}
		if region == "" {
			region = os.Getenv("AWS_DEFAULT_REGION")
		}
		if region == "" {
			region = regionFromBedrockURL(ac.URL)
		}
		sv4 := &sigv4Transport{
			inner:      inner,
			region:     region,
			service:    service,
			refreshCmd: ac.AWSRefreshCmd,
			creds:      sigv4Creds{keyID: keyID, secret: secret, token: token},
		}
		return &Agent{
			baseURL:          strings.TrimRight(ac.URL, "/"),
			model:            ac.Model,
			chatPath:         ac.ChatPath,
			skipHealthCheck:  true,
			useBedrockNative: true,
			client:           &http.Client{Timeout: 5 * time.Minute, Transport: sv4},
			sigv4:            sv4,
		}
	case "", "local":
		// plain transport; extra headers may still apply
	default:
		// treat as Bearer-token provider (OpenRouter, Together.ai, Groq, GitHub Models, …)
		//
		// Azure OpenAI workaround: Azure uses "api-key" header + a non-standard URL path instead
		// of Bearer auth. Use provider="" or "local", set url to the full deployment endpoint,
		// and add {"api-key": "<key>"} to headers. A dedicated azure provider with URL
		// templating is tracked in GitHub Issues.
	}

	// Layer token refresh or static Bearer, then extra headers.
	var tct *tokenCmdTransport
	if ac.TokenCmd != "" && provider != "bedrock" {
		// token_cmd takes precedence: use a refreshing transport so short-lived
		// tokens (e.g. "gh auth token") are re-fetched on 401/403.
		tct = &tokenCmdTransport{inner: transport, cmd: ac.TokenCmd}
		transport = tct
	} else if ac.APIKey != "" && provider != "bedrock" {
		transport = &headerTransport{
			inner:   transport,
			headers: map[string]string{"Authorization": "Bearer " + ac.APIKey},
		}
	}
	// Extra headers (e.g. Copilot editor headers, Azure api-key) on top.
	if len(ac.Headers) > 0 {
		headers := make(map[string]string)
		for k, v := range ac.Headers {
			headers[k] = v
		}
		transport = &headerTransport{inner: transport, headers: headers}
	}

	return &Agent{
		baseURL:  strings.TrimRight(ac.URL, "/"),
		model:    ac.Model,
		chatPath: ac.ChatPath,
		tokenCmd: tct,
		client:   &http.Client{Timeout: 5 * time.Minute, Transport: transport},
	}
}

// buildBaseTransport returns an http.RoundTripper with TLS configured per ac.
// Falls back to http.DefaultTransport when no TLS overrides are set.
func buildBaseTransport(ac config.AgentConfig) http.RoundTripper {
	if !ac.TLSSkipVerify && ac.TLSCACert == "" {
		return http.DefaultTransport
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: ac.TLSSkipVerify} //nolint:gosec
	if ac.TLSCACert != "" {
		pem, err := os.ReadFile(ac.TLSCACert)
		if err == nil {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			tlsCfg.RootCAs = pool
		}
	}
	return &http.Transport{TLSClientConfig: tlsCfg}
}

// WithPermissions wires the permission store and ask callback into the agent.
// permStore may be nil (no persistence). ask is called when a tool requires
// a permission prompt; it returns true if the user allows. When ask is nil,
// all prompts are denied (appropriate for non-interactive single-prompt mode).
func (a *Agent) WithPermissions(ps *PermStore, ask func(tool, summary string) bool) *Agent {
	a.permStore = ps
	a.permAsk = ask
	return a
}

// WithSkipPermissions returns a copy of the agent with the skip-permissions flag set.
// When true, all tool permission checks are bypassed without prompting.
func (a *Agent) WithSkipPermissions(skip bool) *Agent {
	copy := *a
	copy.skipPerms = skip
	return &copy
}

// WithOtelDir sets the otel directory so the agent can offer get_metrics.
func (a *Agent) WithOtelDir(dir string) *Agent {
	a.otelDir = dir
	return a
}

// systemPromptShared is the role-independent core: tool rules, memory rules,
// file/directory conventions, and git protocol. Both primary and escalation
// agents receive this verbatim.
const systemPromptShared = `Rules:
- When you need to run a command, read, write, or edit a file, list a directory, or fetch a URL, call the appropriate tool. Never guess or hallucinate the result.
- To create or overwrite a file use write_file. To make a targeted change to an existing file use edit_file. Never refuse file operations or tell the user to do them manually.
- To find files by name or pattern (e.g. "*_test.go", "*.md", "Makefile") use find_files — never use grep for this. Use grep only to search inside file contents.
- list_dir shows only the top level of a directory; never conclude that files or subdirectories are absent based solely on a list_dir result. To check whether files of a given type exist anywhere in the project, use find_files with the working directory as root.
- After issuing a tool call, stop. Do not describe what the result might be. Wait for the actual output.
**MANDATORY — memory tool actions**: The following require immediate tool calls with NO preamble or confirmation:
  - User asks about past context or preferences → call get_memory NOW before responding.
  - User states a preference, decision, or fact → call record_memory NOW.
  - User says "forget", "remove", "delete" about a percept (by ID, #ID, or description) → call forget_memory NOW. Strip any leading "#" from the ID before passing it. Never say "done" or confirm the action without actually calling the tool.
- Call get_metrics when the user asks about memory usage, percept counts, observability status, or metric values.
- The working directory is provided below. NEVER ask the user to provide a project, files, or code when the working directory is available. When the user says "this project", "here", "the code", "take a look", or anything that implies a codebase without reducing one, call list_dir on the working directory immediately, then read relevant files. Always act first, ask only if the working directory alone is genuinely insufficient.
- For GitHub issues, pull requests, and repo data, use bash with the gh CLI (e.g. "gh issue list", "gh issue view 42", "gh pr list"). Never ask the user to look these up manually.
**MANDATORY — git operations**: Before executing any git commit, push, or merge, follow this exact protocol:
1. Call bash with "git status" and "git diff --stat HEAD" to see what is staged or changed.
2. Call get_session_context with agent: "local" to check whether the primary local agent performed those changes in this session.
3. Call get_session_context with agent: "escalation" to check whether the escalation agent made those changes.
4. Only proceed with a commit if step 2 OR step 3 returned clear context that explains the changes and their purpose. Use that context to write an accurate commit message.
5. If neither step 2 nor step 3 returns relevant context, STOP. Do not commit. Tell the user: "I found no session context explaining these changes — please tell me what they are for before I commit." Never invent a commit message for changes you cannot account for.`

// systemPromptPrimary is prepended for the primary (local) role.
const systemPromptPrimary = `You are a coding and shell automation assistant with access to tools: bash, find_files, grep, read_file, write_file, edit_file, list_dir, http_get, get_session_context, record_memory, get_memory, list_memory, forget_memory, get_metrics, escalate.

` + systemPromptShared + `
- If the user refers to something ("that file", "the previous error", "what we discussed") without enough context, call get_session_context to retrieve shared history. Prefer last_n: 5 for recent context, pattern: "<keyword>" to find a specific fact, or agent: "escalation" to see only the escalation agent's prior turns. Only omit all filters when you genuinely need the full history.
- Use escalate only for architectural design, complex multi-file refactoring, or tasks beyond your capabilities.
**MANDATORY — unknown recent work**: If the user references any past action, change, or artifact you have no direct memory of — including words like "that fix", "the changes", "what you did", "the PR", "that refactor", "the feature", or any named code entity you cannot recall — you MUST call get_session_context with agent: "escalation" BEFORE generating any response. Do not guess, summarise, or attempt to answer without checking first. After retrieving context: (1) if the work was done by the escalation agent, immediately respond "That was done by the escalation agent — do you want me to escalate so it can continue with full context?" and offer escalate. (2) if no relevant context is found, say so explicitly and ask the user to clarify. Never fabricate a summary of work you did not perform.`

// systemPromptEscalationFmt is a fmt.Sprintf template for the escalation role.
// %s is the escalation agent name (e.g. "haiku-aws").
const systemPromptEscalationFmt = `You are a coding and shell automation assistant acting as the escalation agent (%s) in a multi-agent system. The primary local agent has handed off this task because it exceeds its capabilities. You have access to the full shared session history. Tools available: bash, find_files, grep, read_file, write_file, edit_file, list_dir, http_get, get_session_context, record_memory, get_memory, list_memory, forget_memory, get_metrics.

` + systemPromptShared + `
- If the user refers to something without enough context, call get_session_context to retrieve shared history. Prefer agent: "local" to see what the primary agent did, or agent: "escalation" for your own prior turns.
- You are the escalation target — do not attempt to escalate further.
**MANDATORY — unknown recent work**: If the user references any past action, change, or artifact you have no direct memory of, you MUST call get_session_context with agent: "local" BEFORE generating any response to check whether the primary agent performed it. If no context is found, say so and ask the user to clarify. Never fabricate a summary of work you did not perform.`

func buildSystemPrompt(cwd, escalationName string) string {
	var base string
	if escalationName != "" {
		base = fmt.Sprintf(systemPromptEscalationFmt, escalationName)
	} else {
		base = systemPromptPrimary
	}
	if cwd == "" {
		return base
	}
	return base + "\n\nWorking directory: " + cwd
}

func cwdContext(cwd string) string {
	result, _ := runListDir(map[string]any{"path": cwd})
	return "Working directory listing (" + cwd + "):\n" + result
}

// normalizePrompt lowercases and collapses whitespace for repetition comparison.
func normalizePrompt(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.Join(strings.Fields(s), " ")))
}

// repetitionScore scores how much the user is repeating userPrompt in recent
// history and returns whether escalation should trigger.
//
// Algorithm: collect the last 10 user messages from history. For each one that
// matches the current prompt, add 1/distance to the score, where distance is
// the 1-based position counting back from the current turn through user turns
// only (the immediately preceding user turn has distance 1, the one before
// that distance 2, etc.). If the accumulated score ≥ 1.0, escalation fires.
//
// Examples (→ = match, · = no match, rightmost = most recent user turn):
//
//	· → · ·   score = 1/2 = 0.5  → no escalation
//	· · → ·   score = 1/3       → no escalation
//	· → → ·   score = 1/2 + 1/3 = 0.83  → no escalation
//	→ · → ·   score = 1/2 + 1/4 = 0.75  → no escalation
//	· → · →   score = 1/1 + 1/3 = 1.33  → escalate
//	→ →       score = 1/1 + 1/2 = 1.5   → escalate
//	· ·  →    score = 1/1 = 1.0  → escalate (immediate repeat)
//
// This check lives here rather than in the router because it requires []Message
// history — the local agent's internal wire format. The router only sees the
// raw prompt string and session metadata; it has no per-model turn history to
// compare against.
const minRepeatCheckLen = 20

const repetitionWindow = 10     // number of recent user turns to consider
const repetitionThreshold = 0.6 // escalate when score exceeds this

func isRepeatedPrompt(history []Message, userPrompt string) bool {
	norm := normalizePrompt(userPrompt)
	if len(norm) < minRepeatCheckLen {
		return false
	}

	// Collect the last repetitionWindow user messages, most-recent last.
	var userMsgs []string
	for _, m := range history {
		if m.Role == "user" {
			userMsgs = append(userMsgs, normalizePrompt(m.Content))
		}
	}
	if len(userMsgs) > repetitionWindow {
		userMsgs = userMsgs[len(userMsgs)-repetitionWindow:]
	}

	// Walk backwards (distance 1 = most recent user turn).
	var score float64
	for i := len(userMsgs) - 1; i >= 0; i-- {
		distance := len(userMsgs) - i // 1-based: 1 = most recent
		if userMsgs[i] == norm {
			score += 1.0 / float64(distance)
			if score >= repetitionThreshold {
				return true
			}
		}
	}
	return false
}

// Run executes a prompt with the given conversation history, streaming tokens
// to out. Returns an EscalationSignal error if the model requests escalation.
// history is the prior turns; userPrompt is the new user Message.
func (a *Agent) Run(ctx context.Context, history []Message, userPrompt string, out io.Writer, sess *session.Session, mem *memory.Store) ([]Message, error) {
	if history == nil {
		history = []Message{}
	}

	// Escalate immediately if the user is repeating the same question.
	// Skipped when this agent is acting as the escalation target: by definition
	// it must handle prompts the primary agent already tried.
	if !a.skipRepeatCheck && isRepeatedPrompt(history, userPrompt) {
		return history, &EscalationSignal{Reason: "user repeated the same question without expressing satisfaction"}
	}

	msgs := []Message{{Role: "system", Content: buildSystemPrompt(sess.CWD, a.escalationName)}}
	msgs = append(msgs, history...)
	if sess.CWD != "" {
		msgs = append(msgs, Message{Role: "system", Content: cwdContext(sess.CWD)})
	}
	if a.tagNonce != "" {
		msgs = append(msgs, Message{Role: "system", Content: escalation.NeedInstruction(a.tagNonce) + escalation.MemoryInstruction(a.tagNonce)})
	}
	msgs = append(msgs, Message{Role: "user", Content: userPrompt})
	tools := schemas(mem, a.otelDir, sess)

	if a.tagNonce != "" {
		if a.onNeed != nil {
			out = &tags.TagWriter{W: out, OpenPrefix: tags.NeedOpenPrefix, OnTag: a.onNeed, RecordNonce: a.tagNonce}
		}
		if a.onPercept != nil {
			out = &tags.PerceptWriter{W: out, OnPercept: a.onPercept, RecordNonce: a.tagNonce, AgentNames: a.agentNames}
		}
	}

	executedKeys := map[string]bool{}

	for i := 0; i < maxToolIterations; i++ {
		resp, fallbackRaw, toolCalls, err := a.streamCompletion(ctx, msgs, tools, out)
		if err != nil {
			return msgs, err
		}

		if len(toolCalls) == 0 {
			// No tool calls: either a final text response, or the model emitting EOS
			// after completing its tool loop (empty response). Both are terminal.
			msgs = append(msgs, Message{Role: "assistant", Content: resp})
			return msgs, nil
		}

		// Deduplicate: if every tool call in this turn was already executed with
		// the same arguments, the model is stuck in a loop — treat as terminal.
		allSeen := true
		for _, tc := range toolCalls {
			key := tc.Function.Name + "\x00" + tc.Function.Arguments
			if !executedKeys[key] {
				allSeen = false
				break
			}
		}
		if allSeen {
			msgs = append(msgs, Message{Role: "assistant", Content: resp})
			return msgs, nil
		}
		for _, tc := range toolCalls {
			executedKeys[tc.Function.Name+"\x00"+tc.Function.Arguments] = true
		}

		var esc *EscalationSignal
		msgs, esc = a.executeToolCalls(ctx, msgs, toolCalls, fallbackRaw, out, sess, mem)
		if esc != nil {
			return msgs, esc
		}
	}

	return msgs, fmt.Errorf("exceeded maximum tool iterations (%d)", maxToolIterations)
}

// toolNeedsPermission reports whether a tool requires user approval before execution.
// Read-only and internal tools are always permitted; side-effecting tools require a grant.
func toolNeedsPermission(name string) bool {
	switch name {
	case "bash", "write_file", "edit_file", "http_get":
		return true
	}
	return false
}

// checkPermission returns true if the tool may proceed. It checks skipPerms,
// then the persistent store, then (if needed) asks the user interactively and
// persists the answer. denied is returned as a toolResult string when false.
func (a *Agent) checkPermission(tool, summary string) (allowed bool, denied string) {
	if a.skipPerms {
		return true, ""
	}
	if a.permStore != nil && a.permStore.IsAllowed(tool) {
		return true, ""
	}
	if a.permAsk == nil {
		// Non-interactive mode: deny and surface a clear error.
		return false, toolResult{Error: "tool " + tool + " requires permission — run interactively or enable skip-permissions"}.String()
	}
	if a.permAsk(tool, summary) {
		if a.permStore != nil {
			a.permStore.Allow(tool) //nolint:errcheck
		}
		return true, ""
	}
	return false, toolResult{Error: "tool " + tool + " was denied by user"}.String()
}

// executeToolCalls dispatches all tool calls and appends the results to msgs.
// Always uses the structured OpenAI tool_calls wire format — the server's chat
// template renders it into the model-specific markup (<tool_call>, etc.) and
// wraps tool results in <tool_response> automatically.
func (a *Agent) executeToolCalls(ctx context.Context, msgs []Message, toolCalls []toolCall, _ string, out io.Writer, sess *session.Session, mem *memory.Store) ([]Message, *EscalationSignal) {
	msgs = append(msgs, Message{Role: "assistant", ToolCalls: toolCalls})
	for _, tc := range toolCalls {
		printToolLine(out, tc)

		if toolNeedsPermission(tc.Function.Name) {
			var argMap map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &argMap) //nolint:errcheck
			summary := toolArgSummary(argMap)
			if ok, denied := a.checkPermission(tc.Function.Name, summary); !ok {
				msgs = append(msgs, Message{Role: "tool", Content: denied, ToolCallID: tc.ID})
				continue
			}
		}

		result, escalate := dispatchTool(ctx, tc.Function.Name, tc.Function.Arguments, sess, mem, a.otelDir)
		if escalate {
			var escalateArgs struct {
				Reason string `json:"reason"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &escalateArgs) //nolint:errcheck
			return msgs, &EscalationSignal{Reason: escalateArgs.Reason}
		}
		msgs = append(msgs, Message{Role: "tool", Content: result, ToolCallID: tc.ID})
	}
	return msgs, nil
}

// printToolLine writes a one-line dim tool-usage hint to out before a tool is
// dispatched, mirroring what Claude shows for permission requests.
// Format:  ⚙ <name>: <short summary of key argument>
func printToolLine(out io.Writer, tc toolCall) {
	var args map[string]any
	json.Unmarshal([]byte(tc.Function.Arguments), &args) //nolint:errcheck

	summary := toolArgSummary(args)
	if summary != "" {
		fmt.Fprintf(out, "\n\033[2m⚙ %s: %s\033[0m\n", tc.Function.Name, summary)
	} else {
		fmt.Fprintf(out, "\n\033[2m⚙ %s\033[0m\n", tc.Function.Name)
	}
}

// toolArgSummary extracts the most informative single argument value for display.
func toolArgSummary(args map[string]any) string {
	for _, key := range []string{"command", "path", "file_path", "url", "query", "pattern", "reason", "content"} {
		if v, ok := args[key].(string); ok && v != "" {
			if len(v) > 60 {
				return v[:57] + "..."
			}
			return v
		}
	}
	return ""
}

// streamCompletion sends a chat completion request and streams the response.
// Routes to the Bedrock Converse streaming API when useBedrockNative is set;
// otherwise uses the OpenAI-compatible /v1/chat/completions endpoint.
func (a *Agent) streamCompletion(ctx context.Context, msgs []Message, tools []map[string]any, out io.Writer) (string, string, []toolCall, error) {
	if a.useBedrockNative {
		return a.bedrockStreamCompletion(ctx, msgs, tools, out)
	}
	req := chatRequest{
		Model:       a.model,
		Messages:    msgs,
		Tools:       tools,
		Stream:      true,
		Temperature: 0.2,
		Seed:        time.Now().UnixNano(),
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", "", nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.inferenceURL(), bytes.NewReader(body))
	if err != nil {
		return "", "", nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return "", "", nil, fmt.Errorf("inference server unreachable: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(httpResp.Body)
		return "", "", nil, fmt.Errorf("inference server error %d: %s", httpResp.StatusCode, b)
	}

	det := NewStreamDetector(a.detectedFormat)
	partialTools := map[int]*toolCall{}
	var textBuf strings.Builder

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	toolCalls, err := a.scanSSE(scanner, det, partialTools, &textBuf, out)
	if err != nil {
		return "", "", nil, err
	}

	if det.Format != ToolFormatUnknown {
		a.detectedFormat = det.Format
	}
	return a.classifyStreamResult(det, toolCalls, textBuf.String(), out)
}

// classifyStreamResult interprets what the stream produced and returns the
// canonical (text, fallbackRaw, toolCalls, error) tuple.
func (a *Agent) classifyStreamResult(det *StreamDetector, nativeCalls []toolCall, rawText string, out io.Writer) (string, string, []toolCall, error) {
	// Native tool calls (delta field) take priority.
	if len(nativeCalls) > 0 {
		if rawText != "" {
			fmt.Fprintln(out)
		}
		return rawText, "", nativeCalls, nil
	}

	// Detector-extracted tool calls (in-stream block).
	if det.InBlock() || det.RawBlock() != "" {
		if calls := det.Extract(); len(calls) > 0 {
			a.detectedFormat = det.Format
			return "", det.RawBlock(), calls, nil
		}
		// Block was captured but contains no tool call (e.g. a code example in a
		// text response). Flush its content so it appears in the output.
		if raw := det.RawBlock(); raw != "" {
			block := det.ActiveOpen() + raw
			fmt.Fprint(out, block)
			fmt.Fprintln(out)
			return rawText + block, "", nil, nil
		}
	}

	// Fallback: post-hoc scan of accumulated text (handles partial/unclosed fences).
	if rawText != "" {
		if parsed := extractToolCalls(rawText); len(parsed) > 0 {
			return "", rawText, parsed, nil
		}
	}

	// Plain text response.
	if rawText != "" {
		fmt.Fprintln(out)
	}
	return rawText, "", nil, nil
}

// scanSSE reads SSE lines from the scanner, feeding content tokens through the
// detector and accumulating native tool-call deltas in partialTools.
func (a *Agent) scanSSE(
	scanner *bufio.Scanner,
	det *StreamDetector,
	partialTools map[int]*toolCall,
	textBuf *strings.Builder,
	out io.Writer,
) ([]toolCall, error) {
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			processContentToken(choice.Delta.Content, det, textBuf, out)
			accumulateNativeToolCalls(choice.Delta.ToolCalls, partialTools)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return collectNativeToolCalls(partialTools), nil
}

func processContentToken(token string, det *StreamDetector, textBuf *strings.Builder, out io.Writer) {
	if token == "" {
		return
	}
	flush, _ := det.Feed(token)
	if len(flush) > 0 {
		textBuf.Write(flush)
		fmt.Fprint(out, string(flush))
	}
}

func accumulateNativeToolCalls(tcs []toolCall, partialTools map[int]*toolCall) {
	for _, tc := range tcs {
		pt, ok := partialTools[tc.Index]
		if !ok {
			pt = &toolCall{Type: "function"}
			partialTools[tc.Index] = pt
		}
		if tc.ID != "" {
			pt.ID = tc.ID
		}
		pt.Function.Name += tc.Function.Name
		pt.Function.Arguments += tc.Function.Arguments
	}
}

func collectNativeToolCalls(partialTools map[int]*toolCall) []toolCall {
	var out []toolCall
	for i := 0; i < len(partialTools); i++ {
		if tc := partialTools[i]; tc != nil && tc.Function.Name != "" {
			out = append(out, *tc)
		}
	}
	return out
}

// Classify asks the model to classify whether a prompt should be handled locally
// or escalated. Routes to Bedrock Converse when useBedrockNative is set.
func (a *Agent) Classify(ctx context.Context, prompt string) (bool, error) {
	if a.useBedrockNative {
		return a.bedrockClassify(ctx, prompt)
	}
	classifyPrompt := `Respond with exactly one word: "local" or "escalate".
Use "escalate" only when the task clearly requires complex multi-file refactoring, architectural design decisions, or deep reasoning beyond coding assistance.
Use "local" for shell commands, file reading, grep, simple code questions, debugging, and writing small functions.

Task: ` + prompt

	req := chatRequest{
		Model: a.model,
		Messages: []Message{
			{Role: "user", Content: classifyPrompt},
		},
		Stream:      false,
		Temperature: 0,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return false, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.inferenceURL(), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("inference server unreachable: %w", err)
	}
	defer httpResp.Body.Close()

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"Message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&result); err != nil {
		return false, err
	}

	if len(result.Choices) == 0 {
		return false, nil
	}
	answer := strings.TrimSpace(strings.ToLower(result.Choices[0].Message.Content))
	return strings.HasPrefix(answer, "escalate"), nil
}

// Ping checks whether the inference server is reachable and pre-seeds the
// tool-format detector from the loaded model name when possible.
// For remote providers without a /health endpoint (e.g. Bedrock), the health
// check is skipped and the agent is assumed reachable.
func (a *Agent) Ping(ctx context.Context) error {
	if !a.skipHealthCheck {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/health", nil)
		if err != nil {
			return err
		}
		resp, err := a.client.Do(req)
		if err != nil {
			return fmt.Errorf("inference server unreachable at %s: %w", a.baseURL, err)
		}
		resp.Body.Close()
	}

	// Best-effort: query /v1/models to pre-seed the format detector.
	// Skip for Bedrock native path — it doesn't expose /v1/models and the
	// Converse API doesn't use the stream format detector at all.
	if !a.useBedrockNative {
		a.seedFormatFromModels(ctx)
	}
	return nil
}

// seedFormatFromModels calls GET /v1/models and, if successful, uses the first
// model's ID to pre-seed detectedFormat via GuessFormatFromModel.
func (a *Agent) seedFormatFromModels(ctx context.Context) {
	if a.detectedFormat != ToolFormatUnknown {
		return // already confirmed, no need to guess
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/v1/models", nil)
	if err != nil {
		return
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Data) == 0 {
		return
	}
	if f := GuessFormatFromModel(body.Data[0].ID); f != ToolFormatUnknown {
		a.detectedFormat = f
	}
}

// regionFromBedrockURL extracts the AWS region from a Bedrock runtime URL.
// "https://bedrock-runtime.eu-central-1.amazonaws.com" → "eu-central-1"
func regionFromBedrockURL(rawURL string) string {
	host := rawURL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	// host is e.g. "bedrock-runtime.eu-central-1.amazonaws.com"
	parts := strings.SplitN(host, ".", 3)
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}
