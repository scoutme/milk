package claude

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/scoutme/milk/internal/obs"
)

// Agent runs the claude CLI as a subprocess.
type Agent struct {
	bin               string                       // path to claude binary, e.g. "claude"
	skipPermissions   bool                         // pass --dangerously-skip-permissions to the CLI
	allowedTools      []string                     // tools pre-approved via --allowedTools
	addDirs           []string                     // extra directories granted via --add-dir
	permissionHandler PermissionHandler            // nil → denyAllHandler
	debugLog          io.Writer                    // when non-nil, every raw NDJSON line is written here
	onToolUse         func(string)                 // called on content_block_start tool_use events
	onToolUseReady    func(string, map[string]any) // called on content_block_stop with full input
	onThinking        func(string)                 // called on thinking_delta tokens
	onPercept         func(string, string)         // called for each <milk:percept:NONCE> tag; args: content, consumerHint
	perceptNonce      string                       // session-specific nonce matching the system-prompt instruction
	agentNames        []string                     // [primaryName, escalationName] for @<name>: consumer-hint parsing
	onNeed            func(string)                 // called for each <milk:need:NONCE> tag; arg: new current-need text
	needNonce         string                       // session-specific nonce matching the system-prompt need instruction
	extraEnv          []string                     // extra KEY=VALUE pairs injected into subprocess env
	logContext        bool                         // when true, log system context and prompt at DEBUG level
}

func New(bin string) *Agent {
	if bin == "" {
		bin = "claude"
	}
	return &Agent{bin: bin}
}

func NewWithOpts(bin string, skipPermissions bool, allowedTools, addDirs []string) *Agent {
	if bin == "" {
		bin = "claude"
	}
	return &Agent{
		bin: bin, skipPermissions: skipPermissions,
		allowedTools: allowedTools, addDirs: addDirs,
	}
}

// WithPermissionHandler returns a copy of the agent with the given handler.
func (a *Agent) WithPermissionHandler(h PermissionHandler) *Agent {
	c := *a
	c.permissionHandler = h
	return &c
}

// WithLogContext enables logging of the system context and prompt passed to the
// claude subprocess at DEBUG level via obs.LogPayload.
func (a *Agent) WithLogContext(v bool) *Agent {
	c := *a
	c.logContext = v
	return &c
}

// WithDebugLog returns a copy of the agent that writes every raw NDJSON line
// from the claude subprocess to w.
func (a *Agent) WithDebugLog(w io.Writer) *Agent {
	c := *a
	c.debugLog = w
	return &c
}

// WithOnToolUse returns a copy of the agent that calls fn whenever Claude
// begins a tool call (content_block_start with type=tool_use).
func (a *Agent) WithOnToolUse(fn func(string)) *Agent {
	c := *a
	c.onToolUse = fn
	return &c
}

// WithOnToolUseReady returns a copy of the agent that calls fn when a tool
// call block is complete (content_block_stop) and the full input is available.
func (a *Agent) WithOnToolUseReady(fn func(string, map[string]any)) *Agent {
	c := *a
	c.onToolUseReady = fn
	return &c
}

// WithOnThinking returns a copy of the agent that calls fn for each
// thinking_delta token emitted by Claude's extended thinking.
func (a *Agent) WithOnThinking(fn func(string)) *Agent {
	c := *a
	c.onThinking = fn
	return &c
}

// SkipPermissions reports whether the agent is running with dangerously_skip_permissions.
func (a *Agent) SkipPermissions() bool { return a.skipPermissions }

// WithSkipPermissions returns a copy of the agent with skipPermissions set to v.
func (a *Agent) WithSkipPermissions(v bool) *Agent {
	c := *a
	c.skipPermissions = v
	return &c
}

// WithOnPercept returns a copy of the agent that calls fn for each
// <milk:percept:NONCE>…</milk:percept:NONCE> tag intercepted in the response stream.
// fn receives the percept body and the consumer-hint name (one of the configured
// agent names, or "" for all agents).
// nonce must be the same value passed to escalation.MemoryInstruction(nonce, ...).
// primaryName and escalationName are the configured agent names used to parse
// @<name>: prefixes in percept bodies.
func (a *Agent) WithOnPercept(fn func(content, consumerHint string), nonce, primaryName, escalationName string) *Agent {
	c := *a
	c.onPercept = fn
	c.perceptNonce = nonce
	c.agentNames = []string{primaryName, escalationName}
	return &c
}

// WithOnNeed returns a copy of the agent that calls fn for each
// <milk:need:NONCE>…</milk:need:NONCE> tag intercepted in the response stream.
// fn receives the new current-need description. nonce must match the value
// passed to escalation.NeedInstruction(nonce).
func (a *Agent) WithOnNeed(fn func(content string), nonce string) *Agent {
	c := *a
	c.onNeed = fn
	c.needNonce = nonce
	return &c
}

// WithExtraAllowedTool returns a copy of the agent with the tool appended to the allowed list.
func (a *Agent) WithExtraAllowedTool(tool string) *Agent {
	c := *a
	c.allowedTools = mergeUniq(a.allowedTools, []string{tool})
	return &c
}

// WithExtraEnv returns a copy of the agent with the given KEY=VALUE pairs appended
// to the subprocess environment. These override any inherited values for the same key.
func (a *Agent) WithExtraEnv(pairs ...string) *Agent {
	c := *a
	c.extraEnv = append(append([]string{}, a.extraEnv...), pairs...)
	return &c
}

// WithExtraDir returns a copy of the agent with the directory appended to the add-dirs list.
func (a *Agent) WithExtraDir(dir string) *Agent {
	c := *a
	c.addDirs = mergeUniq(a.addDirs, []string{dir})
	return &c
}

// Ping checks whether the claude binary is available.
func (a *Agent) Ping() error {
	cmd := exec.Command(a.bin, "--version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude binary %q not available: %w", a.bin, err)
	}
	return nil
}

// RunFirst runs the first turn of a new Claude escalation session.
// systemContext is the formatted local transcript passed via --append-system-prompt-file.
// Returns the session ID emitted by the subprocess and a ParseResult.
func (a *Agent) RunFirst(ctx context.Context, systemContext, prompt string, out io.Writer) (string, ParseResult, error) {
	if a.logContext {
		obs.LogPayload("claude-cli [first] system-context", []byte(systemContext))
		obs.LogPayload("claude-cli [first] prompt", []byte(prompt))
	}
	sessionID := uuid.New().String()
	var args []string
	args = append(args, "--session-id", sessionID)
	if systemContext != "" {
		f, err := writeTempContext(systemContext)
		if err != nil {
			return "", ParseResult{}, err
		}
		defer os.Remove(f)
		args = append(args, "--append-system-prompt-file", f)
	}
	args = append(args, "--", prompt)

	res, err := a.run(ctx, args, out)
	if res.SessionID != "" {
		sessionID = res.SessionID
	}
	return sessionID, res, err
}

// RunResume continues an existing Claude session.
// systemContext is re-injected via --append-system-prompt-file on every resumed turn
// so that instructions (e.g. the percept tag convention) remain active even
// when Claude's conversation compresses its original context.
func (a *Agent) RunResume(ctx context.Context, claudeSessionID, systemContext, prompt string, out io.Writer) (ParseResult, error) {
	if a.logContext {
		obs.LogPayload("claude-cli [resume] system-context", []byte(systemContext))
		obs.LogPayload("claude-cli [resume] prompt", []byte(prompt))
	}
	args := []string{"--resume", claudeSessionID}
	if systemContext != "" {
		f, err := writeTempContext(systemContext)
		if err != nil {
			return ParseResult{}, err
		}
		defer os.Remove(f)
		args = append(args, "--append-system-prompt-file", f)
	}
	args = append(args, "--", prompt)
	return a.run(ctx, args, out)
}

// writeTempContext writes content to a temp file and returns its path.
func writeTempContext(content string) (string, error) {
	f, err := os.CreateTemp("", "milk-ctx-*.txt")
	if err != nil {
		return "", fmt.Errorf("creating context temp file: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("writing context temp file: %w", err)
	}
	f.Close()
	return f.Name(), nil
}

// run builds the full arg list and delegates to runPipe.
func (a *Agent) run(ctx context.Context, args []string, out io.Writer) (ParseResult, error) {
	var prefix []string
	if a.skipPermissions {
		prefix = append(prefix, "--dangerously-skip-permissions")
	} else {
		prefix = append(prefix, "--permission-prompt-tool", "stdio")
	}
	if len(a.allowedTools) > 0 {
		prefix = append(prefix, "--allowedTools", strings.Join(a.allowedTools, ","))
	}
	for _, dir := range a.addDirs {
		prefix = append(prefix, "--add-dir", dir)
	}
	args = append(prefix, args...)
	pipeArgs := append([]string{"--print", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}, args...)
	return a.runPipe(ctx, pipeArgs, out)
}

// runPipe runs the claude CLI and streams structured JSON output.
// When permissions are handled via stdio, stdin is a pipe so control_response
// messages can be sent. When permissions are skipped, stdin is /dev/null to
// avoid Claude's 3-second "no stdin data" warning.
func (a *Agent) runPipe(ctx context.Context, args []string, out io.Writer) (ParseResult, error) {
	cmd := newCmd(ctx, a.bin, args, a.extraEnv)

	var stdinPipe io.WriteCloser
	if a.skipPermissions {
		devNull, err := os.Open(os.DevNull)
		if err != nil {
			return ParseResult{}, fmt.Errorf("opening /dev/null: %w", err)
		}
		defer devNull.Close()
		cmd.Stdin = devNull
		stdinPipe = discardWriteCloser{} // sentinel: writes are no-ops, Close is safe
	} else {
		var err error
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			return ParseResult{}, fmt.Errorf("creating stdin pipe: %w", err)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ParseResult{}, err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return ParseResult{}, fmt.Errorf("starting claude: %w", err)
	}

	res, parseErr := Stream(stdout, out, stdinPipe, StreamOpts{
		OnPermission:   a.permissionHandler,
		OnToolUse:      a.onToolUse,
		OnToolUseReady: a.onToolUseReady,
		OnThinking:     a.onThinking,
		OnPercept:      a.onPercept,
		PerceptNonce:   a.perceptNonce,
		AgentNames:     a.agentNames,
		OnNeed:         a.onNeed,
		NeedNonce:      a.needNonce,
		DebugLog:       a.debugLog,
	})

	// Close stdin after stream ends so Claude can exit cleanly.
	stdinPipe.Close() //nolint:errcheck

	if err := cmd.Wait(); err != nil {
		stderr := filterKnownWarnings(strings.TrimSpace(stderrBuf.String()))
		if stderr != "" {
			return res, fmt.Errorf("claude exited with error: %s", stderr)
		}
		// Only benign warnings on stderr — if the parse succeeded, don't error.
		if parseErr != nil {
			return res, parseErr
		}
		if res.IsError {
			return res, fmt.Errorf("claude returned an error response")
		}
		return res, nil
	}

	if parseErr != nil {
		return res, parseErr
	}
	if res.IsError {
		return res, fmt.Errorf("claude returned an error response")
	}

	return res, nil
}

// discardWriteCloser is a no-op WriteCloser used as a sentinel stdin when
// Claude's stdin is redirected to /dev/null and no control messages are needed.
type discardWriteCloser struct{}

func (discardWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriteCloser) Close() error                { return nil }

// newCmd builds an exec.Cmd for the given binary and args.
// Claude Code env vars from the parent session are stripped so the subprocess
// does not inherit an in-progress session ID or entrypoint context.
// extraEnv KEY=VALUE pairs are appended last so they override inherited values.
func newCmd(ctx context.Context, bin string, args []string, extraEnv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	strip := []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT"}
	// When explicit AWS credentials are injected, strip any vars that could
	// override them. AWS_BEARER_TOKEN_BEDROCK is used by the Anthropic SDK as a
	// higher-priority auth path and will beat AWS_ACCESS_KEY_ID if left in place.
	if len(extraEnv) > 0 {
		strip = append(strip,
			"AWS_PROFILE", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE",
			"AWS_BEARER_TOKEN_BEDROCK",
			"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_MODEL",
		)
	}
	base := filterEnv(os.Environ(), strip...)
	cmd.Env = append(base, extraEnv...)
	return cmd
}

// filterEnv returns os.Environ() with the named keys removed.
func filterEnv(env []string, stripKeys ...string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		skip := false
		for _, k := range stripKeys {
			if strings.HasPrefix(e, k+"=") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, e)
		}
	}
	return out
}

// filterKnownWarnings removes known benign stderr lines that Claude emits even
// on success (e.g. the 3-second stdin-wait warning when no stdin data arrives).
func filterKnownWarnings(stderr string) string {
	var keep []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "no stdin data received") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

func mergeUniq(base, extra []string) []string {
	out := make([]string, len(base), len(base)+len(extra))
	copy(out, base)
	for _, v := range extra {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
