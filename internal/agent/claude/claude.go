package claude

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"golang.org/x/term"
)

// Agent runs the claude CLI as a subprocess.
type Agent struct {
	bin                string // path to claude binary, e.g. "claude"
	skipPermissions    bool   // pass --dangerously-skip-permissions to the CLI
}

func New(bin string) *Agent {
	if bin == "" {
		bin = "claude"
	}
	return &Agent{bin: bin}
}

func NewWithOpts(bin string, skipPermissions bool) *Agent {
	if bin == "" {
		bin = "claude"
	}
	return &Agent{bin: bin, skipPermissions: skipPermissions}
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

// IsPTYMode returns true when the agent will use the PTY path (stdout is a TTY).
func (a *Agent) IsPTYMode() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// Ping checks whether the claude binary is available.
func (a *Agent) Ping() error {
	cmd := exec.Command(a.bin, "--version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude binary %q not available: %w", a.bin, err)
	}
	return nil
}

// run dispatches to runPTY when stdout is a TTY, otherwise uses the pipe path.
func (a *Agent) run(ctx context.Context, args []string, out io.Writer) (ParseResult, error) {
	if a.skipPermissions {
		args = append([]string{"--dangerously-skip-permissions"}, args...)
	}
	pipeArgs := append([]string{"--print", "--output-format", "stream-json", "--verbose"}, args...)
	if term.IsTerminal(int(os.Stdout.Fd())) {
		return a.runPTY(ctx, args)
	}
	return a.runPipe(ctx, pipeArgs, out)
}

// runPipe is the non-interactive implementation used when stdout is not a TTY.
func (a *Agent) runPipe(ctx context.Context, args []string, out io.Writer) (ParseResult, error) {
	cmd := newCmd(ctx, a.bin, args)

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

// newCmd builds an exec.Cmd for the given binary and args.
func newCmd(ctx context.Context, bin string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
}

// setsidSysProcAttr starts the child in a new session so the PTY slave
// becomes its controlling terminal.
func setsidSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
