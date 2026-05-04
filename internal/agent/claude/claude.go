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
	bin string // path to claude binary, e.g. "claude"
}

func New(bin string) *Agent {
	if bin == "" {
		bin = "claude"
	}
	return &Agent{bin: bin}
}

// RunFirst runs the first turn of a new Claude escalation session.
// systemContext is the formatted local transcript passed via --append-system-prompt.
// Returns the session ID emitted by the subprocess and a ParseResult.
func (a *Agent) RunFirst(ctx context.Context, systemContext, prompt string, out io.Writer) (string, ParseResult, error) {
	sessionID := uuid.New().String()
	args := a.baseArgs()
	args = append(args, "--session-id", sessionID)
	if systemContext != "" {
		args = append(args, "--append-system-prompt", systemContext)
	}
	args = append(args, prompt)

	res, err := a.run(ctx, args, out)
	// Use session_id from the stream if available (Claude may canonicalize it)
	if res.SessionID != "" {
		sessionID = res.SessionID
	}
	return sessionID, res, err
}

// RunResume continues an existing Claude session.
func (a *Agent) RunResume(ctx context.Context, claudeSessionID, prompt string, out io.Writer) (ParseResult, error) {
	args := a.baseArgs()
	args = append(args, "--resume", claudeSessionID)
	args = append(args, prompt)
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

func (a *Agent) baseArgs() []string {
	return []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
	}
}

func (a *Agent) run(ctx context.Context, args []string, out io.Writer) (ParseResult, error) {
	cmd := exec.CommandContext(ctx, a.bin, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ParseResult{}, err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return ParseResult{}, fmt.Errorf("starting claude: %w", err)
	}

	res, parseErr := Stream(stdout, out)

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
