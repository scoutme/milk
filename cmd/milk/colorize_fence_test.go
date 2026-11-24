package main

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// The three parsers that must agree on where a fenced block starts — the
// splitTurns walker, findOpenFence, and (through it) the colorizers — plus
// the seed's fence balancer. Disagreement was the bug behind raw-markdown
// transcripts: splitTurns ignored a fence behind an agent tag's SGR reset
// ("mimo-local: ```\n…"), split mid-block, left the next segment with
// inverted fence parity, and the "incomplete block renders verbatim" rule
// then painted the rest of the transcript raw.
func TestFenceOpensAtPredicate(t *testing.T) {
	cases := []struct {
		name string
		text string
		i    int
		want bool
	}{
		{"text start", "```go", 0, true},
		{"line start", "prose\n```", 6, true},
		{"after SGR reset", "\x1b[0m```", 4, true},
		{"after full-color SGR", "\x1b[38;5;231m```", 11, true},
		{"mid-line plain", "say ``` now", 4, false},
		{"after a space", "word ```", 5, false},
		{"after a letter", "word```", 4, false},
	}
	for _, c := range cases {
		if got := fenceOpensAt(c.text, c.i); got != c.want {
			t.Errorf("%s: fenceOpensAt(%q, %d) = %v, want %v", c.name, c.text, c.i, got, c.want)
		}
	}
}

// seedWithFencedReply is the exact shape that broke: a reply whose first
// content is a quoted shell block behind the agent tag, containing a blank
// line inside the block, followed by prose markdown.
func seedWithFencedReply() string {
	var b strings.Builder
	st := &interactiveState{}
	writeSeedTurn(&b, st, session.Turn{
		Role:      session.RoleAssistant,
		AgentName: "mimo-local",
		Content:   "```\nOn branch feat/acp-steer\n\nnothing to commit, working tree clean\n```\n\nEverything is committed and pushed: **PR #446** is green.",
	})
	return b.String()
}

func TestSplitTurnsKeepsTagFenceBlockWhole(t *testing.T) {
	oldTTY := isTTY
	isTTY = true // the agent tag only carries its SGR on a TTY (paintLines)
	t.Cleanup(func() { isTTY = oldTTY })
	seed := seedWithFencedReply()
	segs := splitTurns(seed)
	if len(segs) != 2 {
		t.Fatalf("want 2 segments (tag+block, prose tail), got %d: %q", len(segs), segs)
	}
	if !strings.Contains(segs[0], "nothing to commit, working tree clean\n```") {
		t.Errorf("first segment must hold the whole fenced block, got %q", segs[0])
	}
	if !strings.Contains(segs[1], "Everything is committed") {
		t.Errorf("second segment must be the prose tail, got %q", segs[1])
	}
}

// TestColorizeTagFenceReplyEndToEnd pins the user-visible contract: the seed
// renders the block verbatim (syntax-highlight path) and the prose after it
// with balanced-mode styling — no raw ** left anywhere outside the block.
func TestColorizeTagFenceReplyEndToEnd(t *testing.T) {
	oldTTY := isTTY
	isTTY = true // production renders the tag with SGR; match that shape
	t.Cleanup(func() { isTTY = oldTTY })
	seed := seedWithFencedReply()
	var out strings.Builder
	for _, seg := range splitTurns(seed) {
		out.WriteString(colorizeSingleWidth(seg, ColorizeBalanced, 120))
	}
	got := out.String()
	if !strings.Contains(got, "On branch feat/acp-steer") {
		t.Errorf("fenced block body must survive verbatim, got %q", got)
	}
	if strings.Contains(got, "**PR #446**") {
		t.Errorf("prose after the block rendered raw — the incomplete-block regression is back: %q", got)
	}
	if !strings.Contains(got, "\x1b[1mPR #446\x1b[0m") {
		t.Errorf("prose must carry the balanced bold style, got %q", got)
	}
	// The bold pair must actually be styled (markers consumed).
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "Everything is committed") && strings.Contains(l, "**") {
			t.Errorf("prose line still carries raw markers: %q", l)
		}
	}
}

// TestSeedTurnBalanceFences: the replay cap can end a message mid-block; the
// seed must close what the cap left open so the turn (and every turn after
// it through splitTurns parity) still colorizes.
func TestSeedTurnBalanceFences(t *testing.T) {
	got := balanceFences("```go\nfmt.Println(1)")
	if strings.Count(got, "```")%2 != 0 {
		t.Errorf("odd fence count after balancing: %q", got)
	}
	if !strings.HasSuffix(got, "```\n") {
		t.Errorf("balancer must append a closing fence, got %q", got)
	}
	if balanceFences("no fences here **bold**") != "no fences here **bold**" {
		t.Errorf("balanced input must pass through unchanged")
	}
	if balanceFences("prose with ``` fence word") != "prose with ``` fence word" {
		t.Errorf("mid-line non-opener fences must not count (they never open a block)")
	}
}

// The turn label sits in front of the response's FIRST line, so every block
// rule (heading, bullet, quote) must run on the content after it — otherwise
// a message opening with "## Title" renders as common text (the user's
// "titles are white" report).
func TestCutTurnLabelPredicate(t *testing.T) {
	tag := "\x1b[0;1m\x1b[38;2;168;181;143mmimo-local:\x1b[0m ## Open issues"
	if label, body, ok := cutTurnLabel(tag); !ok || label == "" || body != " ## Open issues" {
		t.Errorf("classic tag row: ok=%v label=%q body=%q", ok, label, body)
	}
	for _, plain := range []string{
		"plain prose, no SGR",
		"## Heading without a label",
		"\x1b[2mNote: carried dim without its own reset",
		"mimo-local: heading without SGR (off-mode row)",
	} {
		if _, _, ok := cutTurnLabel(plain); ok {
			t.Errorf("must not match %q", plain)
		}
	}
	// A tag name with a space is prose, not a turn label.
	if _, _, ok := cutTurnLabel("\x1b[1mmy note:\x1b[0m ## x"); ok {
		t.Error("space in the name must not match")
	}
}

// End-to-end through the seed writer: a message whose FIRST line is a
// heading gets the heading style right after the tag, markers preserved —
// the same styling the heading gets on any later line.
func TestTagLineHeadingStyles(t *testing.T) {
	oldTTY := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = oldTTY })

	var b strings.Builder
	writeSeedTurn(&b, &interactiveState{sess: &session.Session{}}, session.Turn{
		Role:      session.RoleAssistant,
		AgentName: "mimo-local",
		Content:   "## Open issues on scoutme/milk\n\nrest **bold**",
	})
	first := strings.Split(colorizeSingleWidth(b.String(), ColorizeBalanced, 120), "\n")[0]
	want := "\x1b[1m\x1b[36m ## Open issues on scoutme/milk\x1b[0m"
	if !strings.Contains(first, want) {
		t.Errorf("tag-line heading must carry the heading style after the label:\n%s", first)
	}
	if !strings.Contains(first, "mimo-local:") {
		t.Errorf("label lost: %q", first)
	}
}
