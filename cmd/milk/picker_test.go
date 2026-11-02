package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/scoutme/milk/internal/config"
)

// pickerTestModel builds a model whose config offers two background-agent
// candidates with a deterministic default (gemma), so the wizard opens
// instead of spawning directly.
func pickerTestModel(t *testing.T) model {
	t.Helper()
	m := newTestModelForPanels(t)
	m.st.cfg = config.Config{
		Agents: []config.AgentConfig{
			{Name: "gemma", Provider: "local", URL: "http://x", Model: "m"},
			{Name: "qwen", Provider: "local", URL: "http://x", Model: "m"},
		},
		BackgroundAgent: "gemma",
	}
	// viewString needs a sized, ready layout for the overlay assertions.
	updated, _ := m.handleResize(tea.WindowSizeMsg{Width: 400, Height: 50})
	m = updated.(model)
	return m
}

// pickerKey builds a text keypress the way the TUI receives typed runes.
func pickerKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

func TestAgentPickerEntries_MarksDefaultAndAliases(t *testing.T) {
	// Default outside the candidate list (an external-process background
	// agent): it still gets a row, marked and preselectable.
	entries := agentPickerEntries([]string{"gemma", "qwen"}, "claude")
	if len(entries) != 5 {
		t.Fatalf("entries = %+v, want candidates + aliases + external default", entries)
	}
	hinted := 0
	for _, e := range entries {
		if e.hint != "" {
			hinted++
			if e.name != "claude" {
				t.Errorf("hint on %q, want only on the default row", e.name)
			}
		}
	}
	if hinted != 1 {
		t.Errorf("hinted rows = %d, want 1", hinted)
	}
	if got := pickerDefaultIndex(entries); entries[got].name != "claude" {
		t.Errorf("default index = %d (%q), want the claude row", got, entries[got].name)
	}

	// A default that names a candidate marks that row instead of adding one,
	// and aliases never duplicate configured agent names.
	entries = agentPickerEntries([]string{"primary", "gemma"}, "gemma")
	if len(entries) != 3 { // primary, gemma, escalation
		t.Errorf("dedup entries = %+v, want 3 rows", entries)
	}
	for _, e := range entries {
		if e.name == "gemma" && e.hint == "" {
			t.Error("configured default row must carry the hint")
		}
	}
}

func TestAgentPickerAnswer_MapsSelectionAndTyped(t *testing.T) {
	entries := agentPickerEntries([]string{"gemma", "qwen"}, "gemma")
	for _, tc := range []struct {
		name, typed string
		sel         int
		want        string
	}{
		{"blank selects the default row", "", 0, ""},            // gemma row is (default)
		{"blank on another row", "", 1, "qwen"},                 // plain candidate row
		{"blank clamps out-of-range sel", "", 99, "escalation"}, // clamps to the last row
		{"typed narrows to the match", "qwe", 0, "qwen"},
		{"exact typed wins", "QWEN", 0, "qwen"},
		{"typed naming the default row maps to blank", "gemma", 0, ""},
		{"typed substring picks the alias", "esc", 0, "escalation"},
		{"unmatched text stays raw for validation", "zzz", 0, "zzz"},
	} {
		if got := agentPickerAnswer(entries, tc.typed, tc.sel); got != tc.want {
			t.Errorf("%s: answer(%q, sel=%d) = %q, want %q", tc.name, tc.typed, tc.sel, got, tc.want)
		}
	}
}

func TestBgAgentPicker_FlyoverFilterAccept(t *testing.T) {
	m := pickerTestModel(t)
	updated, cmd := m.startUserBackgroundAgent("research x", "")
	m = updated.(model)
	if m.pendingBgAgent == nil {
		t.Fatal("two candidates must open the picker, not spawn")
	}
	if cmd == nil {
		t.Fatal("opening the picker must arm the timed-default tick")
	}
	if m.pendingBgAgent.sel != 0 {
		t.Errorf("sel = %d, want the default row preselected", m.pendingBgAgent.sel)
	}

	view := ansi.Strip(m.viewString())
	for _, want := range []string{"background agent", "gemma (default)", "qwen", "auto-picks gemma in"} {
		if !strings.Contains(view, want) {
			t.Errorf("flyover view missing %q:\n%s", want, view)
		}
	}

	// Arrows move; Enter on the highlighted row accepts it.
	updated, _ = m.handleBgAgentWizardKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(model)
	if m.pendingBgAgent.sel != 1 {
		t.Fatalf("down = sel %d, want 1", m.pendingBgAgent.sel)
	}
	updated, _ = m.handleBgAgentWizardKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(model)
	if m.pendingBgAgent != nil {
		t.Fatal("Enter must resolve the wizard (spawn attempt follows)")
	}
	if !strings.Contains(m.transcript.String(), "qwen") {
		t.Errorf("transcript must record the chosen agent:\n%s", m.transcript.String())
	}

	// Any key disarms the timed default — the footer line disappears.
	m2 := pickerTestModel(t)
	updated, _ = m2.startUserBackgroundAgent("y", "")
	m2 = updated.(model)
	updated, _ = m2.handleBgAgentWizardKey(pickerKey('x'))
	m2 = updated.(model)
	if !m2.pendingBgAgent.disarmed {
		t.Fatal("a key must disarm the timed default")
	}
	if v := ansi.Strip(m2.renderAgentPicker(72)); strings.Contains(v, "auto-picks") {
		t.Errorf("disarmed picker must not offer a countdown:\n%s", v)
	}
}

func TestBgAgentPicker_TypedFilterAndUnknown(t *testing.T) {
	m := pickerTestModel(t)
	updated, _ := m.startUserBackgroundAgent("research y", "")
	m = updated.(model)

	// A narrowing prefix answers the row it filtered (palette-style).
	for _, r := range "qwe" {
		updated, _ = m.handleBgAgentWizardKey(pickerKey(r))
		m = updated.(model)
	}
	updated, _ = m.handleBgAgentWizardKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(model)
	if m.pendingBgAgent != nil || !strings.Contains(m.transcript.String(), "qwen") {
		t.Fatalf("filtered accept: pending=%v transcript=%q", m.pendingBgAgent, m.transcript.String())
	}

	// Unmatched text keeps the old validation loop: re-prompt, keep wizard.
	m2 := pickerTestModel(t)
	updated, _ = m2.startUserBackgroundAgent("research z", "")
	m2 = updated.(model)
	for _, r := range "zzz" {
		updated, _ = m2.handleBgAgentWizardKey(pickerKey(r))
		m2 = updated.(model)
	}
	updated, _ = m2.handleBgAgentWizardKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m2 = updated.(model)
	if m2.pendingBgAgent == nil {
		t.Fatal("unknown agent must re-prompt, not spawn")
	}
	if !strings.Contains(m2.transcript.String(), `unknown agent "zzz"`) {
		t.Errorf("transcript missing the rejection:\n%s", m2.transcript.String())
	}
}

func TestBgAgentPicker_TimeoutDefaultAndDisarm(t *testing.T) {
	// Expired, untouched: the default agent runs and the record says so.
	m := pickerTestModel(t)
	updated, _ := m.startUserBackgroundAgent("walk away", "")
	m = updated.(model)
	w := m.pendingBgAgent
	w.deadline = time.Now().Add(-time.Second)
	updated, _ = m.Update(bgAgentPickerTickMsg{})
	m = updated.(model)
	if m.pendingBgAgent != nil {
		t.Fatal("expired deadline must resolve the wizard")
	}
	if got := m.transcript.String(); !strings.Contains(got, "no answer within") || !strings.Contains(got, "(default)") {
		t.Errorf("timeout record missing:\n%s", got)
	}

	// Disarmed by a key: the tick must not resolve anything.
	m2 := pickerTestModel(t)
	updated, _ = m2.startUserBackgroundAgent("walk away", "")
	m2 = updated.(model)
	updated, _ = m2.handleBgAgentWizardKey(pickerKey('x'))
	m2 = updated.(model)
	m2.pendingBgAgent.deadline = time.Now().Add(-time.Second)
	updated, _ = m2.Update(bgAgentPickerTickMsg{})
	m2 = updated.(model)
	if m2.pendingBgAgent == nil {
		t.Fatal("a disarmed picker must ignore the deadline")
	}

	// Not yet expired: the tick re-arms instead of resolving.
	m3 := pickerTestModel(t)
	updated, cmd := m3.startUserBackgroundAgent("still deciding", "")
	m3 = updated.(model)
	updated, cmd2 := m3.Update(bgAgentPickerTickMsg{})
	m3 = updated.(model)
	if m3.pendingBgAgent == nil || cmd2 == nil {
		t.Errorf("early tick: pending=%v re-armed=%v, want both", m3.pendingBgAgent, cmd2)
	}
	_ = cmd
}

func TestWorkflowRolePicker_FlyoverAdvance(t *testing.T) {
	m := pickerTestModel(t)
	w := &workflowWizardState{name: "swarm", roles: []string{"gen", "rev", "qa"}, roleValues: map[string]string{}}
	updated, _ := m.advanceToNextGenericRoleOrLaunch(w)
	m = updated.(model)
	if m.pendingWorkflowWizard == nil || m.pendingWorkflowWizard.step != wizardStepGenericRole {
		t.Fatalf("wizard = %+v, want the role step", m.pendingWorkflowWizard)
	}
	// Blank-equivalent row (escalation) preselected.
	if got, entries := m.pendingWorkflowWizard.roleSel, m.rolePickerEntries(&workflowWizardState{roles: w.roles, roleIdx: 0}); got != pickerDefaultIndex(entries) {
		t.Errorf("roleSel = %d, want default index %d", got, pickerDefaultIndex(entries))
	}
	view := ansi.Strip(m.viewString())
	if !strings.Contains(view, "workflow swarm — gen agent") {
		t.Errorf("flyover title missing from view:\n%s", view)
	}

	// Blank Enter answers the default (escalation) and advances one role.
	updated, _ = m.handleWorkflowWizardKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(model)
	got := m.pendingWorkflowWizard
	if got == nil || got.roleIdx != 1 || got.step != wizardStepGenericRole {
		t.Fatalf("after blank answer: %+v, want role 1 of the role step", got)
	}
	if m.pendingWorkflowWizard.roleValues["gen"] != "escalation" {
		t.Errorf("gen = %q, want escalation", m.pendingWorkflowWizard.roleValues["gen"])
	}
	if !strings.Contains(m.transcript.String(), "escalation") {
		t.Errorf("record should name the chosen row:\n%s", m.transcript.String())
	}

	// Typed filter answers the highlighted match; arrows move the cursor.
	for _, r := range "gem" {
		updated, _ = m.handleWorkflowWizardKey(pickerKey(r))
		m = updated.(model)
	}
	updated, _ = m.handleWorkflowWizardKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(model)
	if m.pendingWorkflowWizard == nil {
		t.Fatal("third role must still be unanswered")
	}
	if m.pendingWorkflowWizard.roleValues["rev"] != "gemma" {
		t.Errorf("rev = %q, want gemma (filter answer)", m.pendingWorkflowWizard.roleValues["rev"])
	}
	updated, _ = m.handleWorkflowWizardKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(model)
	sel := m.pendingWorkflowWizard.roleSel
	updated, _ = m.handleWorkflowWizardKey(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(model)
	if m.pendingWorkflowWizard.roleSel == sel && sel == 0 {
		// down from 0 moved it; up moved it back — either way the pair must
		// have moved at least once. sel==0 && same would mean both no-ops.
		t.Error("arrow keys must move the role cursor")
	}
	updated, _ = m.handleWorkflowWizardKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(model)
	if m.pendingWorkflowWizard != nil {
		t.Error("esc must cancel the wizard")
	}
}

func TestOverlayAgentPicker_NoPickerIsNoop(t *testing.T) {
	m := pickerTestModel(t)
	if got := m.overlayAgentPicker("abc"); got != "abc" {
		t.Errorf("overlay without a picker changed the block: %q", got)
	}
	if got := m.renderAgentPicker(72); got != "" {
		t.Errorf("render without a picker = %q, want empty", got)
	}
}
