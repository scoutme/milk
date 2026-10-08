package main

import (
	"strings"
	"testing"
)

func TestRetractStreamed_ReplacesLastExactOccurrence(t *testing.T) {
	m := newTestModelForPanels(t)
	const raw = "<tool_call>=bash><parameter=command>ls</parameter></function></tool_call>"
	for _, b := range []*strings.Builder{m.transcript, m.transcriptNoThink} {
		b.WriteString("earlier\n" + raw + "\n")
	}
	m.retractStreamed(raw, "[clean summary]")
	for name, b := range map[string]*strings.Builder{"full": m.transcript, "nothink": m.transcriptNoThink} {
		got := b.String()
		if strings.Contains(got, "<tool_call>") || !strings.Contains(got, "[clean summary]") || !strings.HasPrefix(got, "earlier\n") {
			t.Errorf("%s transcript = %q", name, got)
		}
		if strings.Contains(got, retractFallbackNote) {
			t.Errorf("%s: exact match must not add the fallback note", name)
		}
	}
}

func TestRetractStreamed_FallsBackToNoteWhenNotFound(t *testing.T) {
	m := newTestModelForPanels(t)
	m.transcript.WriteString("something else")
	m.transcriptNoThink.WriteString("something else")
	m.retractStreamed("<tool_call>x", "[clean summary]")
	got := m.transcript.String()
	if !strings.HasPrefix(got, "something else\n[clean summary]\n") || !strings.Contains(got, retractFallbackNote) {
		t.Errorf("transcript = %q", got)
	}
}
