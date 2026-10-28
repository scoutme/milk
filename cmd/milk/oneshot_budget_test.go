package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/transport/streamjson"
)

// The one-shot default must stay long enough for the hardest scoring
// scenario: the original hardcoded 10m killed eval's
// proceed-on-hard-design-task second turn mid-work (milk eval,
// 2026-10-28). 20m doubles that and matches local.Agent's defaultJobTimeout
// for background jobs.
func TestOneShotTurnTimeoutDefault(t *testing.T) {
	if flagTurnTimeout != 20*time.Minute {
		t.Errorf("--turn-timeout default = %v, want 20m", flagTurnTimeout)
	}
}

// oneShotBudgetErr must name an expired budget, keep errors.Is intact, and
// pass everything else through unchanged (the bare "Error: context deadline
// exceeded" that hid the 10-minute constant for so long was exactly this
// function missing).
func TestOneShotBudgetErr(t *testing.T) {
	budget := 20 * time.Minute
	bare := context.DeadlineExceeded

	// Named wrap for a deadline expiry, errors.Is preserved.
	got := oneShotBudgetErr(bare, budget)
	if got == nil || !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(DeadlineExceeded) = false for %v", got)
	}
	for _, want := range []string{"turn budget of 20m0s exceeded", "--turn-timeout", "0 disables"} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("message %q does not mention %q", got, want)
		}
	}

	// Wrapped deep inside a turn (e.g. dispatch's ctx.Done select) — still named.
	deep := fmt.Errorf("executeWithRetry: %w", context.DeadlineExceeded)
	if got := oneShotBudgetErr(deep, budget); !strings.Contains(got.Error(), "turn budget of 20m0s exceeded") {
		t.Errorf("wrapped deadline not named: %v", got)
	}

	// Non-deadline errors pass through untouched.
	boom := errors.New("boom")
	if got := oneShotBudgetErr(boom, budget); !errors.Is(got, boom) {
		t.Errorf("plain error changed: %v", got)
	}

	// No budget (0 — flag disables the deadline) means nothing can expire.
	if got := oneShotBudgetErr(bare, 0); got != bare {
		t.Errorf("budget 0 must pass the error through, got %v", got)
	}

	if got := oneShotBudgetErr(nil, budget); got != nil {
		t.Errorf("nil error = %v, want nil", got)
	}
}

// responseCapture: segment chunks accumulate (a budget-killed turn keeps its
// partial text), the final successful response replaces them.
func TestResponseCapture(t *testing.T) {
	var c responseCapture
	c.onSegment("partial ")
	c.onSegment("work")
	if c.text != "partial work" {
		t.Errorf("after segments: %q, want %q", c.text, "partial work")
	}
	c.onResponse("the full final answer")
	if c.text != "the full final answer" {
		t.Errorf("final must replace chunks: %q", c.text)
	}
}

// buildErrorEvent must produce the design §6.3 system/error shape:
// is_error:true, message (string form), recoverable.
func TestBuildErrorEvent(t *testing.T) {
	msg := "turn budget of 40ms exceeded (--turn-timeout, 0 disables): context deadline exceeded"
	ev := buildErrorEvent("sess_1", errors.New(msg))

	if ev.Type != streamjson.TypeSystem || ev.Subtype != streamjson.SubtypeError {
		t.Errorf("Type/Subtype = %q/%q, want system/error", ev.Type, ev.Subtype)
	}
	if ev.SessionID != "sess_1" {
		t.Errorf("SessionID = %q", ev.SessionID)
	}
	if ev.IsError == nil || !*ev.IsError {
		t.Errorf("IsError = %v, want true", ev.IsError)
	}
	got, err := ev.MessageText()
	if err != nil || got != msg {
		t.Errorf("MessageText = %q, %v; want %q", got, err, msg)
	}
	if ev.Recoverable == nil || *ev.Recoverable {
		t.Errorf("Recoverable = %v, want false for a deadline expiry", ev.Recoverable)
	}

	// Transient HTTP/2 stream failures are the retryable class.
	h2 := buildErrorEvent("sess_1", fmt.Errorf("stream reset: %w", http2.StreamError{StreamID: 3, Code: http2.ErrCodeCancel}))
	if h2.Recoverable == nil || !*h2.Recoverable {
		t.Errorf("Recoverable = %v, want true for an HTTP/2 stream error", h2.Recoverable)
	}

	line, err := streamjson.EncodeLine(ev)
	if err != nil {
		t.Fatalf("EncodeLine: %v", err)
	}
	if _, err := streamjson.Decode(line[:len(line)-1]); err != nil {
		t.Fatalf("Decode(EncodeLine(ev)): %v", err)
	}
}

// emitOneShotOutcome pins the terminal sequence a batch consumer sees:
// [assistant?] [system/error?] result — exactly one result, last.
func TestEmitOneShotOutcome(t *testing.T) {
	budgetErr := errors.New("turn budget of 20m0s exceeded (--turn-timeout, 0 disables): context deadline exceeded")

	for _, tc := range []struct {
		name        string
		captured    string
		turnErr     error
		wantEvents  []string // "assistant" | "system/error" | "result"
		wantResult  string
		wantIsError bool
		wantSubtype string
	}{
		{
			name:        "success with text",
			captured:    "the answer",
			turnErr:     nil,
			wantEvents:  []string{"assistant", "result"},
			wantResult:  "the answer",
			wantIsError: false,
			wantSubtype: streamjson.ResultSuccess,
		},
		{
			name:        "killed with partial text",
			captured:    "partial work streamed before the kill",
			turnErr:     budgetErr,
			wantEvents:  []string{"assistant", "system/error", "result"},
			wantResult:  "partial work streamed before the kill",
			wantIsError: true,
			wantSubtype: streamjson.ResultErrorDuringExecution,
		},
		{
			name:        "killed before any text",
			captured:    "",
			turnErr:     budgetErr,
			wantEvents:  []string{"system/error", "result"},
			wantResult:  budgetErr.Error(), // diagnosable fallback (design failed-turn example)
			wantIsError: true,
			wantSubtype: streamjson.ResultErrorDuringExecution,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := streamjson.NewEncoder(&buf)
			emitOneShotOutcome(enc, "sess_1", router.TargetLocal, tc.turnErr, tc.captured, 42, 1, nil, nil)

			dec := streamjson.NewDecoder(&buf)
			var got []string
			var lastResult *streamjson.Event
			var errMsg string
			for {
				ev, err := dec.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("decoding stream: %v", err)
				}
				switch ev.Type {
				case streamjson.TypeAssistant:
					got = append(got, "assistant")
				case streamjson.TypeResult:
					got = append(got, "result")
					lastResult = ev
				case streamjson.TypeSystem:
					if ev.Subtype == streamjson.SubtypeError {
						got = append(got, "system/error")
						m, _ := ev.MessageText()
						errMsg = m
					}
				default:
					t.Errorf("unexpected event in terminal sequence: %s/%s", ev.Type, ev.Subtype)
				}
			}

			if strings.Join(got, ",") != strings.Join(tc.wantEvents, ",") {
				t.Errorf("event sequence = %v, want %v", got, tc.wantEvents)
			}
			if lastResult == nil {
				t.Fatal("no terminal result event")
			}
			if lastResult.Subtype != tc.wantSubtype {
				t.Errorf("result subtype = %q, want %q", lastResult.Subtype, tc.wantSubtype)
			}
			if lastResult.IsError == nil || *lastResult.IsError != tc.wantIsError {
				t.Errorf("result is_error = %v, want %v", lastResult.IsError, tc.wantIsError)
			}
			if lastResult.Result != tc.wantResult {
				t.Errorf("result text = %q, want %q", lastResult.Result, tc.wantResult)
			}
			if tc.turnErr != nil && errMsg != tc.turnErr.Error() {
				t.Errorf("error banner message = %q, want %q", errMsg, tc.turnErr.Error())
			}
		})
	}
}
