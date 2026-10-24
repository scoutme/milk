package main

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

// toolWizardModel builds a model with just enough state to drive
// startAgentTool / handleAgentToolWizardKey: transcript builders, a textarea
// and an interactiveState backed by mvState's config fixture.
func toolWizardModel(t *testing.T) model {
	t.Helper()
	m := model{
		transcript: &strings.Builder{},
	}
	m.st = mvState(t)
	m.ta = textarea.New()
	return m
}

// sendToolWizardEnter feeds an answer + Enter to the wizard handler and
// returns the updated model.
func sendToolWizardEnter(t *testing.T, m model, answer string) model {
	t.Helper()
	m.ta.SetValue(answer)
	nm, _ := m.handleAgentToolWizardKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	out, ok := nm.(model)
	if !ok {
		t.Fatalf("handleAgentToolWizardKey returned %T, want model", nm)
	}
	return out
}

// TestStartAgentTool_MissingArgsOpensWizard: required parameters missing →
// the multi-step wizard starts instead of a bare usage line.
func TestStartAgentTool_MissingArgsOpensWizard(t *testing.T) {
	for _, sub := range []string{"add", "enable", "disable", "remove", "add coder"} {
		m := toolWizardModel(t)
		m, _ = m.startAgentTool(sub)
		if m.pendingTool == nil {
			t.Fatalf("startAgentTool(%q) did not open the wizard", sub)
		}
		if got := m.transcript.String(); !strings.Contains(got, milkTag()) {
			t.Errorf("startAgentTool(%q) transcript missing a prompt:\n%s", sub, got)
		}
	}
}

// TestStartAgentTool_InlineExecutesImmediately: fully specified calls keep the
// one-shot path — no wizard, config mutated.
func TestStartAgentTool_InlineExecutesImmediately(t *testing.T) {
	m := toolWizardModel(t)
	m, _ = m.startAgentTool(`add coder description="writes code" for local`)
	if m.pendingTool != nil {
		t.Fatal("wizard opened for a fully specified call")
	}
	ai := findAgentIdx(m.st.cfg, "local")
	if idx := findToolEntryIdx(m.st.cfg.Agents[ai].Tools, "coder"); idx < 0 {
		t.Fatalf("entry not added (transcript %q)", m.transcript.String())
	}
}

// TestAgentToolWizard_FlowAdd walks the add wizard end to end: name →
// description (with an embedded "for") → scope, then the entry lands on the
// scoped agent with the description intact.
func TestAgentToolWizard_FlowAdd(t *testing.T) {
	m := toolWizardModel(t)
	m, _ = m.startAgentTool("add")

	if m.pendingTool == nil || m.pendingTool.step != toolStepName {
		t.Fatalf("expected wizard at stepName, got %+v", m.pendingTool)
	}
	m = sendToolWizardEnter(t, m, "mimo-pro-local")
	if m.pendingTool == nil || m.pendingTool.step != toolStepDesc {
		t.Fatalf("expected wizard at stepDesc, got %+v", m.pendingTool)
	}
	m = sendToolWizardEnter(t, m, "coding agent for specific coding task")
	if m.pendingTool == nil || m.pendingTool.step != toolStepScope {
		t.Fatalf("expected wizard at stepScope, got %+v", m.pendingTool)
	}
	m = sendToolWizardEnter(t, m, "local")
	if m.pendingTool != nil {
		t.Fatalf("wizard not finished: %+v", m.pendingTool)
	}
	ai := findAgentIdx(m.st.cfg, "local")
	idx := findToolEntryIdx(m.st.cfg.Agents[ai].Tools, "mimo-pro-local")
	if idx < 0 {
		t.Fatalf("entry not added (transcript %q)", m.transcript.String())
	}
	if e := m.st.cfg.Agents[ai].Tools[idx]; e.Description != "coding agent for specific coding task" {
		t.Errorf("description = %q, want %q", e.Description, "coding agent for specific coding task")
	}
}

// TestAgentToolWizard_RequiredStepsRejectEmpty: empty answers re-prompt
// instead of committing a blank name/description.
func TestAgentToolWizard_RequiredStepsRejectEmpty(t *testing.T) {
	m := toolWizardModel(t)
	m, _ = m.startAgentTool("add")

	m = sendToolWizardEnter(t, m, "")
	if m.pendingTool == nil || m.pendingTool.step != toolStepName {
		t.Fatalf("empty name advanced the wizard: %+v", m.pendingTool)
	}
	m = sendToolWizardEnter(t, m, "helper2")
	m = sendToolWizardEnter(t, m, "")
	if m.pendingTool == nil || m.pendingTool.step != toolStepDesc {
		t.Fatalf("empty description advanced the wizard: %+v", m.pendingTool)
	}
}

// TestAgentToolWizard_SkippingScopeUsesPrimary: pressing Enter at the scope
// step leaves the scope empty — the documented default (active primary agent).
func TestAgentToolWizard_SkippingScopeUsesPrimary(t *testing.T) {
	m := toolWizardModel(t)
	m, _ = m.startAgentTool("add")

	m = sendToolWizardEnter(t, m, "helper2")
	m = sendToolWizardEnter(t, m, "helps with tasks")
	m = sendToolWizardEnter(t, m, "")
	if m.pendingTool != nil {
		t.Fatalf("wizard not finished: %+v", m.pendingTool)
	}
	primary := m.st.cfg.ActiveAgent().Name
	ai := findAgentIdx(m.st.cfg, primary)
	if idx := findToolEntryIdx(m.st.cfg.Agents[ai].Tools, "helper2"); idx < 0 {
		t.Fatalf("entry not added to primary %q (transcript %q)", primary, m.transcript.String())
	}
}

// TestAgentToolWizard_EscCancels: Esc aborts without touching config.
func TestAgentToolWizard_EscCancels(t *testing.T) {
	m := toolWizardModel(t)
	m, _ = m.startAgentTool("add")

	nm, _ := m.handleAgentToolWizardKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	out, ok := nm.(model)
	if !ok {
		t.Fatalf("handleAgentToolWizardKey returned %T, want model", nm)
	}
	if out.pendingTool != nil {
		t.Fatal("wizard still pending after esc")
	}
	for _, ac := range out.st.cfg.Agents {
		if len(ac.Tools) != 0 {
			t.Fatalf("config mutated after cancel: %v", ac.Tools)
		}
	}
}
