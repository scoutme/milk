package main

// Tests for the host-independent /config init wizard core (initwizard_core.go)
// and for /init's registration across the TUI surfaces (extraction, completion
// variants, help, dispatch) — the pattern TestNotificationsRegistration pins
// for /clear, applied to the wizard alias.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// localWizardOpts is the ACP-style option set (no editor question): the agent
// tools step is skipped for a config with at most one agent.
func localWizardOpts(numAgents int) initWizardOpts {
	return initWizardOpts{NumAgents: numAgents, OfferOpenEditor: false}
}

// stubWizardDiscovery fixes what the first-run scan finds and Claude Code's
// login state, so wizard tests never read the developer's real environment.
func stubWizardDiscovery(t *testing.T, found []initWizardFinding, claude claudeAuthState) {
	t.Helper()
	prevD, prevC := initWizardDiscoverFn, claudeAuthStatusFn
	initWizardDiscoverFn = func() []initWizardFinding { return found }
	claudeAuthStatusFn = func() claudeAuthState { return claude }
	t.Cleanup(func() { initWizardDiscoverFn, claudeAuthStatusFn = prevD, prevC })
}

// stubWizardModels fixes what the wizard's local-server probe finds.
func stubWizardModels(t *testing.T, models []string, err error) {
	t.Helper()
	stubWizardDiscovery(t, nil, claudeAuthState{})
	prev := initWizardListModelsFn
	initWizardListModelsFn = func(context.Context, string) ([]string, error) { return models, err }
	t.Cleanup(func() { initWizardListModelsFn = prev })
}

// pinClaudeOnPath makes the escalation question's default deterministic.
func pinClaudeOnPath(t *testing.T, found bool) {
	t.Helper()
	prev := claudeOnPath
	claudeOnPath = func() bool { return found }
	t.Cleanup(func() { claudeOnPath = prev })
}

// TestInitWizardApply_LocalProviderFlow drives the common first-run path
// (a local server) end to end and checks the prompt sequence, the final commit
// and that no step asks about opening an editor. The whole path needs only the
// provider, a model and the two defaults — no name, run_cmd or limits.
func TestInitWizardApply_LocalProviderFlow(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))
	pinClaudeOnPath(t, true)

	st, banner := initWizardStart("")
	if !strings.Contains(banner, "setup wizard") || !strings.Contains(banner, "primary") || !strings.Contains(banner, "applied at once") {
		t.Fatalf("banner = %q, want the setup banner explaining primary/escalation and hot reload", banner)
	}
	if p := initWizardPrompt(st); !strings.Contains(p, "Which AI should answer") {
		t.Fatalf("first prompt = %q, want the plain-language provider question", p)
	}

	steps := []struct {
		in, want string
	}{
		{"", "Where is it running"},            // blank provider → 1 (local)
		{"", "model name"},                     // blank URL → the local default
		{"qwen2.5-coder", "escalation agent?"}, // → escalation
		{"", "tune now"},                       // blank → Claude on PATH → yes; → tuning gate
		{"", "config written to"},              // blank → no tuning → commit + summary
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
	if cfg.Agent != "local" || len(cfg.Agents) != 2 {
		t.Errorf("cfg.Agent = %q, len(cfg.Agents) = %d, want primary %q + escalation", cfg.Agent, len(cfg.Agents), "local")
	}
	a := cfg.Agents[0]
	if a.URL != "http://localhost:11434" || a.Model != "qwen2.5-coder" || a.Provider != "local" {
		t.Errorf("committed agent = %+v", a)
	}
	if a.RunCmd != "" || a.ContextWindowTokens != 0 {
		t.Errorf("committed agent = %+v, want advanced knobs left unset when tuning is skipped", a)
	}
	if cfg.EscalationAgent != "claude" || len(cfg.AgentTools) > 0 {
		t.Errorf("escalation = %q, agent tools = %v, want claude and no tools", cfg.EscalationAgent, cfg.AgentTools)
	}
	if st.step != initStepDone {
		t.Errorf("st.step = %v, want initStepDone", st.step)
	}
}

// TestInitWizardApply_TuningGate: answering yes to the tuning gate opens the
// advanced knobs (run_cmd for local, context window, agent tools).
func TestInitWizardApply_TuningGate(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))

	st, _ := initWizardStart("")
	opts := localWizardOpts(2)
	for _, in := range []string{"1", "", "m", "n"} { // provider, url, model, escalation
		initWizardApply(st, in, opts)
	}
	res := initWizardApply(st, "y", opts) // tuning → yes
	if !strings.Contains(res.Prompt, "run_cmd") {
		t.Fatalf("tuning yes: prompt = %q, want the run_cmd question", res.Prompt)
	}
	res = initWizardApply(st, "llama-server -m x.gguf", opts)
	if !strings.Contains(res.Prompt, "context window") {
		t.Fatalf("after run_cmd: prompt = %q, want the context-window question", res.Prompt)
	}
	res = initWizardApply(st, "8192", opts)
	if !strings.Contains(res.Prompt, "agents as tools") && !strings.Contains(res.Prompt, "enable any agents") {
		t.Fatalf("after limits: prompt = %q, want the agent-tools question (2 agents configured)", res.Prompt)
	}
	res = initWizardApply(st, "", opts)
	if !res.Done || res.Committed == nil {
		t.Fatalf("result = %+v, want Done + committed", res)
	}
	a := res.Committed.Agents[0]
	if a.RunCmd != "llama-server -m x.gguf" || a.ContextWindowTokens != 8192 {
		t.Errorf("committed agent = %+v, want the tuned run_cmd and context window", a)
	}
}

// TestInitWizardApply_BearerFlow covers the bearer branch: the URL default
// and a trailing /v1 are normalized (no chat-path question — the standard path
// stays out of the config), a non-empty API key skips the token_cmd step, and
// a missing claude CLI makes the escalation default "no".
func TestInitWizardApply_BearerFlow(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))
	pinClaudeOnPath(t, false)

	st, _ := initWizardStart("")
	opts := localWizardOpts(0)

	initWizardApply(st, "2", opts) // provider → bearer → URL
	res := initWizardApply(st, "https://openrouter.ai/api/v1/", opts)
	if !strings.Contains(res.Output, "dropped the trailing /v1") || !strings.Contains(res.Prompt, "model name") {
		t.Fatalf("after URL: output+prompt = %q, want the /v1 note and the model prompt", res.Output+res.Prompt)
	}
	res = initWizardApply(st, "meta-llama/llama-3.1-8b-instruct", opts) // → auth
	if !strings.Contains(res.Output+res.Prompt, "API key") {
		t.Fatalf("bearer must ask for an API key, output+prompt = %q", res.Output+res.Prompt)
	}
	res = initWizardApply(st, "sk-test", opts) // → escalation (non-empty key skips token_cmd)
	if !strings.Contains(res.Prompt, "escalation agent?") || !strings.Contains(res.Prompt, "[y/N]") {
		t.Fatalf("after API key: prompt = %q, want the escalation question defaulting to no (claude CLI missing)", res.Prompt)
	}
	initWizardApply(st, "", opts)       // escalation → default (n)
	res = initWizardApply(st, "", opts) // tuning → skip → commit
	if !res.Done || res.Committed == nil {
		t.Fatalf("result = %+v, want Done + committed", res)
	}
	cfg := *res.Committed
	a := cfg.Agents[0]
	if a.APIKey != "sk-test" || a.URL != "https://openrouter.ai/api" || a.Name != "cloud" {
		t.Errorf("committed agent = %+v, want the key, the normalized URL and the provider-derived name", a)
	}
	if a.ChatPath != "" {
		t.Errorf("ChatPath = %q, want empty (standard default is not stored)", a.ChatPath)
	}
	if cfg.EscalationAgent != "" {
		t.Errorf("EscalationAgent = %q, want none (blank answer with no claude CLI)", cfg.EscalationAgent)
	}
}

// TestInitWizardApply_ClaudeCLIPrimarySkipsEscalation: a claude-cli primary is
// already Claude Code, so the escalation question is not asked.
func TestInitWizardApply_ClaudeCLIPrimarySkipsEscalation(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))

	st, _ := initWizardStart("")
	res := initWizardApply(st, "4", localWizardOpts(0))
	if !strings.Contains(res.Prompt, "tune now") {
		t.Fatalf("claude-cli primary: prompt = %q, want the tuning gate (no URL, model or escalation)", res.Prompt)
	}
	res = initWizardApply(st, "", localWizardOpts(0))
	if res.Committed == nil || res.Committed.EscalationAgent != "" || res.Committed.Agent != "claude" {
		t.Fatalf("result = %+v, want claude as the only agent with no escalation", res)
	}
}

// TestInitWizardApply_ValidationReprompts: a rejected answer must not advance
// the state machine — the same step's prompt comes back.
func TestInitWizardApply_ValidationReprompts(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))

	st, _ := initWizardStart("")
	opts := localWizardOpts(1)

	res := initWizardApply(st, "9", opts)
	if !strings.Contains(res.Output, "invalid choice") || res.Done {
		t.Errorf("bad provider: res = %+v, output = %q, want invalid-choice re-prompt", res, res.Output)
	}
	if st.step != initStepProvider {
		t.Errorf("st.step = %v, want initStepProvider (unchanged by the rejected answer)", st.step)
	}
	initWizardApply(st, "bedrock", opts) // provider by name → URL (no default for bedrock)
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
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))

	st, _ := initWizardStart("")
	opts := initWizardOpts{NumAgents: 1, OfferOpenEditor: true}

	initWizardApply(st, "1", opts)        // provider
	initWizardApply(st, "", opts)         // URL → default
	initWizardApply(st, "m", opts)        // model
	initWizardApply(st, "n", opts)        // escalation
	res := initWizardApply(st, "n", opts) // tuning → commit + editor question
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

// TestInitWizardSmokeCheck: success says the user is ready and names the first
// commands; a server that cannot be reached says what to fix, never panics.
func TestInitWizardSmokeCheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}
	}))
	defer ok.Close()

	cfg := config.InitConfig(config.AgentConfig{Name: "local", Provider: "local", URL: ok.URL, Model: "m"}, nil)
	got := initWizardSmokeCheck(context.Background(), cfg)
	if !strings.Contains(got, "you're ready") || !strings.Contains(got, "/help") || !strings.Contains(got, "/permissions") {
		t.Errorf("smoke success = %q, want the ready line and the first commands", got)
	}

	ok.Close()
	got = initWizardSmokeCheck(context.Background(), cfg)
	if !strings.Contains(got, "couldn't get an answer") || !strings.Contains(got, "/config init") {
		t.Errorf("smoke failure = %q, want an actionable error naming how to fix it", got)
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
	out, err := renderConfigShow("")
	if err != nil {
		t.Fatalf("renderConfigShow: %v", err)
	}
	if !strings.Contains(out, "```json") || !strings.Contains(out, "config") {
		t.Errorf("renderConfigShow = %q, want fenced JSON", out)
	}
	jout, err := renderConfigJSON("")
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

// TestInitWizardApply_GuidedPicks: the URL and model questions are pick lists,
// not blanks — a number chooses a preset URL, the server's own models are
// offered when it answers, and every question carries its step counter.
func TestInitWizardApply_GuidedPicks(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, []string{"qwen2.5-coder:7b", "llama3.1:8b"}, nil)

	st, _ := initWizardStart("")
	opts := localWizardOpts(1)
	if p := initWizardPrompt(st); !strings.Contains(p, "step 1") || !strings.Contains(p, "Ollama") {
		t.Fatalf("provider prompt = %q, want the step counter and the examples beside the choices", p)
	}

	res := initWizardApply(st, "1", opts)
	for _, want := range []string{"step 2/5", "1) Ollama", "2) llama.cpp", "3) LM Studio"} {
		if !strings.Contains(res.Prompt, want) {
			t.Errorf("URL prompt = %q, want %q", res.Prompt, want)
		}
	}

	res = initWizardApply(st, "2", opts) // pick llama.cpp by number
	if st.primary.URL != "http://localhost:8080" {
		t.Errorf("URL = %q, want the llama.cpp preset", st.primary.URL)
	}
	if !strings.Contains(res.Output, "reached it") {
		t.Errorf("output = %q, want the probe verdict", res.Output)
	}
	for _, want := range []string{"step 3/5", "1) qwen2.5-coder:7b", "2) llama3.1:8b", "found on your server"} {
		if !strings.Contains(res.Prompt, want) {
			t.Errorf("model prompt = %q, want %q", res.Prompt, want)
		}
	}

	initWizardApply(st, "2", opts) // pick the second discovered model
	if st.primary.Model != "llama3.1:8b" {
		t.Errorf("Model = %q, want the second discovered model", st.primary.Model)
	}
}

// TestInitWizardApply_ProbeFailureKeepsGoing: a server that is not up yet is
// reported, never blocks the wizard, and the model question falls back to text.
func TestInitWizardApply_ProbeFailureKeepsGoing(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("connection refused"))

	st, _ := initWizardStart("")
	initWizardApply(st, "1", localWizardOpts(1))
	res := initWizardApply(st, "", localWizardOpts(1))
	if !strings.Contains(res.Output, "nothing answered") || !strings.Contains(res.Prompt, "model name") {
		t.Errorf("output+prompt = %q, want the unreachable note and the plain model question", res.Output+res.Prompt)
	}
}

// TestConfigInitHyphenAlias: "/config-init" starts the guided setup like
// "/config init" — it must not fall through as plain text to a model.
func TestConfigInitHyphenAlias(t *testing.T) {
	cmd, rest, found := extractSlashCommand("/config-init")
	if !found || cmd != "/config" || rest != "init" {
		t.Errorf("extractSlashCommand(/config-init) = (%q, %q, %v), want (/config, init, true)", cmd, rest, found)
	}
}
