package main

// Tests for /setup over ACP (acp_setup.go): the status/enable/disable
// replies, the interactive wizard (token consumed as typed chat, confirm
// over the clickable choice prompt or the typed floor, config committed
// and published to every live session), and its escape hatches. The
// wizard's two Telegram API calls and the notifier build are stubbed — no
// network, no bot.

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/transport/acp"
)

// stubTelegram wires the wizard's two Telegram API calls and the notifier
// build to fakes — no network, no bot. The returned fakeNotifier is a
// non-Noop type on purpose: oversightRemote() then reports an active
// backend, so the wizard's commit takes the "test message sent" branch it
// takes with a real one.
func stubTelegram(t *testing.T, bot string, getMeErr error, chatID int64, resolveErr error) *fakeNotifier {
	t.Helper()
	prevGet, prevRes := telegramGetMe, telegramResolveChatID
	telegramGetMe = func(string) (string, error) { return bot, getMeErr }
	telegramResolveChatID = func(string) (int64, error) { return chatID, resolveErr }
	t.Cleanup(func() { telegramGetMe, telegramResolveChatID = prevGet, prevRes })

	fake := &fakeNotifier{}
	prevNot := newNotifierFor
	newNotifierFor = func(config.Config) oversight.Notifier { return fake }
	t.Cleanup(func() { newNotifierFor = prevNot })
	return fake
}

// countingTestServer is acpTestServer with a model-call counter, so tests
// can pin that wizard answers and slash-command output never reach the
// model.
func countingTestServer(t *testing.T, calls *atomic.Int32) (*acpServer, *fakeACPConn) {
	t.Helper()
	return acpTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"model reply\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
}

// TestACPSetup_StatusUsageAndHelp: /setup telegram status reports the
// unconfigured state, a bogus subcommand shows the full usage line, and
// both — plus /help — are served from the command table without ever
// reaching the model.
func TestACPSetup_StatusUsageAndHelp(t *testing.T) {
	var calls atomic.Int32
	server, conn := countingTestServer(t, &calls)
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/setup telegram status")
	acpPromptText(t, server, id, "/setup bogus")
	acpPromptText(t, server, id, "/help")

	chunks := acpChunks(conn)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %q, want exactly three command outputs", chunks)
	}
	if !strings.Contains(chunks[0], "Telegram oversight: off") || !strings.Contains(chunks[0], "not configured") {
		t.Errorf("status = %q, want the off state and the not-configured credentials line", chunks[0])
	}
	if !strings.Contains(chunks[1], "usage: /setup telegram | /setup telegram on | /setup telegram off | /setup telegram status") {
		t.Errorf("bogus subcommand = %q, want the full usage line", chunks[1])
	}
	if !strings.Contains(chunks[2], "/setup telegram") || !strings.Contains(chunks[2], "Commands available over ACP") {
		t.Errorf("/help = %q, want /setup telegram listed", chunks[2])
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("model reached %d times, want 0 (all three are command output)", n)
	}

	// The advertised list carries /setup with its telegram hint.
	found := false
	for _, c := range acpAdvertisedCommands() {
		if c.Name == "setup" {
			found = true
			if c.Input == nil || !strings.Contains(c.Input.Hint, "telegram") {
				t.Errorf("advertised /setup input = %+v, want a telegram hint", c.Input)
			}
		}
	}
	if !found {
		t.Error("/setup not in the advertised command list")
	}
}

// TestACPSetup_EnableDisable: on without credentials is an error, off
// works (and persists); neither touches a network.
func TestACPSetup_EnableDisable(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/setup telegram on")
	acpPromptText(t, server, id, "/setup telegram off")
	acpPromptText(t, server, id, "/setup telegram status")

	chunks := acpChunks(conn)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %q, want exactly three command outputs", chunks)
	}
	if !strings.Contains(chunks[0], "no Telegram credentials configured") {
		t.Errorf("enable without credentials = %q, want the credentials error", chunks[0])
	}
	if !strings.Contains(chunks[1], "Telegram oversight disabled") {
		t.Errorf("disable = %q, want confirmation", chunks[1])
	}
	if !strings.Contains(chunks[2], "Telegram oversight: off") {
		t.Errorf("status after disable = %q, want off", chunks[2])
	}
}

// TestACPSetup_WizardTypedFlowCommits: the whole wizard over typed chat —
// the token is consumed by the wizard (never the model), the confirm step
// lands on its typed floor (a client whose permission responses carry no
// recognizable outcome is remembered in noChoice), and the commit saves
// the config, publishes it to the session, and pings the fake backend.
func TestACPSetup_WizardTypedFlowCommits(t *testing.T) {
	fake := stubTelegram(t, "testbot", nil, 4242, nil)
	var calls atomic.Int32
	server, conn := countingTestServer(t, &calls)
	id := acpNewSession(t, server)
	as := server.session(id)

	acpPromptText(t, server, id, "/setup telegram")
	acpPromptText(t, server, id, "123456:TESTTOKEN")
	acpPromptText(t, server, id, "done")

	chunks := acpChunks(conn)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %q, want intro/confirm/commit", chunks)
	}
	if !strings.Contains(chunks[0], "Bot token") {
		t.Errorf("intro = %q, want the token question", chunks[0])
	}
	if !strings.Contains(chunks[1], "bot validated: @testbot") || !strings.Contains(chunks[1], "type 'done'") {
		t.Errorf("after the token = %q, want validation and the typed confirm floor", chunks[1])
	}
	if !strings.Contains(chunks[2], "Telegram configured (chat_id: 4242)") {
		t.Errorf("commit = %q, want the configured summary", chunks[2])
	}
	if strings.Contains(chunks[2], "failed to activate") {
		t.Errorf("commit = %q, but the fake notifier is an active backend", chunks[2])
	}
	if as.pendingTelegram != nil {
		t.Error("pendingTelegram not cleared after the wizard finished")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("model reached %d times, want 0 — wizard answers are consumed by the wizard", n)
	}

	// Config committed: in the session copy (applyOversight published it)
	// and on disk.
	ro := as.st.cfg.RemoteOversight
	if ro == nil || ro.Backend != "telegram" || ro.Telegram == nil || ro.Telegram.ChatID != 4242 {
		t.Errorf("session config remote_oversight = %+v, want telegram backend with chat_id 4242", ro)
	}
	disk, err := config.LoadMerged()
	if err != nil {
		t.Fatalf("LoadMerged: %v", err)
	}
	if disk.RemoteOversight == nil || disk.RemoteOversight.Backend != "telegram" ||
		disk.RemoteOversight.Telegram == nil || disk.RemoteOversight.Telegram.ChatID != 4242 {
		t.Errorf("saved config remote_oversight = %+v, want telegram backend with chat_id 4242", disk.RemoteOversight)
	}

	// The rebuilt notifier got the test message.
	if joined := strings.Join(fake.recorded(), "\n"); !strings.Contains(joined, "Telegram oversight configured successfully") {
		t.Errorf("notifier calls = %q, want the configured-successfully test message", joined)
	}
}

// TestACPSetup_WizardClickableConfirm: on a client that answers
// session/request_permission, the confirm step is decided by a button —
// the option set carries the done/cancel pair the wizard shows.
func TestACPSetup_WizardClickableConfirm(t *testing.T) {
	fake := stubTelegram(t, "clickbot", nil, 777, nil)
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)

	var permReq acp.RequestPermissionRequest
	conn.respond = func(method string, params any) (any, error) {
		if method != "session/request_permission" {
			t.Errorf("client request = %q, want session/request_permission", method)
			return nil, nil
		}
		permReq = params.(acp.RequestPermissionRequest)
		return acp.RequestPermissionResponse{
			Outcome: acp.RequestPermissionOutcome{Outcome: "selected", OptionID: "done"},
		}, nil
	}

	acpPromptText(t, server, id, "/setup telegram")
	acpPromptText(t, server, id, "654321:CLICKTOKEN")

	chunks := acpChunks(conn)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %q, want intro + one combined answer/commit", chunks)
	}
	if !strings.Contains(chunks[1], "Telegram configured (chat_id: 777)") {
		t.Errorf("commit via button = %q, want the configured summary", chunks[1])
	}
	if permReq.Title != "Telegram setup" || len(permReq.Options) != 2 {
		t.Errorf("permission request = %+v, want title %q with two options", permReq, "Telegram setup")
	} else {
		if permReq.Options[0].OptionID != "done" || permReq.Options[1].OptionID != "cancel" {
			t.Errorf("option IDs = %q/%q, want done/cancel",
				permReq.Options[0].OptionID, permReq.Options[1].OptionID)
		}
		if permReq.Options[1].Kind != acp.PermissionRejectOnce {
			t.Errorf("cancel option kind = %q, want %q", permReq.Options[1].Kind, acp.PermissionRejectOnce)
		}
	}
	if joined := strings.Join(fake.recorded(), "\n"); !strings.Contains(joined, "Telegram oversight configured successfully") {
		t.Errorf("notifier calls = %q, want the configured-successfully test message", joined)
	}
}

// TestACPSetup_WizardCancelSwapAndBadToken: the escape hatches — a typed
// cancel word aborts; any other slash command cancels with a note naming
// how to restart; a wizard-swap command suppresses that note (the new
// wizard's banner speaks for itself); and a failed token validation ends
// the wizard (TUI parity: rerun /setup telegram).
func TestACPSetup_WizardCancelSwapAndBadToken(t *testing.T) {
	stubTelegram(t, "testbot", nil, 1, nil)
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)
	as := server.session(id)

	// Typed cancel.
	acpPromptText(t, server, id, "/setup telegram")
	acpPromptText(t, server, id, "cancel")
	if as.pendingTelegram != nil {
		t.Error("pendingTelegram survived a typed cancel word")
	}
	if ch := acpChunks(conn); !strings.Contains(ch[len(ch)-1], "telegram setup cancelled") {
		t.Errorf("cancel reply = %q, want the cancellation note", ch[len(ch)-1])
	}

	// A non-wizard slash command cancels and runs, with a restart hint.
	acpPromptText(t, server, id, "/setup telegram")
	acpPromptText(t, server, id, "/help")
	if as.pendingTelegram != nil {
		t.Error("pendingTelegram survived a slash command")
	}
	if last := acpChunks(conn); !strings.Contains(last[len(last)-1], "telegram setup cancelled — restart with /setup telegram") ||
		!strings.Contains(last[len(last)-1], "Commands available over ACP") {
		t.Errorf("slash-command reply = %q, want the cancel note plus the command output", last[len(last)-1])
	}

	// A wizard-swap command suppresses the note: the new wizard's banner
	// is the only thing said.
	acpPromptText(t, server, id, "/setup telegram")
	acpPromptText(t, server, id, "/config init")
	if as.pendingInit == nil {
		t.Fatal("/config init did not start the init wizard")
	}
	if last := acpChunks(conn); strings.Contains(last[len(last)-1], "telegram setup cancelled") ||
		!strings.Contains(last[len(last)-1], "primary agent name") {
		t.Errorf("swap reply = %q, want the init wizard banner and no telegram cancel note", last[len(last)-1])
	}
	acpPromptText(t, server, id, "cancel") // clear the init wizard

	// A failed token validation ends the wizard.
	telegramGetMe = func(string) (string, error) { return "", fmt.Errorf("401 Unauthorized") }
	acpPromptText(t, server, id, "/setup telegram")
	acpPromptText(t, server, id, "bogustoken")
	if as.pendingTelegram != nil {
		t.Error("pendingTelegram survived a failed token validation")
	}
	if last := acpChunks(conn); !strings.Contains(last[len(last)-1], "token validation failed") {
		t.Errorf("bad-token reply = %q, want the validation failure", last[len(last)-1])
	}
}

// TestWizardSwapPrompt: which commands count as wizard launches (they eat
// the pending wizard's cancel note) — the three wizards, and nothing else.
func TestWizardSwapPrompt(t *testing.T) {
	cases := []struct {
		prompt string
		want   bool
	}{
		{"/init", true},
		{"/config init", true},
		{"/setup telegram", true},
		{"/setup telegram off", true},
		{"/setup", false}, // usage line, not a wizard
		{"/help", false},
		{"/config show", false},
		{"plain prompt about /setup telegram", false},
	}
	for _, c := range cases {
		if got := wizardSwapPrompt(c.prompt); got != c.want {
			t.Errorf("wizardSwapPrompt(%q) = %v, want %v", c.prompt, got, c.want)
		}
	}
}
