package main

import (
	"context"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/testutil"
)

// newFoldDefaultModel mirrors newTestModelForPanels but seeds the config
// field under test (issue #278).
func newFoldDefaultModel(t *testing.T, foldDefault string) model {
	t.Helper()
	testutil.SandboxHome(t)
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{
		sess:     sess,
		cwd:      "/repo",
		notifier: oversight.Noop{},
		cfg:      config.Config{ThinkFoldDefault: foldDefault},
	}
	return newModel(context.Background(), st, nil, dispatchAgents{}, nil)
}

// "expanded" seeds the session's fold status: blocks arrive open, the
// turn-boundary tidy is opted out from the start, a stream close keeps them
// open (the seeded status is a standing preference, like /think expand),
// and a reseed restores the configured status instead of hardcoded folded.
func TestThinkFoldDefault_ExpandedSeedsSession(t *testing.T) {
	m := newFoldDefaultModel(t, "expanded")
	if !m.thinkFoldOpen {
		t.Fatal("expanded seed not applied to thinkFoldOpen")
	}

	m.appendThinkingStreamed("first thought\n")
	if len(m.thinkBlocks) != 1 {
		t.Fatalf("expected one block, got %d", len(m.thinkBlocks))
	}
	if !m.thinkBlocks[0].expanded {
		t.Errorf("first block must arrive open, got %+v", m.thinkBlocks[0])
	}

	// The streaming close keeps the seeded-open block open.
	m.closeOpenThinkBlock()
	if !m.thinkBlocks[0].expanded {
		t.Errorf("seeded-open block must stay open at close, got %+v", m.thinkBlocks[0])
	}

	// The turn-boundary tidy is opted out from the start.
	m.appendTranscript("answer\n")
	m.appendThinkingStreamed("second thought\n")
	m.collapseThinkBlocksBefore(len(m.thinkBlocks))
	if !m.thinkBlocks[0].expanded || !m.thinkBlocks[1].expanded {
		t.Errorf("tidy must not run under the expanded seed: %+v", m.thinkBlocks)
	}

	// A reseed (session switch) restores the configured status — not false.
	m.reseedTranscriptFromHistory()
	if !m.thinkFoldOpen {
		t.Error("reseed must restore the configured starting status")
	}
	m.appendThinkingStreamed("after reseed\n")
	if !m.thinkBlocks[0].expanded {
		t.Errorf("block after reseed must arrive open, got %+v", m.thinkBlocks[0])
	}
}

// Unset (and any non-"expanded" value) is exactly today's behavior: folded
// seeds, folded first block, tidy runs, reseed folded.
func TestThinkFoldDefault_FoldedByDefault(t *testing.T) {
	for _, v := range []string{"", "folded", "garbage"} {
		t.Run(v, func(t *testing.T) {
			m := newFoldDefaultModel(t, v)
			if m.thinkFoldOpen {
				t.Fatalf("think_fold_default=%q must seed folded", v)
			}
			m.appendThinkingStreamed("thought\n")
			if len(m.thinkBlocks) != 1 || m.thinkBlocks[0].expanded {
				t.Errorf("first block must arrive folded, got %+v", m.thinkBlocks)
			}
			// The tidy runs as ever for the folded default.
			m.appendTranscript("a\n")
			m.appendThinkingStreamed("prev turn\n")
			m.thinkBlocks[0].expanded = true // pretend it was open
			m.collapseThinkBlocksBefore(len(m.thinkBlocks))
			if m.thinkBlocks[0].expanded {
				t.Error("folded default must keep the turn tidy active")
			}
			m.reseedTranscriptFromHistory()
			if m.thinkFoldOpen {
				t.Error("reseed must stay folded for the folded default")
			}
		})
	}
}

// Per-session actions still win over the seed for the rest of the session:
// folding one block flips the status, so the next block arrives folded even
// under the expanded seed (the knob is a starting status only).
func TestThinkFoldDefault_UserActionWinsOverSeed(t *testing.T) {
	m := newFoldDefaultModel(t, "expanded")
	m.appendThinkingStreamed("one\n")
	m.toggleThinkBlock(0) // fold + pin → the status is now folded
	if m.thinkFoldOpen {
		t.Fatal("folding must flip the status")
	}
	m.appendTranscript("a\n")
	m.appendThinkingStreamed("two\n")
	if m.thinkBlocks[1].expanded {
		t.Errorf("user fold must win over the seed for later blocks, got %+v", m.thinkBlocks[1])
	}
}

// Guard the strings-only plumbing: transcriptDisplay stays balanced with a
// seeded-open block (the fixture's render path).
func TestThinkFoldDefault_RenderBalanced(t *testing.T) {
	m := newFoldDefaultModel(t, "expanded")
	m.appendThinkingStreamed("line one\nline two\n")
	d := m.transcriptDisplay()
	if !strings.Contains(d, "thinking") {
		t.Errorf("block header missing from render: %q", d)
	}
	if strings.Contains(d, "▸") || !strings.Contains(d, "▾") {
		t.Errorf("seeded-open block must render expanded (▾), got: %q", d)
	}
}
