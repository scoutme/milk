package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
)

func seededModel(t *testing.T, hist []session.Turn) model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	sess.History = hist
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	return newModel(context.Background(), st, nil, dispatchAgents{}, nil)
}

func TestSeedTranscript_FreshSessionStaysEmpty(t *testing.T) {
	m := seededModel(t, nil)
	if m.transcript.Len() != 0 || m.transcriptNoThink.Len() != 0 {
		t.Errorf("fresh session must keep the welcome screen, got %q", m.transcript.String())
	}
}

func TestSeedTranscript_ResumedHistoryShownInBothVariants(t *testing.T) {
	m := seededModel(t, []session.Turn{
		{Role: session.RoleUser, Content: "fix the bug"},
		{Role: session.RoleAssistant, Agent: session.AgentLocal, AgentName: "mimo-local", Content: "done", Thinking: "secret"},
		{Role: session.RoleToolResult, Content: "tool output"},
		{Role: session.RoleAssistant, Agent: session.AgentEscalation, AgentName: "claude", Content: "reviewed"},
	})
	for name, b := range map[string]*strings.Builder{"full": m.transcript, "nothink": m.transcriptNoThink} {
		got := b.String()
		for _, want := range []string{"resumed session", "/export", "fix the bug", "mimo-local:", "done", "claude:", "reviewed"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s transcript missing %q:\n%s", name, want, got)
			}
		}
		for _, bad := range []string{"secret", "tool output"} {
			if strings.Contains(got, bad) {
				t.Errorf("%s transcript must not replay %q", name, bad)
			}
		}
	}
}

func TestSeedTranscript_LongHistoryIsWindowedWithGapMarker(t *testing.T) {
	var hist []session.Turn
	total := replayHeadTurns + replayTailTurns + 25
	for i := 0; i < total; i++ {
		hist = append(hist, session.Turn{Role: session.RoleUser, Content: fmt.Sprintf("msg-%04d", i)})
	}
	hist[replayHeadTurns+3].Content = "MIDDLE-OMITTED"
	hist = append(hist, session.Turn{Role: session.RoleUser, Content: strings.Repeat("x", 3*replayMaxMessageChars)})
	got := seededModel(t, hist).transcript.String()
	if !strings.Contains(got, "msg-0000") || !strings.Contains(got, fmt.Sprintf("msg-%04d", total-1)) {
		t.Error("head and tail must be kept")
	}
	if strings.Contains(got, "MIDDLE-OMITTED") || !strings.Contains(got, "26 earlier turns omitted") {
		t.Errorf("middle must be replaced by a gap marker")
	}
	if len(got) > 2*replayMaxMessageChars+(replayHeadTurns+replayTailTurns)*40 {
		t.Errorf("oversized message not capped: transcript is %d bytes", len(got))
	}
}
