package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
)

// Truncating live writers must complete their fences before the text reaches
// the shared transcript (the #450 contract "live content is completed by its
// writer"): one odd "```" leaves splitTurns unable to split at later blank
// lines, so EVERY subsequent message renders as verbatim raw markdown until
// the next restart re-seeds through balanceFences. That's the 2026-11-24
// colorization report — background completion blocks were prime suspects for
// the cut (backgroundJobResultMaxChars caps result/tail text mid-block).

// poison is a result cut inside a code fence: opener, no closer.
const poison = "result summary:\n\n```go\nfmt.Println(\"cut mid-block\")\n"

// assertCompleted fails when got still carries an odd fence count —
// balanceFences(got) == got is the idempotence check: an already-balanced
// text is returned unchanged, an odd one would get a closer appended. The
// control proves the input itself is actually odd.
func assertCompleted(t *testing.T, name, got, src string) {
	t.Helper()
	if balanceFences(src) == src {
		t.Fatalf("%s: test input must actually be odd-fenced (control): %q", name, src)
	}
	if balanceFences(got) != got {
		t.Errorf("%s: output still carries an unclosed fence — it will flatten every later message:\n%q", name, got)
	}
}

func TestLiveWritersCompleteTheirFences(t *testing.T) {
	job := &local.Job{ID: "bg1", Label: "probe", Result: poison}
	failed := &local.Job{ID: "bg2", Label: "probe", Result: poison, Err: errors.New("probe failed")}
	shell := local.ShellInfo{ID: "s1", Command: "build", Finished: true}

	cases := []struct {
		name string
		got  string
	}{
		{"job done block", renderBackgroundJobDoneBlock(job)},
		{"job done block (failed, partial)", renderBackgroundJobDoneBlock(failed)},
		{"shell done block", renderShellDoneBlock(shell, poison)},
		{"shell notices", renderShellNotices([]local.ShellEvent{{Info: shell, Tail: poison}})},
		{"shell notices (long cut)", renderShellNotices([]local.ShellEvent{{Info: shell, Tail: strings.Repeat("x", 13000) + "\n" + poison}})},
		{"history events", historyUserContent(poison+"\n", "user text")},
		{"workflow notice", workflowCompletionNotice("wf", poison, nil, nil)},
	}
	for _, tc := range cases {
		assertCompleted(t, tc.name, tc.got, poison)
	}

	// The truncation itself must not re-open the class it cuts: a tail whose
	// fence lands BEYOND the cap is cut away entirely (even), and one cut
	// mid-block comes out balanced — same balanceFences, exercised through
	// the long path above.
	long := strings.Repeat("y", 12500) + "\n```go\nz := 1"
	assertCompleted(t, "shell done block (long cut)", renderShellDoneBlock(shell, long), long)
}
