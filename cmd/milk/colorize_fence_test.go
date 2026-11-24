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
