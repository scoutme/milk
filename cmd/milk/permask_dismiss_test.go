package main

// Tests for the honest-withdrawal rules on permission/question prompts
// (reported: "(answered elsewhere)" showed up even when the user answered
// the prompt through the normal TUI input):
//
//  1. a prompt whose ask was already resolved before it was ever shown is
//     never flashed at all (no prompt, no note);
//  2. an answer that lands in the same instant as the cancellation wins the
//     tie — a real TUI answer never produces the note;
//  3. an unanswered withdrawal says WHO resolved the ask (remote oversight,
//     the timed answer, cancellation), recorded on the ask's ctx by
//     askPermission/askUserQuestion, falling back to "(answered elsewhere)".

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/scoutme/milk/internal/oversight"
)

// instantAnswerRemote answers every ask immediately — the remote surface
// winning the race.
type instantAnswerRemote struct{ oversight.Noop }

func (instantAnswerRemote) AskPermission(context.Context, oversight.PermRequest) (oversight.PermDecision, bool) {
	return oversight.PermAllow, true
}

func (instantAnswerRemote) AskInput(context.Context, oversight.InputRequest) (string, bool) {
	return "choice", true
}

// instantSilentRemote never replies: with remoteTimeoutResolves (foreground)
// its timeout action resolves the ask.
type instantSilentRemote struct{ oversight.Noop }

func (instantSilentRemote) AskPermission(context.Context, oversight.PermRequest) (oversight.PermDecision, bool) {
	return oversight.PermTimeout, false
}

// captureReader is a tuiInputReader that records every message it sends so a
// test can inspect prompts and dismissals (mutex-guarded: the reader runs on
// its own goroutine while the test polls).
type safeMsgs struct {
	sync.Mutex
	msgs []tea.Msg
}

func (s *safeMsgs) add(m tea.Msg) {
	s.Lock()
	s.msgs = append(s.msgs, m)
	s.Unlock()
}

func (s *safeMsgs) all() []tea.Msg {
	s.Lock()
	defer s.Unlock()
	out := make([]tea.Msg, len(s.msgs))
	copy(out, s.msgs)
	return out
}

func captureReader(s *safeMsgs) *tuiInputReader {
	return &tuiInputReader{send: func(m tea.Msg) { s.add(m) }}
}

func waitPrompt(t *testing.T, s *safeMsgs) permRequestMsg {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range s.all() {
			if req, ok := m.(permRequestMsg); ok {
				return req
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no permRequestMsg sent")
	return permRequestMsg{}
}

// TestReadLineCtx_PrematureCtx_NoPromptShown: when the ask's ctx is already
// done before the surface is asked, nothing may be shown — a flash prompt
// with an immediate dismissal is exactly the confusing "(answered elsewhere)"
// on a prompt the user never had a chance to answer.
func TestReadLineCtx_PrematureCtx_NoPromptShown(t *testing.T) {
	var s safeMsgs
	ir := captureReader(&s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := ir.readLineCtx(ctx, "[milk] Allow? [Y/n] ", "", "", time.Time{}); err == nil {
		t.Fatal("expected ctx error for an already-resolved ask")
	}
	if got := len(s.all()); got != 0 {
		t.Fatalf("no prompt may be shown for an already-resolved ask, got %d message(s)", got)
	}
}

// TestReadLineCtx_AnswerWinsTieOverCancel: a TUI answer written at the same
// instant as the cancellation must be honored — readLineCtx returns it and
// sends no permDismissMsg, so the transcript never claims the prompt was
// "answered elsewhere" when the user simply answered it.
func TestReadLineCtx_AnswerWinsTieOverCancel(t *testing.T) {
	var s safeMsgs
	ir := captureReader(&s)

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		ans string
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		ans, err := ir.readLineCtx(ctx, "[milk] Allow? [Y/n] ", "", "", time.Time{})
		done <- outcome{ans, err}
	}()

	req := waitPrompt(t, &s)
	req.respCh <- "y" // the user's regular TUI answer
	cancel()          // the racing source resolves the ask at the same instant

	res := <-done
	if res.err != nil {
		t.Fatalf("the answer must win the tie, got err=%v", res.err)
	}
	if res.ans != "y" {
		t.Fatalf("answer = %q, want %q", res.ans, "y")
	}
	for _, m := range s.all() {
		if d, ok := m.(permDismissMsg); ok {
			t.Fatalf("a real answer must not produce a withdrawal, got dismiss note %q", d.note)
		}
	}
}

// TestReadLineCtx_DismissCarriesReasonNote: an unanswered withdrawal carries
// the resolving source's note from the ask layer's ctx onto the dismiss msg.
func TestReadLineCtx_DismissCarriesReasonNote(t *testing.T) {
	var s safeMsgs
	ir := captureReader(&s)

	ctx, cancel := context.WithCancel(context.Background())
	note := "remote oversight timed out — deny applied"
	ctx = context.WithValue(ctx, permDismissNoteKey{}, &note)

	done := make(chan error, 1)
	go func() {
		_, err := ir.readLineCtx(ctx, "[milk] Allow? [Y/n] ", "", "", time.Time{})
		done <- err
	}()

	waitPrompt(t, &s) // prompt shown
	cancel()

	if err := <-done; err == nil {
		t.Fatal("expected ctx error after the cancellation")
	}
	var dismiss *permDismissMsg
	for _, m := range s.all() {
		if d, ok := m.(permDismissMsg); ok {
			dismiss = &d
		}
	}
	if dismiss == nil {
		t.Fatal("no permDismissMsg sent — the prompt would stay parked in the TUI")
	}
	if dismiss.note != note {
		t.Fatalf("dismiss note = %q, want %q", dismiss.note, note)
	}
}

// TestAskPermission_DismissNoteSaysWhoAnswered: every resolution path records
// its note on the direct surface's ctx before cancelling, so the withdrawn
// prompt can say what actually happened.
func TestAskPermission_DismissNoteSaysWhoAnswered(t *testing.T) {
	cases := []struct {
		name     string
		n        oversight.Notifier
		p        askPolicy
		allow    bool
		src      permSource
		wantNote string
	}{
		{
			name:     "remote answered",
			n:        instantAnswerRemote{},
			p:        foregroundAskPolicy,
			allow:    true,
			src:      permSrcRemote,
			wantNote: "answered via remote oversight",
		},
		{
			name:     "remote timed out",
			n:        instantSilentRemote{},
			p:        foregroundAskPolicy,
			allow:    false,
			src:      permSrcRemoteTimeout,
			wantNote: "remote oversight timed out — deny applied",
		},
		{
			name:     "timed answer",
			n:        nil,
			p:        askPolicy{deadline: 20 * time.Millisecond, def: false},
			allow:    false,
			src:      permSrcDefault,
			wantNote: "timed answer — deny applied",
		},
		{
			name:     "parent ctx cancelled",
			n:        nil,
			p:        foregroundAskPolicy,
			allow:    false,
			src:      permSrcDefault,
			wantNote: "", // no note can be recorded safely — generic fallback
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The direct surface waits exactly like the TUI prompt does: it is
			// withdrawn when the ask's ctx ends, then reads the recorded note.
			directDone := make(chan string, 1)
			direct := func(ctx context.Context) bool {
				<-ctx.Done()
				directDone <- dismissNoteFrom(ctx)
				return false
			}

			ctx := context.Background()
			if tc.name == "parent ctx cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				go func() {
					time.Sleep(20 * time.Millisecond)
					cancel()
				}()
			}

			allow, src := askPermission(ctx, tc.n, oversight.PermRequest{ToolName: "bash"}, direct, tc.p)
			if allow != tc.allow || src != tc.src {
				t.Fatalf("got (%v, %s), want (%v, %s)", allow, src, tc.allow, tc.src)
			}
			if note := <-directDone; note != tc.wantNote {
				t.Fatalf("dismiss note = %q, want %q", note, tc.wantNote)
			}
		})
	}
}

// TestAskUserQuestion_DismissNoteForQuestions: question withdrawals use the
// question wording — silence is "no answer in time", never a default.
func TestAskUserQuestion_DismissNoteForQuestions(t *testing.T) {
	directDone := make(chan string, 1)
	direct := func(ctx context.Context) (string, bool) {
		<-ctx.Done()
		directDone <- dismissNoteFrom(ctx)
		return "", false
	}
	answer, ok, _ := askUserQuestion(context.Background(), instantAnswerRemote{},
		oversight.InputRequest{Question: "which one?"}, direct, foregroundAskPolicy)
	if !ok || answer != "choice" {
		t.Fatalf("remote should answer the question: got (%q, %v)", answer, ok)
	}
	if note := <-directDone; note != "answered via remote oversight" {
		t.Fatalf("dismiss note = %q, want %q", note, "answered via remote oversight")
	}
}

// TestHandlePermDismiss_RendersReasonNote: the TUI prints the carried note
// instead of the generic wording, and an empty note keeps the fallback.
func TestHandlePermDismiss_RendersReasonNote(t *testing.T) {
	resp := make(chan string, 1)
	m := model{st: &interactiveState{}, transcript: &strings.Builder{}, ta: textarea.New()}
	m.pendingPerm = &permRequestMsg{prompt: "[milk] Allow? [Y/n] ", respCh: resp}

	nm, _ := m.handlePermDismiss(permDismissMsg{respCh: resp, note: "remote oversight timed out — deny applied"})
	mm := nm.(model)
	if mm.pendingPerm != nil {
		t.Fatal("pending prompt must be cleared")
	}
	if !strings.Contains(mm.transcript.String(), "remote oversight timed out — deny applied") {
		t.Fatalf("transcript = %q, want the reason note", mm.transcript.String())
	}

	// Fallback: no note recorded → generic wording.
	resp2 := make(chan string, 1)
	m2 := model{st: &interactiveState{}, transcript: &strings.Builder{}, ta: textarea.New()}
	m2.pendingPerm = &permRequestMsg{prompt: "p", respCh: resp2}
	nm2, _ := m2.handlePermDismiss(permDismissMsg{respCh: resp2})
	if got := nm2.(model).transcript.String(); !strings.Contains(got, "(answered elsewhere)") {
		t.Fatalf("transcript = %q, want the generic fallback note", got)
	}
}
