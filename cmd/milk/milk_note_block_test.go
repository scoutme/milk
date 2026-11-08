package main

import (
	"strings"
	"testing"
)

// The [milk]-standalone-line contract (separateMilkNote): an annotation is a
// block of its own — closed off from whatever partial line was streaming
// before it and closed with a newline after — so it never lands inside a
// turn's output and its tag starts a row (which is what dims it at render).

func TestMilkNote_SeparatedFromMidStreamTail(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("partial streamed text") // chunks carry no trailing \n
	m.appendTranscript("[milk] escalated to pro: model classifier")
	got := m.transcript.String()
	want := "partial streamed text\n[milk] escalated to pro: model classifier\n"
	if got != want {
		t.Fatalf("buffer = %q, want %q", got, want)
	}
	// What streams after the note starts on its own line.
	m.appendTranscriptStreamed("more output")
	if !strings.HasSuffix(m.transcript.String(), "model classifier\nmore output") {
		t.Errorf("post-note output did not start fresh: %q", m.transcript.String())
	}
	// The note owns its row: the row that contains it begins with the tag,
	// which is exactly the shape dimMilkAnnotationRows styles.
	for _, row := range strings.Split(got, "\n") {
		if strings.Contains(row, "[milk]") && !strings.HasPrefix(row, "[milk] ") {
			t.Errorf("note not at row start: %q", row)
		}
	}
}

func TestMilkNote_StyledTagAlsoSeparates(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("partial streamed text")
	m.appendTranscript(milkTag() + " ⋯ removed the last queued prompt (1 left) · /queue to review\n")
	want := "partial streamed text\n" + milkTag() + " ⋯ removed the last queued prompt (1 left) · /queue to review\n"
	if got := m.transcript.String(); got != want {
		t.Fatalf("buffer = %q, want %q", got, want)
	}
}

func TestMilkNote_EmptyTranscript_NoLeadingNewline(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("[milk] background agents finished — running a follow-up turn to report their results.\n")
	got := m.transcript.String()
	if strings.HasPrefix(got, "\n") {
		t.Errorf("a first-line note must not open with a blank line: %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("note must close its line: %q", got)
	}
}

func TestMilkNote_LineStartAlready_NoDoubleSeparation(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("done\n") // tail already at a boundary
	m.appendTranscript("[milk] escalated to pro: primary unavailable\n")
	want := "done\n[milk] escalated to pro: primary unavailable\n"
	if got := m.transcript.String(); got != want {
		t.Fatalf("buffer = %q, want %q", got, want)
	}
}

func TestNonMilkContent_MidLineUntouched(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("partial streamed text")
	m.appendTranscript(" — continued") // ordinary content must never gain newlines
	m.appendTranscriptStreamed(" tail")
	want := "partial streamed text — continued tail"
	if got := m.transcript.String(); got != want {
		t.Fatalf("buffer = %q, want %q", got, want)
	}
	// A mid-string mention is not an annotation either: chunk-splitting
	// plain output must stay byte-identical.
	m2 := newTestModelForPanels(t)
	m2.appendTranscript("the tag [milk] appears mid-string")
	if got := m2.transcript.String(); got != "the tag [milk] appears mid-string" {
		t.Errorf("mid-string mention split the line: %q", got)
	}
}

// The production seam: runTurn's route note reaches the buffer as one
// chunkMsg through sendWriter (repl.go runTurn → appendTranscriptStreamed).
// Driven through Update here so the handler path is what's tested.
func TestMilkNote_ViaChunkMsg_SeparatesMidStream(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("streaming in progress") // no trailing newline
	nm, _ := m.Update(chunkMsg{text: "[milk] escalated to mimo-pro-local: auto-sticky escalation\n"})
	got := nm.(model).transcript.String()
	want := "streaming in progress\n[milk] escalated to mimo-pro-local: auto-sticky escalation\n"
	if got != want {
		t.Fatalf("buffer = %q, want %q", got, want)
	}
}
