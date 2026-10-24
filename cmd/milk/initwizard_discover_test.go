package main

// Tests for the first-run credential discovery (#320): what the wizard offers
// as its opening question, how a pick is applied without copying a secret into
// the config, and Claude Code's browser login step.

import (
	"errors"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func TestInitWizardMask(t *testing.T) {
	if got := initWizardMask("sk-or-v1-abcdef123456"); got != "sk-o…456" {
		t.Errorf("long key mask = %q, want prefix…suffix", got)
	}
	if got := initWizardMask("short"); got != "5 chars" || strings.Contains(got, "short") {
		t.Errorf("short key mask = %q, want only the length", got)
	}
}

func TestInitWizardDiscover_EnvKeysAreMaskedAndOrdered(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-abcdef123456")
	t.Setenv("GROQ_API_KEY", "")
	stubWizardModels(t, nil, errors.New("no server"))
	stubWizardDiscovery(t, nil, claudeAuthState{Installed: true, LoggedIn: true, Account: "me@example.com"})
	initWizardDiscoverFn = initWizardDiscover // the real scan, over stubbed probes

	found := initWizardDiscoverFn()
	if len(found) < 2 || found[0].Label != "Claude Code" || !strings.Contains(found[0].Detail, "signed in as me@example.com") {
		t.Fatalf("found = %+v, want a signed-in Claude Code first", found)
	}
	var or *initWizardFinding
	for i := range found {
		if found[i].KeyEnv == "OPENROUTER_API_KEY" {
			or = &found[i]
		}
		if found[i].KeyEnv == "GROQ_API_KEY" {
			t.Errorf("an empty variable must not be offered: %+v", found[i])
		}
	}
	if or == nil {
		t.Fatalf("found = %+v, want the OpenRouter key", found)
	}
	if strings.Contains(or.Detail, "abcdef123456") || !strings.Contains(or.Detail, "OPENROUTER_API_KEY") {
		t.Errorf("detail = %q, want the variable name and a masked fingerprint, never the key", or.Detail)
	}
}

func TestInitWizardFlow_PickingAFoundKeyWritesAnEnvPointer(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))
	stubWizardDiscovery(t, []initWizardFinding{
		{Name: "openrouter", Label: "OpenRouter", Provider: "bearer", URL: "https://openrouter.ai/api",
			KeyEnv: "OPENROUTER_API_KEY", Model: "qwen/qwen-2.5-coder-32b-instruct", Detail: "OPENROUTER_API_KEY (sk-o…456)"},
	}, claudeAuthState{})
	pinClaudeOnPath(t, false)

	st, banner := initWizardStart("")
	if strings.Contains(banner, "nothing found") {
		t.Errorf("banner = %q, must not claim nothing was found", banner)
	}
	p := initWizardPrompt(st)
	for _, want := range []string{"found these", "1) ", "OpenRouter", "OPENROUTER_API_KEY", "2) ", "None of these"} {
		if !strings.Contains(p, want) {
			t.Errorf("first prompt = %q, want %q", p, want)
		}
	}

	opts := localWizardOpts(0)
	res := initWizardApply(st, "", opts) // Enter = the first finding
	if !strings.Contains(res.Output, "stays in $OPENROUTER_API_KEY") || !strings.Contains(res.Prompt, "model name") {
		t.Fatalf("after the pick: %q + %q, want the env-pointer note and the model question", res.Output, res.Prompt)
	}
	if !strings.Contains(res.Prompt, "qwen/qwen-2.5-coder-32b-instruct") {
		t.Errorf("model prompt = %q, want the suggested model as the default", res.Prompt)
	}
	res = initWizardApply(st, "", opts) // model → default; no API key question
	if strings.Contains(res.Prompt, "API key") || !strings.Contains(res.Prompt, "escalation agent?") {
		t.Fatalf("after the model: %q, want escalation (the key question is skipped)", res.Prompt)
	}
	initWizardApply(st, "", opts) // escalation → default n (no claude CLI)
	res = initWizardApply(st, "", opts)
	if res.Committed == nil {
		t.Fatalf("result = %+v, want the config committed", res)
	}
	a := res.Committed.Agents[0]
	if a.APIKey != "" || a.APIKeyEnv != "OPENROUTER_API_KEY" || a.Model != "qwen/qwen-2.5-coder-32b-instruct" || a.Name != "openrouter" {
		t.Errorf("committed agent = %+v, want an env pointer, no literal key", a)
	}
}

func TestInitWizardFlow_NothingFoundSaysSoAndAsksNormally(t *testing.T) {
	stubWizardModels(t, nil, errors.New("no server"))
	st, banner := initWizardStart("")
	if !strings.Contains(banner, "nothing found") || !strings.Contains(banner, "OPENROUTER_API_KEY") {
		t.Errorf("banner = %q, want the nothing-found note naming what was searched", banner)
	}
	if st.step != initStepProvider {
		t.Errorf("step = %v, want the provider question", st.step)
	}
}

func TestInitWizardFlow_NoneOfTheseFallsToTheProviderMenu(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))
	stubWizardDiscovery(t, []initWizardFinding{{Name: "ollama", Label: "Ollama", Provider: "local", URL: "http://localhost:11434", Detail: "running"}}, claudeAuthState{})
	st, _ := initWizardStart("")
	res := initWizardApply(st, "2", localWizardOpts(0)) // 1 finding → 2 = none of these
	if !strings.Contains(res.Prompt, "Which AI should answer") {
		t.Errorf("prompt = %q, want the provider menu", res.Prompt)
	}
	if res = initWizardApply(st, "9", localWizardOpts(0)); st.step != initStepProvider {
		t.Errorf("bad provider answer moved the step to %v", st.step)
	}
	_ = res
}

func TestInitWizardFlow_ClaudeLoginIsOfferedWhenSignedOut(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))
	stubWizardDiscovery(t, nil, claudeAuthState{Installed: true, LoggedIn: false})

	st, _ := initWizardStart("")
	opts := localWizardOpts(0)
	res := initWizardApply(st, "4", opts) // Claude Code
	if !strings.Contains(res.Prompt, "isn't signed in") || !strings.Contains(res.Prompt, "browser") {
		t.Fatalf("prompt = %q, want the browser-login question", res.Prompt)
	}
	res = initWizardApply(st, "", opts) // Enter = yes
	if !res.RunClaudeLogin {
		t.Error("accepting must ask the host to run the Claude Code login")
	}
	if !strings.Contains(res.Prompt, "tune now") {
		t.Errorf("prompt = %q, want the flow to continue (escalation is skipped for a claude-cli primary)", res.Prompt)
	}
}

func TestInitWizardFlow_ClaudeLoginDeclinedOrSignedIn(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))

	stubWizardDiscovery(t, nil, claudeAuthState{Installed: true, LoggedIn: false})
	st, _ := initWizardStart("")
	initWizardApply(st, "4", localWizardOpts(0))
	res := initWizardApply(st, "n", localWizardOpts(0))
	if res.RunClaudeLogin || !strings.Contains(res.Output, "claude auth login") {
		t.Errorf("declined: %+v, want no login and a how-to note", res)
	}

	stubWizardDiscovery(t, nil, claudeAuthState{Installed: true, LoggedIn: true})
	st, _ = initWizardStart("")
	res = initWizardApply(st, "4", localWizardOpts(0))
	if strings.Contains(res.Prompt, "signed in yet") || !strings.Contains(res.Prompt, "tune now") {
		t.Errorf("signed in: prompt = %q, want the login question skipped", res.Prompt)
	}
}

func TestInitWizardFlow_EscalationToSignedOutClaudeOffersLogin(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))
	stubWizardDiscovery(t, nil, claudeAuthState{Installed: true, LoggedIn: false})

	st, _ := initWizardStart("")
	opts := localWizardOpts(0)
	initWizardApply(st, "1", opts)
	initWizardApply(st, "", opts)
	initWizardApply(st, "m", opts)
	res := initWizardApply(st, "y", opts) // escalation → Claude Code, signed out
	if !strings.Contains(res.Prompt, "isn't signed in") {
		t.Fatalf("prompt = %q, want the login question after choosing Claude as escalation", res.Prompt)
	}
}

func TestResolvedAPIKey(t *testing.T) {
	t.Setenv("MILK_TEST_KEY", "  from-env  ")
	if got := (config.AgentConfig{APIKeyEnv: "MILK_TEST_KEY"}).ResolvedAPIKey(); got != "from-env" {
		t.Errorf("env pointer = %q, want the trimmed variable value", got)
	}
	if got := (config.AgentConfig{APIKey: "literal", APIKeyEnv: "MILK_TEST_KEY"}).ResolvedAPIKey(); got != "literal" {
		t.Errorf("literal key = %q, want it to win over the pointer", got)
	}
	if got := (config.AgentConfig{APIKeyEnv: "MILK_UNSET_VAR_XYZ"}).ResolvedAPIKey(); got != "" {
		t.Errorf("unset pointer = %q, want empty", got)
	}
}
