package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/transport/acp"
)

// Busy Enter on /permissions and /skip-permissions executes at once: the
// permission family is exactly what a user needs while a turn is running (or
// about to ask), so it must not be answered with "unavailable while agent is
// responding".
func TestPermissionsCommands_RunWhileBusy(t *testing.T) {
	tests := []struct {
		input string
		check func(t *testing.T, m model)
	}{
		{"/permissions default allow", func(t *testing.T, m model) {
			if ov := m.st.permOverridesNow(); !ov.bgAllowSet || !ov.bgAllow {
				t.Errorf("override not applied while busy: %+v", ov)
			}
		}},
		{"/permissions", func(t *testing.T, m model) {
			if !strings.Contains(m.transcript.String(), "permission asks") {
				t.Errorf("listing not printed while busy: %q", m.transcript.String())
			}
		}},
		{"/skip-permissions on", func(t *testing.T, m model) {
			if !m.st.skipPermissions.Load() {
				t.Error("/skip-permissions on did not apply while busy")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			m := layoutTestModel(t, 100, 30)
			m.st.notifier = newNotifier(m.st.cfg)
			m.busy = true
			m.ta.SetValue(tt.input)
			out, _ := m.handleBusyKey(tea.KeyPressMsg{Code: tea.KeyEnter})
			got := out.(model)
			if strings.Contains(got.busyHint, "unavailable") {
				t.Fatalf("busy Enter refused %q: %q", tt.input, got.busyHint)
			}
			if !got.busy {
				t.Error("running a busy-safe command must not clear the busy state")
			}
			tt.check(t, got)
		})
	}
}

// Agent goroutines read the overrides (background asks) and the skip flag
// (permission checks) while the UI goroutine writes them; run under -race.
func TestPermissionsState_ConcurrentReadWrite(t *testing.T) {
	m := layoutTestModel(t, 100, 30)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = bgAskPolicy(m.st.cfg, m.st.permOverridesNow())
			_ = m.st.skipPermissions.Load()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			mm := m
			mm.handlePermissionsCmd("default allow")
			mm.handlePermissionsCmd("timeout 30")
			execSkipPerms("on", m.st)
		}
	}()
	wg.Wait()
}

// Every ACP busy-safe command has a plain run handler and is also busy-safe
// in the TUI — the two lists must not drift.
func TestACPBusySafe_SubsetOfTUIAndPlainHandlers(t *testing.T) {
	table := map[string]acpCommand{}
	for _, c := range acpCommandTable() {
		table[c.name] = c
	}
	for cmd := range acpBusySafe {
		if !busySafeCommands[cmd] {
			t.Errorf("%s is ACP busy-safe but not TUI busy-safe", cmd)
		}
		c, ok := table[cmd]
		if !ok || c.run == nil {
			t.Errorf("%s is ACP busy-safe but has no plain run handler", cmd)
		}
	}
}

// A client prompt /permissions while a turn is running answers at once
// (while the test holds the running turn's lock), applies the override, and
// leaves the running turn alone.
func TestACPPermissions_BusyPromptRunsWithoutQueueing(t *testing.T) {
	server, _ := acpTestServer(t, "MUST NOT REACH THE MODEL")
	id := acpNewSession(t, server)
	as := server.session(id)

	as.turnMu.Lock()
	defer as.turnMu.Unlock()
	canceled := false
	as.mu.Lock()
	as.turnCtx = context.Background()
	as.cancel = func() { canceled = true }
	as.mu.Unlock()
	defer func() {
		as.mu.Lock()
		as.turnCtx = nil
		as.cancel = nil
		as.mu.Unlock()
	}()

	done := make(chan any, 1)
	go func() {
		done <- acpRequest(t, server, "session/prompt", acp.PromptRequest{
			SessionID: id,
			Prompt:    []acp.ContentBlock{acp.TextBlock("/permissions default allow")},
		})
	}()
	select {
	case res := <-done:
		if res == nil {
			t.Fatal("no PromptResponse")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/permissions blocked on the running turn's lock (turnMu)")
	}
	if canceled {
		t.Error("a busy-safe command must not cancel the running turn")
	}
	as.mu.Lock()
	ov := as.permOverrides
	as.mu.Unlock()
	if !ov.bgAllowSet || !ov.bgAllow {
		t.Errorf("override not applied: %+v", ov)
	}
	var sawEcho, sawResult bool
	for _, n := range server.conn.(*fakeACPConn).sent() {
		upd, ok := n.Params.(acp.UpdateSessionNotification)
		if !ok {
			continue
		}
		switch u := upd.Update.(type) {
		case acp.MessageUpsert:
			for _, b := range u.Content {
				if u.SessionUpdate == "user_message" && strings.Contains(b.Text, "/permissions") {
					sawEcho = true
				}
			}
		case acp.ContentChunk:
			if strings.Contains(u.Content.Text, "background timed answer") {
				sawResult = true
			}
		}
	}
	if !sawEcho || !sawResult {
		t.Errorf("client should see the echo and the result (echo=%v result=%v)", sawEcho, sawResult)
	}
}
