package main

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/events"
)

// fakeSyncSend returns a send func that, given a permRequestMsg, immediately
// replies on its respCh with answer — safe because tuiInputReader.send is
// called synchronously before readLineLabeled blocks on the (buffered-1)
// channel, so no goroutine is needed to drive this in a unit test.
func fakeSyncSend(t *testing.T, answer string, gotPrompt, gotLabel *string) func(tea.Msg) {
	t.Helper()
	return func(msg tea.Msg) {
		pr, ok := msg.(permRequestMsg)
		if !ok {
			t.Fatalf("send got %T, want permRequestMsg", msg)
		}
		*gotPrompt = pr.prompt
		*gotLabel = pr.label
		pr.respCh <- answer
	}
}

func TestTUIHost_RequestPermission(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		want   bool
	}{
		{"empty defaults to allow", "", true},
		{"lowercase y allows", "y", true},
		{"uppercase Y allows", "Y", true},
		{"n denies", "n", false},
		{"anything else denies", "nope", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPrompt, gotLabel string
			ir := &tuiInputReader{send: fakeSyncSend(t, tt.answer, &gotPrompt, &gotLabel)}
			host := newTUIHost(ir)

			outcome, err := host.RequestPermission(context.Background(), events.PermissionRequest{Prompt: "allow? "})
			if err != nil {
				t.Fatalf("RequestPermission error: %v", err)
			}
			if outcome.Allow != tt.want {
				t.Errorf("Allow = %v, want %v", outcome.Allow, tt.want)
			}
			if gotPrompt != "allow? " {
				t.Errorf("prompt sent = %q, want %q", gotPrompt, "allow? ")
			}
		})
	}
}

func TestTUIHost_Elicit(t *testing.T) {
	var gotPrompt, gotLabel string
	ir := &tuiInputReader{send: fakeSyncSend(t, "some answer", &gotPrompt, &gotLabel)}
	host := newTUIHost(ir)

	result, err := host.Elicit(context.Background(), events.ElicitationRequest{Prompt: "pick one: ", Label: "[select]"})
	if err != nil {
		t.Fatalf("Elicit error: %v", err)
	}
	if result.Value != "some answer" {
		t.Errorf("Value = %q, want %q", result.Value, "some answer")
	}
	if gotLabel != "[select]" {
		t.Errorf("label sent = %q, want %q", gotLabel, "[select]")
	}
}

func TestTUIHost_Notify(t *testing.T) {
	var got notifyMsg
	ir := &tuiInputReader{send: func(msg tea.Msg) {
		n, ok := msg.(notifyMsg)
		if !ok {
			t.Fatalf("send got %T, want notifyMsg", msg)
		}
		got = n
	}}
	host := newTUIHost(ir)

	host.Notify(events.Notification{Text: "hello", CommandHint: "/foo"})

	if got.text != "hello" || got.hint != "/foo" {
		t.Errorf("notifyMsg = %+v, want {text:hello hint:/foo}", got)
	}
}

// TestMakeLocalPermAsk_PromptUnchanged locks the exact prompt text
// makeLocalPermAsk renders — unchanged by the Host migration (Phase 1 of
// docs/machine-readable-output-design.md) — and that y/n parsing still
// matches pre-migration behavior.
func TestMakeLocalPermAsk_PromptUnchanged(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		summary    string
		answer     string
		wantPrompt string
		wantAllow  bool
	}{
		{
			name:       "with summary, allowed",
			tool:       "bash",
			summary:    "ls -la",
			answer:     "",
			wantPrompt: "\n[milk] permission request — primary agent tool: bash  (ls -la)\n[milk] Allow? [Y/n] ",
			wantAllow:  true,
		},
		{
			name:       "no summary, denied",
			tool:       "write_file",
			summary:    "",
			answer:     "n",
			wantPrompt: "\n[milk] permission request — primary agent tool: write_file\n[milk] Allow? [Y/n] ",
			wantAllow:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldTTY := isTTY
			isTTY = false // keep rendered text ANSI-free and byte-comparable
			t.Cleanup(func() { isTTY = oldTTY })

			var gotPrompt, gotLabel string
			ir := &tuiInputReader{send: fakeSyncSend(t, tt.answer, &gotPrompt, &gotLabel)}
			ask := makeLocalPermAsk(newTUIHost(ir), nil)

			allow := ask(tt.tool, tt.summary)

			if allow != tt.wantAllow {
				t.Errorf("allow = %v, want %v", allow, tt.wantAllow)
			}
			if gotPrompt != tt.wantPrompt {
				t.Errorf("prompt = %q, want %q", gotPrompt, tt.wantPrompt)
			}
		})
	}
}
