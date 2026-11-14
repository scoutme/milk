package main

// Tests for the foreground remote-timeout fuse (ADR-0052): the configured
// permission timeout (perm_timeout_secs / timeout_action) stays honoured for
// live prompts — the ask resolves with the remote's timeout action — and is
// never invisible: the prompt the hosts compose carries the same countdown
// the backend will fire at.

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/oversight"
)

// TestForegroundFuse: (duration, action) mirrors the config the backend is
// built from — for a remote surface only. No remote surface (nil / Noop) or
// no remote_oversight section means no fuse: the prompt waits for a human.
func TestForegroundFuse(t *testing.T) {
	remote := instantAnswerRemote{} // any non-Noop notifier counts as remote
	cases := []struct {
		name      string
		n         oversight.Notifier
		ro        *config.RemoteOversightConfig
		wantDur   time.Duration
		wantAllow bool
	}{
		{name: "nil notifier", n: nil, ro: &config.RemoteOversightConfig{}, wantDur: 0},
		{name: "noop notifier", n: oversight.Noop{}, ro: &config.RemoteOversightConfig{}, wantDur: 0},
		{name: "no oversight config", n: remote, ro: nil, wantDur: 0},
		{name: "defaults (120s / deny)", n: remote, ro: &config.RemoteOversightConfig{}, wantDur: 120 * time.Second},
		{name: "configured allow", n: remote, ro: &config.RemoteOversightConfig{PermTimeoutSecs: 90, TimeoutAction: "allow"}, wantDur: 90 * time.Second, wantAllow: true},
		{name: "configured deny", n: remote, ro: &config.RemoteOversightConfig{PermTimeoutSecs: 5}, wantDur: 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dur, allow := foregroundFuse(tc.n, tc.ro)
			if dur != tc.wantDur || allow != tc.wantAllow {
				t.Fatalf("foregroundFuse = (%v, %v), want (%v, %v)", dur, allow, tc.wantDur, tc.wantAllow)
			}
		})
	}
}

// TestForegroundPrompt: fuse 0 keeps the ordinary prompt with no timed
// metadata; a fuse composes the timed-answer note shape (so the queue
// display, status bar and permDisplayPrompt refresh all apply unchanged)
// with the matching AutoDefault, and the display refresh states the time
// actually left when the prompt is finally shown.
func TestForegroundPrompt(t *testing.T) {
	plain, autoDef, autoAt := foregroundPrompt("bash", "ls", 0, false)
	if plain != permAskPrompt("bash", "ls") || autoDef != "" || !autoAt.IsZero() {
		t.Fatalf("no fuse must keep the plain prompt: (%q, %q, %v)", plain, autoDef, autoAt)
	}

	allowPrompt, autoDef, autoAt := foregroundPrompt("bash", "ls", 90*time.Second, true)
	if !strings.Contains(allowPrompt, "auto-allow in 1m30s") {
		t.Fatalf("prompt must show the fuse: %q", allowPrompt)
	}
	if autoDef != "y" {
		t.Fatalf("autoDefault = %q, want y (timeout_action allow)", autoDef)
	}
	if rem := time.Until(autoAt); rem < 85*time.Second || rem > 91*time.Second {
		t.Fatalf("autoAt must be now+90s, got %v left", rem)
	}
	if !strings.Contains(allowPrompt, "— Allow? [Y/n/a/d]") {
		t.Fatalf("answer line missing: %q", allowPrompt)
	}

	denyPrompt, autoDef, _ := foregroundPrompt("bash", "ls", 90*time.Second, false)
	if !strings.Contains(denyPrompt, "auto-deny in 1m30s") || autoDef != "n" {
		t.Fatalf("deny fuse: (%q, %q)", denyPrompt, autoDef)
	}

	// The displayed prompt promises the REMAINING time: a fuse prompt
	// promoted from the queue (or rendered later) must re-render the note,
	// same rule as the background asks.
	msg := permRequestMsg{prompt: denyPrompt, autoDefault: autoDef, autoAt: time.Now().Add(30 * time.Second)}
	got := permDisplayPrompt(msg)
	if strings.Contains(got, "1m30s") || !strings.Contains(got, "auto-deny in 30s") {
		t.Fatalf("fuse countdown not refreshed: %q", got)
	}
}

// gatedRemote answers only when the test releases it, so the prompt can be
// composed and delivered first and its fields asserted deterministically.
type gatedRemote struct {
	oversight.Noop
	release chan struct{}
}

func (g *gatedRemote) AskPermission(ctx context.Context, _ oversight.PermRequest) (oversight.PermDecision, bool) {
	select {
	case <-g.release:
	case <-ctx.Done():
		return oversight.PermTimeout, false
	}
	return oversight.PermAllow, true
}

// TestMakeLocalPermAsk_FuseFromConfig: the TUI's foreground ask carries the
// configured timeout into the prompt it shows — countdown note, matching
// AutoDefault and AutoAt — whenever a remote surface exists, and the ask
// still resolves through the remote race as before.
func TestMakeLocalPermAsk_FuseFromConfig(t *testing.T) {
	msgs := make(chan permRequestMsg, 1)
	ir := &tuiInputReader{send: func(m tea.Msg) {
		if p, ok := m.(permRequestMsg); ok {
			msgs <- p
		}
		// No reply: readLineCtx waits on respCh/ctx like the real TUI.
	}}
	remote := &gatedRemote{release: make(chan struct{})}
	st := &interactiveState{cfg: config.Config{
		RemoteOversight: &config.RemoteOversightConfig{PermTimeoutSecs: 90, TimeoutAction: "allow"},
	}}
	ask := makeLocalPermAsk(newTUIHost(ir), nil, remote, st)

	done := make(chan bool, 1)
	go func() { done <- ask("bash", "ls") }()

	var msg permRequestMsg
	select {
	case msg = <-msgs:
	case <-time.After(3 * time.Second):
		t.Fatal("the prompt never reached the TUI")
	}
	if !strings.Contains(msg.prompt, "auto-allow in 1m30s") {
		t.Fatalf("prompt must carry the fuse: %q", msg.prompt)
	}
	if msg.autoDefault != "y" {
		t.Fatalf("autoDefault = %q, want y (timeout_action allow)", msg.autoDefault)
	}
	if rem := time.Until(msg.autoAt); rem < 85*time.Second || rem > 91*time.Second {
		t.Fatalf("AutoAt must be now+90s, got %v left", rem)
	}

	close(remote.release)
	select {
	case allow := <-done:
		if !allow {
			t.Fatal("the remote's allow must resolve the ask")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ask did not return after the remote answered")
	}
}

// TestMakeLocalPermAsk_NoRemoteNoFuse: without a remote surface (nil
// notifier) even a configured timeout must not decorate the prompt — there
// is no backend to fire it, and the plain prompt is the contract.
func TestMakeLocalPermAsk_NoRemoteNoFuse(t *testing.T) {
	var got permRequestMsg
	ir := &tuiInputReader{send: func(m tea.Msg) {
		if p, ok := m.(permRequestMsg); ok {
			got = p
			p.respCh <- "y"
		}
	}}
	st := &interactiveState{cfg: config.Config{
		RemoteOversight: &config.RemoteOversightConfig{PermTimeoutSecs: 90, TimeoutAction: "allow"},
	}}
	ask := makeLocalPermAsk(newTUIHost(ir), nil, nil, st)
	if !ask("bash", "ls") {
		t.Fatal("the client's y must allow")
	}
	if !strings.Contains(got.prompt, "permission request") || strings.Contains(got.prompt, "auto-") {
		t.Fatalf("prompt must stay plain: %q", got.prompt)
	}
	if got.autoDefault != "" || !got.autoAt.IsZero() {
		t.Fatalf("no timed metadata without a remote surface: (%q, %v)", got.autoDefault, got.autoAt)
	}
}
