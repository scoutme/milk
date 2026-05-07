package claude

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/google/uuid"
)

// Agent runs the claude CLI as a subprocess.
type Agent struct {
	bin             string   // path to claude binary, e.g. "claude"
	skipPermissions bool     // pass --dangerously-skip-permissions to the CLI
	allowedTools    []string // tools pre-approved via --allowedTools
	addDirs         []string // extra directories granted via --add-dir
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
	return &Agent{bin: bin, skipPermissions: skipPermissions, allowedTools: allowedTools, addDirs: addDirs}
}

// WithExtraTools returns a copy of the agent with additional tools appended to
// the allowed list. Used by the reactive permission retry path.
func (a *Agent) WithExtraTools(extra ...string) *Agent {
	return &Agent{
		bin: a.bin, skipPermissions: a.skipPermissions, addDirs: a.addDirs,
		allowedTools: mergeUniq(a.allowedTools, extra),
	}
}

// WithExtraDirs returns a copy of the agent with additional directories added.
// Used by the reactive directory-restriction retry path.
func (a *Agent) WithExtraDirs(extra ...string) *Agent {
	return &Agent{
		bin: a.bin, skipPermissions: a.skipPermissions, allowedTools: a.allowedTools,
		addDirs: mergeUniq(a.addDirs, extra),
	}
}

func mergeUniq(base, extra []string) []string {
	out := make([]string, len(base), len(base)+len(extra))
	copy(out, base)
	for _, v := range extra {
		found := false
		for _, e := range out {
			if e == v {
				found = true
				break
			}
		}
		if !found {
			out = append(out, v)
		}
	}
	return out
}

// RunFirst runs the first turn of a new Claude escalation session.
// systemContext is the formatted local transcript passed via --append-system-prompt.
// Returns the session ID emitted by the subprocess and a ParseResult.
func (a *Agent) RunFirst(ctx context.Context, systemContext, prompt string, out io.Writer) (string, ParseResult, error) {
	sessionID := uuid.New().String()
	var args []string
	args = append(args, "--session-id", sessionID)
	if systemContext != "" {
		args = append(args, "--append-system-prompt", systemContext)
	}
	args = append(args, prompt)

	res, err := a.run(ctx, args, out)
	if res.SessionID != "" {
		sessionID = res.SessionID
	}
	return sessionID, res, err
}

// RunResume continues an existing Claude session.
func (a *Agent) RunResume(ctx context.Context, claudeSessionID, prompt string, out io.Writer) (ParseResult, error) {
	args := []string{"--resume", claudeSessionID, prompt}
	return a.run(ctx, args, out)
}

// Ping checks whether the claude binary is available.
func (a *Agent) Ping() error {
	cmd := exec.Command(a.bin, "--version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude binary %q not available: %w", a.bin, err)
	}
	return nil
}

// run builds the pipe args and delegates to runPipe.
func (a *Agent) run(ctx context.Context, args []string, out io.Writer) (ParseResult, error) {
	var prefix []string
	if a.skipPermissions {
		prefix = append(prefix, "--dangerously-skip-permissions")
	}
	if len(a.allowedTools) > 0 {
		prefix = append(prefix, "--allowedTools", strings.Join(a.allowedTools, ","))
	}
	for _, dir := range a.addDirs {
		prefix = append(prefix, "--add-dir", dir)
	}
	args = append(prefix, args...)
	pipeArgs := append([]string{"--print", "--output-format", "stream-json", "--verbose"}, args...)
	return a.runPipe(ctx, pipeArgs, out)
}

// runPipe runs the claude CLI and streams structured JSON output.
func (a *Agent) runPipe(ctx context.Context, args []string, out io.Writer) (ParseResult, error) {
	cmd := newCmd(ctx, a.bin, args)
	cmd.Stdin = nil // disconnect TTY stdin so Claude doesn't launch its interactive TUI

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ParseResult{}, err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return ParseResult{}, fmt.Errorf("starting claude: %w", err)
	}

	res, parseErr := Stream(stdout, out, a.allowedTools)

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
