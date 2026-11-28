package local

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/obs"
)

// captureMilkLog swaps obs's package logger for a text handler writing into
// buf and returns a restore func (issue #455: asserting what actually lands
// on a milk.log line instead of trusting the plumbing).
func captureMilkLog(buf *bytes.Buffer) func() {
	prev := obs.Logger()
	obs.SetLogger(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { obs.SetLogger(prev) }
}

// TestLogWarnCtx_StampsSessionIDFromContext: plain milk.log WARN lines fired
// during a turn must carry session_id (from obs.ContextWithSessionID, stashed
// at TUI/ACP turn entry), while job= attribution keeps working and lines
// without a session stay clean — never an invented id.
func TestLogWarnCtx_StampsSessionIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	restore := captureMilkLog(&buf)
	t.Cleanup(restore)

	a := &Agent{model: "m", jobID: "job_7"}

	a.logWarnCtx(obs.ContextWithSessionID(context.Background(), "sess-455"), "ctx warn", "k", "v")
	a.logWarnCtx(context.Background(), "ctx warn no session", "k", "v")
	a.logWarn("plain warn", "k", "v")

	lines := map[string]string{}
	for _, line := range strings.Split(buf.String(), "\n") {
		for _, msg := range []string{"ctx warn", "ctx warn no session", "plain warn"} {
			// Match the exact slog msg= token — plain substring matching
			// lets "ctx warn" swallow the "ctx warn no session" line.
			if strings.Contains(line, `msg="`+msg+`"`) || strings.Contains(line, "msg="+msg+" ") {
				lines[msg] = line
			}
		}
	}

	withSess := lines["ctx warn"]
	if !strings.Contains(withSess, "session_id=sess-455") {
		t.Errorf("session-carrying ctx must stamp session_id, got line %q", withSess)
	}
	if !strings.Contains(withSess, "job=job_7") {
		t.Errorf("job attribution must survive logWarnCtx, got line %q", withSess)
	}
	if line := lines["ctx warn no session"]; strings.Contains(line, "session_id") {
		t.Errorf("ctx without a session must not invent one, got %q", line)
	}
	if line := lines["plain warn"]; strings.Contains(line, "session_id") {
		t.Errorf("plain logWarn must stay unchanged, got %q", line)
	}
}

// TestLogWarnSess_ExplicitID: the scanSSE path (no ctx in scope) threads the
// session id through explicitly — same attribution, empty id adds nothing.
func TestLogWarnSess_ExplicitID(t *testing.T) {
	var buf bytes.Buffer
	restore := captureMilkLog(&buf)
	t.Cleanup(restore)

	a := &Agent{model: "m"}
	a.logWarnSess("sess-455", "scanner warn", "k", "v")
	a.logWarnSess("", "scanner warn no session", "k", "v")

	out := buf.String()
	if !strings.Contains(out, "session_id=sess-455") {
		t.Errorf("explicit session id must land on the line:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "scanner warn no session") && strings.Contains(line, "session_id") {
			t.Errorf("empty session id must not add a key, got %q", line)
		}
	}
}
