package main

// Tests for the background-ask prompt queue (ADR-0052): the timed answer's
// auto-default on overflow, the "a"/"d" bulk answers, the input draft stash,
// and the /permissions override grammar.

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/config"
)

// enterKey is the Enter keystroke handlePermKey answers on.
func enterKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEnter} }

func newPermTestModel() model {
	return model{
		st:                &interactiveState{cfg: config.Config{}},
		transcript:        &strings.Builder{},
		transcriptNoThink: &strings.Builder{},
		ta:                textarea.New(),
	}
}

func timedAsk(answer chan string) permRequestMsg {
	return permRequestMsg{
		prompt:      "\n[milk] permission request — primary agent tool: bash  (x)\n[milk] auto-deny in 6m0s — Allow? [Y/n] (a: allow all, d: deny all) [milk] ",
		respCh:      answer,
		autoDefault: "n",
		autoAt:      time.Now().Add(6 * time.Minute),
	}
}

func TestPermQueue_BulkAnswerAllowsAll(t *testing.T) {
	m := newPermTestModel()
	c1, c2, c3 := make(chan string, 1), make(chan string, 1), make(chan string, 1)

	nm, _ := m.handlePermRequest(timedAsk(c1))
	m = nm.(model)
	nm, _ = m.handlePermRequest(timedAsk(c2))
	m = nm.(model)
	nm, _ = m.handlePermRequest(timedAsk(c3))
	m = nm.(model)
	if m.pendingPerm == nil || len(m.permQueue) != 2 {
		t.Fatalf("pending=%v queue=%d, want 1 pending and 2 queued", m.pendingPerm != nil, len(m.permQueue))
	}

	// The answer field holds the typed bulk key.
	m.ta.SetValue("a")
	nm, _ = m.handlePermKey(enterKey())
	m = nm.(model)

	for i, c := range []chan string{c1, c2, c3} {
		select {
		case got := <-c:
			if got != "y" {
				t.Errorf("ask %d answered %q, want %q", i+1, got, "y")
			}
		default:
			t.Errorf("ask %d not answered", i+1)
		}
	}
	if m.pendingPerm != nil || len(m.permQueue) != 0 {
		t.Errorf("queue not cleared: pending=%v queued=%d", m.pendingPerm != nil, len(m.permQueue))
	}
}

func TestPermQueue_BulkAnswerDeniesAll(t *testing.T) {
	m := newPermTestModel()
	c1, c2 := make(chan string, 1), make(chan string, 1)
	nm, _ := m.handlePermRequest(timedAsk(c1))
	m = nm.(model)
	nm, _ = m.handlePermRequest(timedAsk(c2))
	m = nm.(model)

	m.ta.SetValue("d")
	nm, _ = m.handlePermKey(enterKey())
	m = nm.(model)

	for i, c := range []chan string{c1, c2} {
		select {
		case got := <-c:
			if got != "n" {
				t.Errorf("ask %d answered %q, want %q", i+1, got, "n")
			}
		default:
			t.Errorf("ask %d not answered", i+1)
		}
	}
	if m.pendingPerm != nil || len(m.permQueue) != 0 {
		t.Errorf("queue not cleared: pending=%v queued=%d", m.pendingPerm != nil, len(m.permQueue))
	}
}

func TestPermQueue_OverflowAppliesTimedDefault(t *testing.T) {
	m := newPermTestModel()
	var chans []chan string
	for i := 0; i < permQueueMax+2; i++ { // 1 pending + permQueueMax queued + 1 overflow
		c := make(chan string, 1)
		chans = append(chans, c)
		nm, _ := m.handlePermRequest(timedAsk(c))
		m = nm.(model)
	}
	if m.pendingPerm == nil || len(m.permQueue) != permQueueMax {
		t.Fatalf("queue = %d, want %d", len(m.permQueue), permQueueMax)
	}
	// The overflow ask (last one) must have been auto-answered with the
	// timed default ("n" here) instead of queued.
	select {
	case got := <-chans[len(chans)-1]:
		if got != "n" {
			t.Errorf("overflow ask answered %q, want %q", got, "n")
		}
	default:
		t.Error("overflow ask was not answered")
	}
}

func TestPermRequest_StashesInputAndRestoresOnDismiss(t *testing.T) {
	m := newPermTestModel()
	m.ta.SetValue("half-typed prompt")

	c := make(chan string, 1)
	nm, _ := m.handlePermRequest(timedAsk(c))
	m = nm.(model)
	if got := m.ta.Value(); got != "" {
		t.Errorf("input = %q, want empty while a prompt is pending", got)
	}

	nm, _ = m.handlePermDismiss(permDismissMsg{respCh: c})
	m = nm.(model)
	if got := m.ta.Value(); got != "half-typed prompt" {
		t.Errorf("input = %q, want the stashed draft back", got)
	}
	if m.permInputStash != nil {
		t.Error("stash not cleared")
	}
}

func TestExecPermissionsCore(t *testing.T) {
	cfg := config.Config{}
	ov := permOverrides{}

	if out, listing := execPermissionsCore("", cfg, &ov); !listing || out != "" {
		t.Errorf("bare form: out=%q listing=%v, want listing", out, listing)
	}

	out, listing := execPermissionsCore("default allow", cfg, &ov)
	if listing || !ov.bgAllowSet || !ov.bgAllow || ov.safetyAllowSet {
		t.Errorf("default allow: out=%q ov=%+v", out, ov)
	}
	out, _ = execPermissionsCore("default deny safety", cfg, &ov)
	if ov.safetyAllowSet && ov.safetyAllow {
		t.Errorf("default deny safety must not allow: out=%q ov=%+v", out, ov)
	}
	out, _ = execPermissionsCore("timeout 90 safety", cfg, &ov)
	if !ov.safetyTimeoutSet || ov.safetyTimeout != 90*time.Second {
		t.Errorf("timeout 90 safety: ov=%+v out=%q", ov, out)
	}
	out, _ = execPermissionsCore("timeout 0", cfg, &ov)
	if !strings.Contains(out, "usage:") {
		t.Errorf("timeout 0 should be a usage error, got %q", out)
	}
	out, _ = execPermissionsCore("bogus", cfg, &ov)
	if !strings.Contains(out, "usage:") {
		t.Errorf("bogus should be a usage error, got %q", out)
	}

	// The safety override must not bleed into the background policy and vice
	// versa (separate knobs).
	bg := bgAskPolicy(cfg, ov)
	if !bg.def || bg.deadline != cfg.PermBackgroundTimeout() {
		t.Errorf("bgAskPolicy = %+v, want default deadline %s and allow (from the session override)", bg, cfg.PermBackgroundTimeout())
	}
	sf := safetyAskPolicy(cfg, ov)
	if sf.def || sf.deadline != 90*time.Second {
		t.Errorf("safetyAskPolicy = %+v, want deadline 90s and deny", sf)
	}
}
