// Package subprocess provides a generic subprocess runner for CLI agent backends.
// Individual agent packages (claude, smolagent) implement ArgBuilder and StreamParser
// to describe their specific CLI arg shapes and stdout protocols; this package owns
// the shared mechanics: subprocess lifecycle, temp file injection, env stripping.
package subprocess

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/scoutme/milk/internal/obs"
)

const subprocessScope = "github.com/scoutme/milk"

// Runner executes a subprocess agent binary and streams its output.
type Runner struct {
	builder    ArgBuilder
	parser     StreamParser
	extraEnv   []string  // KEY=VALUE pairs injected into subprocess env
	debugLog   io.Writer // when non-nil, every raw stdout line is written here
	logContext bool      // when true, log static/dynamic context and prompt via obs.LogPayload
}

// New creates a Runner for the given ArgBuilder and StreamParser.
func New(b ArgBuilder, p StreamParser) *Runner {
	return &Runner{builder: b, parser: p}
}

// WithExtraEnv returns a copy of the Runner with extra KEY=VALUE pairs appended.
func (r *Runner) WithExtraEnv(pairs ...string) *Runner {
	c := *r
	c.extraEnv = append(append([]string{}, r.extraEnv...), pairs...)
	return &c
}

// WithDebugLog returns a copy of the Runner that writes every raw stdout line to w.
func (r *Runner) WithDebugLog(w io.Writer) *Runner {
	c := *r
	c.debugLog = w
	return &c
}

// WithLogContext enables logging of static/dynamic context and prompt at DEBUG level.
func (r *Runner) WithLogContext(v bool) *Runner {
	c := *r
	c.logContext = v
	return &c
}

// Ping delegates to the ArgBuilder's Ping method.
func (r *Runner) Ping() error {
	return r.builder.Ping()
}

// RunFirst runs the first turn of a new session.
// staticContext and dynamicContext are written to separate temp files and passed
// to the ArgBuilder as context file paths. Returns the session ID used (emitted
// by the subprocess when it can, otherwise the generated UUID).
func (r *Runner) RunFirst(ctx context.Context, staticContext, dynamicContext, prompt string, opts ParseOpts, out io.Writer) (string, ParseResult, error) {
	if r.logContext {
		obs.LogPayload(r.builder.Bin()+" [first] static-context", []byte(staticContext))
		obs.LogPayload(r.builder.Bin()+" [first] dynamic-context", []byte(dynamicContext))
		obs.LogPayload(r.builder.Bin()+" [first] prompt", []byte(prompt))
	}
	sessionID := uuid.New().String()
	contextFiles, cleanup := writeContextFiles(staticContext, dynamicContext)
	defer cleanup()

	sessionArgs := r.builder.FirstArgs(sessionID, prompt, contextFiles)
	args := append(r.builder.BaseArgs(), sessionArgs...)

	start := time.Now()
	res, err := r.runPipe(ctx, args, opts, out)
	elapsed := time.Since(start)
	r.recordMetrics(ctx, "first", elapsed, res, err)

	if res.SessionID != "" {
		sessionID = res.SessionID
	}
	return sessionID, res, err
}

// RunResume continues an existing session identified by sessionID.
func (r *Runner) RunResume(ctx context.Context, sessionID, staticContext, dynamicContext, prompt string, opts ParseOpts, out io.Writer) (ParseResult, error) {
	if r.logContext {
		obs.LogPayload(r.builder.Bin()+" [resume] static-context", []byte(staticContext))
		obs.LogPayload(r.builder.Bin()+" [resume] dynamic-context", []byte(dynamicContext))
		obs.LogPayload(r.builder.Bin()+" [resume] prompt", []byte(prompt))
	}
	contextFiles, cleanup := writeContextFiles(staticContext, dynamicContext)
	defer cleanup()

	sessionArgs := r.builder.ResumeArgs(sessionID, prompt, contextFiles)
	args := append(r.builder.BaseArgs(), sessionArgs...)

	start := time.Now()
	res, err := r.runPipe(ctx, args, opts, out)
	elapsed := time.Since(start)
	r.recordMetrics(ctx, "resume", elapsed, res, err)

	return res, err
}

// runPipe starts the subprocess, feeds its stdout to the parser, and returns.
func (r *Runner) runPipe(ctx context.Context, args []string, opts ParseOpts, out io.Writer) (ParseResult, error) {
	if r.debugLog != nil {
		opts.DebugLog = r.debugLog
	}

	cmd := newCmd(ctx, r.builder.Bin(), args, r.builder.EnvStrip(), r.extraEnv)

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return ParseResult{}, fmt.Errorf("opening /dev/null: %w", err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ParseResult{}, err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return ParseResult{}, fmt.Errorf("starting %s: %w", r.builder.Bin(), err)
	}

	res, parseErr := r.parser.Parse(stdout, out, opts)

	if err := cmd.Wait(); err != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			obs.Info("subprocess.stderr", "bin", r.builder.Bin(), "stderr", stderr)
			return res, fmt.Errorf("%s exited with error: %s", r.builder.Bin(), stderr)
		}
		if parseErr != nil {
			return res, parseErr
		}
		if res.IsError {
			return res, fmt.Errorf("%s returned an error response", r.builder.Bin())
		}
		return res, nil
	}
	if parseErr != nil {
		return res, parseErr
	}
	if res.IsError {
		return res, fmt.Errorf("%s returned an error response", r.builder.Bin())
	}
	return res, nil
}

// recordMetrics emits turn counter, latency histogram, error counters, token
// recording, and escalation signal for a completed subprocess run.
func (r *Runner) recordMetrics(ctx context.Context, mode string, elapsed time.Duration, res ParseResult, err error) {
	bin := r.builder.Bin()
	obs.Inc(ctx, subprocessScope, "milk.subprocess.turns",
		attribute.String("bin", bin),
		attribute.String("mode", mode),
	)
	obs.RecordDuration(ctx, subprocessScope, "milk.subprocess.latency_ms", elapsed,
		attribute.String("bin", bin),
		attribute.String("mode", mode),
	)
	if err != nil {
		obs.Inc(ctx, subprocessScope, "milk.subprocess.errors",
			attribute.String("bin", bin),
			attribute.String("kind", "subprocess"),
		)
	} else if res.IsError {
		obs.Inc(ctx, subprocessScope, "milk.subprocess.errors",
			attribute.String("bin", bin),
			attribute.String("kind", "is_error"),
		)
	}
	if res.InputTokens > 0 || res.OutputTokens > 0 {
		obs.RecordTokens(ctx, bin, "escalation", res.InputTokens, res.OutputTokens)
	}
	if res.EscalationReason != "" {
		obs.Inc(ctx, subprocessScope, "milk.router.escalation_signals",
			attribute.String("reason", "explicit_tool_call"),
			attribute.String("bin", bin),
		)
	}
}

// writeContextFiles writes non-empty context strings to temp files and returns
// their paths plus a cleanup function. Callers must call cleanup when done.
func writeContextFiles(staticContext, dynamicContext string) ([]string, func()) {
	var paths []string
	for _, content := range []string{staticContext, dynamicContext} {
		if content == "" {
			continue
		}
		f, err := os.CreateTemp("", "milk-ctx-*.txt")
		if err != nil {
			continue
		}
		if _, err := f.WriteString(content); err != nil {
			f.Close()
			os.Remove(f.Name()) //nolint:errcheck
			continue
		}
		f.Close()
		paths = append(paths, f.Name())
	}
	return paths, func() {
		for _, p := range paths {
			os.Remove(p) //nolint:errcheck
		}
	}
}

// newCmd builds an exec.Cmd for the given binary, applying env stripping and extras.
func newCmd(ctx context.Context, bin string, args []string, stripPrefixes []string, extraEnv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	base := filterEnv(os.Environ(), stripPrefixes...)
	cmd.Env = append(base, extraEnv...)
	return cmd
}

// filterEnv returns env with entries whose key matches any stripPrefixes removed.
func filterEnv(env []string, stripPrefixes ...string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		skip := false
		for _, k := range stripPrefixes {
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
