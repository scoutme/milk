package eval

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestProgressReporterNonTTY(t *testing.T) {
	var buf bytes.Buffer
	p := newProgressReporter(&buf)

	p.Set("starting agent")
	// Give the goroutine a moment in case it writes concurrently; the write
	// itself happens synchronously inside Set for non-TTY writers.
	time.Sleep(10 * time.Millisecond)

	p.Log("Running scenario: fix-typo")
	p.Logf("warning: %s failed", "judging")
	p.Stop()

	out := buf.String()
	if !strings.Contains(out, "starting agent") {
		t.Errorf("expected phase label in output, got: %q", out)
	}
	if !strings.Contains(out, "Running scenario: fix-typo") {
		t.Errorf("expected log line in output, got: %q", out)
	}
	if !strings.Contains(out, "warning: judging failed") {
		t.Errorf("expected formatted log line in output, got: %q", out)
	}
}

func TestProgressReporterNilSafe(t *testing.T) {
	var p *progressReporter
	// Must not panic on a nil receiver — callers that skip setting up a
	// reporter (e.g. calling runOne directly, outside RunAll) still work.
	p.Set("phase")
	p.Setf("phase %d", 1)
	p.Log("line")
	p.Logf("line %d", 1)
	p.Stop()
}
