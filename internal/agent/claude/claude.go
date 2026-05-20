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
)

// Agent runs the claude CLI as a subprocess.
type Agent struct {
	bin                   string                       // path to claude binary, e.g. "claude"
	skipPermissions       bool                         // pass --dangerously-skip-permissions to the CLI
	allowedTools          []string                     // tools pre-approved via --allowedTools
	addDirs               []string                     // extra directories granted via --add-dir
	permissionPhrases     []string                     // phrases indicating tool permission denial
	dirRestrictionPhrases []string                     // phrases indicating directory restriction
	permissionHandler     PermissionHandler            // nil → denyAllHandler
	debugLog              io.Writer                    // when non-nil, every raw NDJSON line is written here
	onToolUse             func(string)                 // called on content_block_start tool_use events
	onToolUseReady        func(string, map[string]any) // called on content_block_stop with full input
	onThinking            func(string)                 // called on thinking_delta tokens
	onPercept             func(string, string)         // called for each <milk:percept:NONCE> tag; args: content, consumerHint
	perceptNonce          string                       // session-specific nonce matching the system-prompt instruction
}

func New(bin string) *Agent {
	if bin == "" {
		bin = "claude"
	}
	return &Agent{bin: bin}
}

func NewWithOpts(bin string, skipPermissions bool, allowedTools, addDirs, permissionPhrases, dirRestrictionPhrases []string) *Agent {
	if bin == "" {
		bin = "claude"
	}
	return &Agent{
		bin: bin, skipPermissions: skipPermissions,
		allowedTools: allowedTools, addDirs: addDirs,
		permissionPhrases: permissionPhrases, dirRestrictionPhrases: dirRestrictionPhrases,
	}
}

// WithPermissionHandler returns a copy of the agent with the given handler.
func (a *Agent) WithPermissionHandler(h PermissionHandler) *Agent {
	c := *a
	c.permissionHandler = h
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

// WithOnPercept returns a copy of the agent that calls fn for each
// <milk:percept:NONCE>…</milk:percept:NONCE> tag intercepted in the response stream.
// fn receives the percept body and a consumerHint ("local", "claude", or "").
// nonce must be the same value passed to escalation.MemoryInstruction(nonce).
func (a *Agent) WithOnPercept(fn func(content, consumerHint string), nonce string) *Agent {
	c := *a
	c.onPercept = fn
	c.perceptNonce = nonce
	return &c
}

// WithExtraAllowedTool returns a copy of the agent with the tool appended to the allowed list.
func (a *Agent) WithExtraAllowedTool(tool string) *Agent {
	c := *a
	c.allowedTools = mergeUniq(a.allowedTools, []string{tool})
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
	args = append(args, prompt)

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
	args := []string{"--resume", claudeSessionID}
	if systemContext != "" {
		f, err := writeTempContext(systemContext)
		if err != nil {
			return ParseResult{}, err
		}
		defer os.Remove(f)
		args = append(args, "--append-system-prompt-file", f)
	}
	args = append(args, prompt)
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
// Claude's stdin is connected to a pipe so control_response messages can be sent.
func (a *Agent) runPipe(ctx context.Context, args []string, out io.Writer) (ParseResult, error) {
	cmd := newCmd(ctx, a.bin, args)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return ParseResult{}, fmt.Errorf("creating stdin pipe: %w", err)
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
		PermissionPhrases:     a.permissionPhrases,
		DirRestrictionPhrases: a.dirRestrictionPhrases,
		AllowedTools:          a.allowedTools,
		OnPermission:          a.permissionHandler,
		OnToolUse:             a.onToolUse,
		OnToolUseReady:        a.onToolUseReady,
		OnThinking:            a.onThinking,
		OnPercept:             a.onPercept,
		PerceptNonce:          a.perceptNonce,
		DebugLog:              a.debugLog,
	})

	// Close stdin after stream ends so Claude can exit cleanly.
	stdinPipe.Close() //nolint:errcheck

	if err := cmd.Wait(); err != nil {
		if stderrBuf.Len() > 0 {
			return res, fmt.Errorf("claude exited with error: %s", strings.TrimSpace(stderrBuf.String()))
		}
		return res, fmt.Errorf("claude exited: %w", err)
	}

	if parseErr != nil {
		return res, parseErr
	}
	if res.IsError {
		return res, fmt.Errorf("claude returned an error response")
	}

	return res, nil
}

// newCmd builds an exec.Cmd for the given binary and args.
func newCmd(ctx context.Context, bin string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
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
