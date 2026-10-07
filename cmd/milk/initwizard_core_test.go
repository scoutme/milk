package main

// Tests for the host-independent /config init wizard core (initwizard_core.go)
// and for /init's registration across the TUI surfaces (extraction, completion
// variants, help, dispatch) — the pattern TestNotificationsRegistration pins
// for /clear, applied to the wizard alias.

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// localWizardOpts is the ACP-style option set (no editor question): the agent
// tools step is skipped for a config with at most one agent.
func localWizardOpts(numAgents int) initWizardOpts {
	return initWizardOpts{NumAgents: numAgents, OfferOpenEditor: false}
}

// TestInitWizardApply_LocalProviderFlow drives the common first-run path
// (llama.cpp-style local provider) end to end and checks the prompt sequence,
// the final commit and that no step asks about opening an editor.
func TestInitWizardApply_LocalProviderFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	st, banner := initWizardStart()
	if !strings.Contains(banner, "setup wizard") {
		t.Fatalf("banner = %q, want the setup-wizard banner", banner)
	}
	if p := initWizardPrompt(st); !strings.Contains(p, "primary agent name") {
		t.Fatalf("first prompt = %q, want the name question", p)
	}

	steps := []struct {
		in, want string
	}{
		{"", "provider — select"},             // blank name → "local"
		{"1", "server URL"},                   // provider 1 = local
		{"http://localhost:8080", "run_cmd:"}, // local → run_cmd step
		{"", "model name"},                    // blank run_cmd → model
		{"qwen2.5-coder", "context window"},   // → limits step
		{"", "escalation agent?"},             // blank limits → escalation
		{"n", "config written to"},            // no escalation → commit + summary
	}
	var res initWizardResult
	for i, s := range steps {
		res = initWizardApply(st, s.in, localWizardOpts(1))
		if shown := res.Output + res.Prompt; !strings.Contains(shown, s.want) {
			t.Errorf("step %d (input %q): output+prompt = %q, want substring %q", i, s.in, shown, s.want)
		}
		wantDone := i == len(steps)-1
		if res.Done != wantDone {
			t.Errorf("step %d (input %q): Done = %v, want %v", i, s.in, res.Done, wantDone)
		}
		if i < len(steps)-1 && res.Committed != nil {
			t.Errorf("step %d: committed config before the final step", i)
		}
	}
	if !res.Done || res.Committed == nil {
		t.Fatalf("final result = %+v, want Done with a committed config", res)
	}
	if res.OpenEditor {
		t.Error("OfferOpenEditor=false must never ask about opening an editor")
	}
	cfg := *res.Committed
	if cfg.Agent != "local" || len(cfg.Agents) != 1 {
		t.Errorf("cfg.Agent = %q, len(cfg.Agents) = %d, want single agent %q", cfg.Agent, len(cfg.Agents), "local")
	}
	a := cfg.Agents[0]
	if a.URL != "http://localhost:8080" || a.Model != "qwen2.5-coder" || a.Provider != "local" {
		t.Errorf("committed agent = %+v", a)
	}
	if cfg.EscalationAgent != "" || len(cfg.AgentTools) > 0 {
		t.Errorf("escalation = %q, agent tools = %v, want neither (answered n, NumAgents=1)", cfg.EscalationAgent, cfg.AgentTools)
	}
	if st.step != initStepDone {
		t.Errorf("st.step = %v, want initStepDone", st.step)
	}
}

// TestInitWizardApply_BearerFlow covers the bearer branch: the chat-path step
// appears (and keeps the standard default out of the config), a non-empty API
// key skips the token_cmd step, and NumAgents=0 skips agent-tools.
func TestInitWizardApply_BearerFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	st, _ := initWizardStart()
	opts := localWizardOpts(0)

	initWizardApply(st, "orb", opts)                          // name
	initWizardApply(st, "3", opts)                            // provider → bearer → URL
	initWizardApply(st, "https://openrouter.ai/api/v1", opts) // → chat path
	res := initWizardApply(st, "", opts)                      // default chat path
	if !strings.Contains(res.Output+res.Prompt, "model name") {
		t.Fatalf("after blank chat path: output+prompt = %q, want the model prompt", res.Output+res.Prompt)
	}
	res = initWizardApply(st, "meta-llama/llama-3.1-8b-instruct", opts) // → auth
	if !strings.Contains(res.Output+res.Prompt, "API key") {
		t.Fatalf("bearer must ask for an API key, output+prompt = %q", res.Output+res.Prompt)
	}
	res = initWizardApply(st, "sk-test", opts) // → limits (non-empty key skips token_cmd)
	if !strings.Contains(res.Output+res.Prompt, "context window") {
		t.Fatalf("after API key: output+prompt = %q, want the limits prompt (token_cmd must be skipped)", res.Output+res.Prompt)
	}
	initWizardApply(st, "", opts)        // limits
	res = initWizardApply(st, "y", opts) // escalation → commit
	if !res.Done || res.Committed == nil {
		t.Fatalf("result = %+v, want Done + committed", res)
	}
	cfg := *res.Committed
	if cfg.Agents[0].APIKey != "sk-test" {
		t.Errorf("APIKey = %q, want sk-test", cfg.Agents[0].APIKey)
	}
	if cfg.Agents[0].ChatPath != "" {
		t.Errorf("ChatPath = %q, want empty (standard default is not stored)", cfg.Agents[0].ChatPath)
	}
	if cfg.EscalationAgent != "claude" {
		t.Errorf("EscalationAgent = %q, want claude (answered y)", cfg.EscalationAgent)
	}
	if cfg.AgentTools != nil {
		t.Errorf("AgentTools = %v, want none (NumAgents=0 skips the step)", cfg.AgentTools)
	}
}

// TestInitWizardApply_ValidationReprompts: a rejected answer must not advance
// the state machine — the same step's prompt comes back.
func TestInitWizardApply_ValidationReprompts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	st, _ := initWizardStart()
	opts := localWizardOpts(1)

	initWizardApply(st, "", opts) // name → provider
	res := initWizardApply(st, "9", opts)
	if !strings.Contains(res.Output, "invalid choice") || res.Done {
		t.Errorf("bad provider: res = %+v, output = %q, want invalid-choice re-prompt", res, res.Output)
	}
	if st.step != initStepProvider {
		t.Errorf("st.step = %v, want initStepProvider (unchanged by the rejected answer)", st.step)
	}
	initWizardApply(st, "1", opts) // provider accepted → URL
	res = initWizardApply(st, "", opts)
	if !strings.Contains(res.Output, "URL is required") {
		t.Errorf("blank URL: output = %q, want URL-required error", res.Output)
	}
	if st.step != initStepURL {
		t.Errorf("st.step = %v, want initStepURL (unchanged)", st.step)
	}
}

// TestInitWizardApply_OpenEditorQuestionTUI: with OfferOpenEditor the config
// is written when the flow reaches the editor step, the question is asked, and
// only the answer to it ends the wizard (marking it for the TUI to open $EDITOR).
func TestInitWizardApply_OpenEditorQuestionTUI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	st, _ := initWizardStart()
	opts := initWizardOpts{NumAgents: 1, OfferOpenEditor: true}

	initWizardApply(st, "", opts)                      // name
	initWizardApply(st, "1", opts)                     // provider
	initWizardApply(st, "http://localhost:8080", opts) // URL
	initWizardApply(st, "", opts)                      // run_cmd
	initWizardApply(st, "m", opts)                     // model
	initWizardApply(st, "", opts)                      // limits
	res := initWizardApply(st, "n", opts)              // escalation → commit + editor question
	if res.Committed == nil {
		t.Fatalf("res = %+v, want the config committed at the editor step", res)
	}
	if res.Done {
		t.Error("wizard must not be Done before the editor question is answered")
	}
	if !strings.Contains(res.Prompt, "open config in editor now?") {
		t.Errorf("prompt = %q, want the open-editor question", res.Prompt)
	}
	res = initWizardApply(st, "y", opts)
	if !res.Done || !res.OpenEditor || res.Output != "" {
		t.Errorf("answer y = %+v, want Done+OpenEditor with no further output", res)
	}
}

// TestInitWizardCancelWord: the plain-text escape hatches ACP relies on
// (the TUI cancels with esc).
func TestInitWizardCancelWord(t *testing.T) {
	for _, w := range []string{"cancel", "CANCEL", " Quit ", "abort"} {
		if !initWizardCancelWord(w) {
			t.Errorf("initWizardCancelWord(%q) = false, want true", w)
		}
	}
	for _, w := range []string{"", "local", "http://localhost:8080", "n"} {
		if initWizardCancelWord(w) {
			t.Errorf("initWizardCancelWord(%q) = true, want false", w)
		}
	}
}

// TestInitAliasRegistration pins every surface /init must appear on: slash
// command extraction, tab-completion variants, help text, and the TUI dispatch
// that actually starts the wizard.
func TestInitAliasRegistration(t *testing.T) {
	cmd, rest, found := extractSlashCommand("/init")
	if !found || cmd != "/init" || rest != "" {
		t.Errorf("extractSlashCommand(/init) = (%q, %q, %v), want (/init, \"\", true)", cmd, rest, found)
	}
	if vs, ok := cmdVariants["/init"]; !ok || len(vs) == 0 {
		t.Errorf("cmdVariants[/init] = %v, want a variant from the help line", vs)
	}
	if !strings.Contains(interactiveHelp, "alias for /config init") {
		t.Error("interactiveHelp must document /init as the /config init alias")
	}

	m := layoutTestModel(t, 120, 40)
	updated, _ := m.handleSlashInput("/init", "")
	got := updated.(model)
	if got.pendingInit == nil {
		t.Fatal("TUI /init must start the setup wizard (pendingInit set)")
	}
	if !strings.Contains(got.transcript.String(), "setup wizard") {
		t.Errorf("transcript = %q, want the wizard banner", got.transcript.String())
	}
}

// TestConfigShowRendered: /config show goes through the shared renderer on
// both hosts — the no-local-config branch must still be valid fenced JSON.
func TestConfigShowRendered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// A config on disk so the render has something to show.
	if err := config.Save(config.Config{Agent: "x", Agents: []config.AgentConfig{{Name: "x", URL: "http://localhost:1", Model: "m", Provider: "local"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := renderConfigShow()
	if err != nil {
		t.Fatalf("renderConfigShow: %v", err)
	}
	if !strings.Contains(out, "```json") || !strings.Contains(out, "config") {
		t.Errorf("renderConfigShow = %q, want fenced JSON", out)
	}
	jout, err := renderConfigJSON()
	if err != nil {
		t.Fatalf("renderConfigJSON: %v", err)
	}
	if !strings.Contains(jout, "```json") || !strings.Contains(jout, `"x"`) {
		t.Errorf("renderConfigJSON = %q, want the agent rendered as JSON", jout)
	}
}

// TestInitWizardPicks: the clickable answers a host can offer per field —
// the blank answer as a one-click button (the empty turn's stand-in), titled
// choices for selects (closed set, default marked), one-click examples, and
// the typed escape. Secrets never carry a value as a pick.
func TestInitWizardPicks(t *testing.T) {
	labels := func(picks []initWizardPick) string {
		var s []string
		for _, p := range picks {
			s = append(s, p.Label+"="+p.Value)
		}
		return strings.Join(s, " | ")
	}
	// default-bearing text: the default is one click, plus the typed escape.
	if got := labels(initWizardPicks(initWizardField{Name: "n", Kind: "text", Default: "local"})); got != "use default — local= | type my own value…=" {
		t.Errorf("default text picks = %q", got)
	}
	// required text without a default: no skip button — examples are clicks.
	if got := labels(initWizardPicks(initWizardField{Name: "u", Kind: "text", Required: true, Examples: []string{"http://localhost:8080"}})); got != "http://localhost:8080=http://localhost:8080 | type my own value…=" {
		t.Errorf("required text picks = %q", got)
	}
	// select: closed set, default marked, no typed escape.
	if got := labels(initWizardPicks(initWizardField{Kind: "select", Default: "y", Options: []initWizardChoice{{"y", "yes"}, {"n", "no"}}})); got != "yes (default)=y | no=n" {
		t.Errorf("select picks = %q", got)
	}
	// secret: the value itself is never a pick — only skip vs type.
	if got := labels(initWizardPicks(initWizardField{Kind: "text", Secret: true})); got != "skip — no credential= | type my own value…=" {
		t.Errorf("secret picks = %q", got)
	}
	// multiselect: skip, enable-all (the comma form initWizardApply accepts),
	// and the typed escape.
	if got := labels(initWizardPicks(initWizardField{Kind: "multiselect", Options: []initWizardChoice{{"a", "a"}, {"b", "b"}}})); got != "skip — none= | enable all — a, b=a,b | type my own value…=" {
		t.Errorf("multiselect picks = %q", got)
	}
	// required text with nothing to offer: the typed escape alone — hosts
	// treat that as "nothing worth clicking".
	if got := initWizardPicks(initWizardField{Kind: "text", Required: true}); len(got) != 1 || !got[0].Custom {
		t.Errorf("bare required picks = %v, want the lone typed escape", got)
	}
}
