package main

// Tests for the config-save scope surface shared by the CLI and the slash
// commands: the --local/--global token spelling (takeScopeFlag), /config
// init's argument grammar (parseInitArgs), the wizard's scope step (the
// TUI/ACP twin of the CLI's "Apply to local?" prompt) and the no-wizard
// bootstrap (`milk config init --empty` / `/config init empty`).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func TestTakeScopeFlag(t *testing.T) {
	cases := []struct {
		in        string
		wantRest  string
		wantScope string
	}{
		{"add name=x provider=local --local", "add name=x provider=local", "local"},
		{"--global remove foo", "remove foo", "global"},
		{"remove foo", "remove foo", ""},
		{"add name=x --local --global", "add name=x", "global"},     // last wins
		{"add name=x --LOCAL", "add name=x", "local"},               // case-insensitive
		{"add --local name=--global", "add name=--global", "local"}, // values are not tokens
	}
	for _, c := range cases {
		rest, scope := takeScopeFlag(c.in)
		if rest != c.wantRest || scope != c.wantScope {
			t.Errorf("takeScopeFlag(%q) = (%q, %q), want (%q, %q)", c.in, rest, scope, c.wantRest, c.wantScope)
		}
	}
}

func TestParseInitArgs(t *testing.T) {
	cases := []struct {
		in        string
		wantEmpty bool
		wantScope string
	}{
		{"", false, ""},
		{"empty", true, ""},
		{"empty global", true, "global"},
		{"empty --global", true, "global"},
		{"local", false, "local"},
		{"--global", false, "global"},
		{"EMPTY Global", true, "global"},
		{"nonsense words", false, ""}, // unrecognized words are ignored
	}
	for _, c := range cases {
		empty, scope := parseInitArgs(c.in)
		if empty != c.wantEmpty || scope != c.wantScope {
			t.Errorf("parseInitArgs(%q) = (%v, %q), want (%v, %q)", c.in, empty, scope, c.wantEmpty, c.wantScope)
		}
	}
}

// TestInitWizardScopeStep_DefaultLocal: the blank answer takes "local" — the
// CLI prompt's "[Y/n]" default — and the commit lands in the wizard's dir.
func TestInitWizardScopeStep_DefaultLocal(t *testing.T) {
	prev := config.SetRoot(t.TempDir())
	defer config.SetRoot(prev)
	stubWizardModels(t, nil, errors.New("no server"))
	dir := t.TempDir()

	st, _ := initWizardStart(dir)
	opts := localWizardOpts(1)
	for _, in := range []string{"1", "", "m", "n", ""} { // provider, url, model, escalation, tuning
		res := initWizardApply(st, in, opts)
		if res.Done {
			t.Fatalf("wizard finished before the scope question: %+v", res)
		}
	}
	if !strings.Contains(initWizardPrompt(st), "apply to local") {
		t.Fatalf("prompt = %q, want the save-scope question", initWizardPrompt(st))
	}
	res := initWizardApply(st, "", opts) // blank → local
	if !res.Done || res.Committed == nil {
		t.Fatalf("result = %+v, want Done with a committed config", res)
	}
	if st.scope != "local" {
		t.Errorf("st.scope = %q, want local (the blank answer's default)", st.scope)
	}
	p := filepath.Join(dir, config.LocalConfigDir, "config.json")
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the default scope must write the project-local config at %s: %v", p, err)
	}
}

// TestInitWizardScopeStep_GlobalAnswer: "n" (or "global") takes the global
// config, and the summary names the real global path.
func TestInitWizardScopeStep_GlobalAnswer(t *testing.T) {
	root := t.TempDir()
	prev := config.SetRoot(root)
	defer config.SetRoot(prev)
	stubWizardModels(t, nil, errors.New("no server"))

	st, _ := initWizardStart(t.TempDir())
	opts := localWizardOpts(1)
	for _, in := range []string{"1", "", "m", "n", ""} {
		initWizardApply(st, in, opts)
	}
	res := initWizardApply(st, "global", opts)
	if !res.Done || res.Committed == nil {
		t.Fatalf("result = %+v, want Done with a committed config", res)
	}
	if st.scope != "global" {
		t.Errorf("st.scope = %q, want global", st.scope)
	}
	if _, err := os.Stat(filepath.Join(root, "config.json")); err != nil {
		t.Errorf("global answer must write the state root's config.json: %v", err)
	}
	if !strings.Contains(res.Output, filepath.Join(root, "config.json")) {
		t.Errorf("summary = %q, want it to name the written global path", res.Output)
	}
}

// TestInitWizardScopeStep_InvalidReprompts: a rejected answer keeps the step.
func TestInitWizardScopeStep_InvalidReprompts(t *testing.T) {
	prev := config.SetRoot(t.TempDir())
	defer config.SetRoot(prev)
	st, _ := initWizardStart(t.TempDir())
	st.step = initStepScope
	res := initWizardApply(st, "maybe", localWizardOpts(1))
	if res.Done || !strings.Contains(res.Output, "invalid choice") {
		t.Fatalf("result = %+v, want an invalid-choice re-prompt", res)
	}
	if st.step != initStepScope {
		t.Errorf("st.step = %v, want initStepScope (unchanged by the rejected answer)", st.step)
	}
}

// TestInitWizardScopeStep_PreselectedSkipsQuestion: /config init local|global
// (or the CLI's --local/--global) lands without asking — the flag's spelling
// of skipping the prompt.
func TestInitWizardScopeStep_PreselectedSkipsQuestion(t *testing.T) {
	root := t.TempDir()
	prev := config.SetRoot(root)
	defer config.SetRoot(prev)
	stubWizardModels(t, nil, errors.New("no server"))

	st, _ := initWizardStart(t.TempDir())
	st.scope = "global"
	opts := localWizardOpts(1)
	for _, in := range []string{"1", "", "m", "n"} {
		initWizardApply(st, in, opts)
	}
	res := initWizardApply(st, "", opts) // tuning → skip; scope preselected → commit
	if !res.Done || res.Committed == nil {
		t.Fatalf("result = %+v, want Done with a committed config (no scope question)", res)
	}
	if strings.Contains(res.Output+res.Prompt, "apply to local") {
		t.Errorf("a preselected scope must not ask the question: %q", res.Output+res.Prompt)
	}
	if _, err := os.Stat(filepath.Join(root, "config.json")); err != nil {
		t.Errorf("preselected global must write the state root's config.json: %v", err)
	}
}

// TestRunConfigInitEmpty: the bootstrap creates the minimal file and refuses
// to touch an existing one.
func TestRunConfigInitEmpty(t *testing.T) {
	dir := t.TempDir()
	out := runConfigInitEmpty("", dir) // default: local
	if !strings.Contains(out, "empty local config created") || !strings.Contains(out, "inherit from the global config") {
		t.Errorf("output = %q, want the creation note with the inheritance hint", out)
	}
	p := filepath.Join(dir, config.LocalConfigDir, "config.json")
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "{}\n" {
		t.Fatalf("created file = %q, %v, want the minimal empty object", data, err)
	}
	if out := runConfigInitEmpty("local", dir); !strings.Contains(out, "already exists") {
		t.Errorf("second run = %q, want an already-exists note", out)
	}
}
