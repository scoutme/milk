package main

// Credential discovery for the /config init wizard: before asking anything,
// look (read-only) at what the machine already has — provider API keys in the
// environment, local model servers that are up, and the Claude Code CLI with
// its login state — and offer those as the first, one-keystroke choice.
//
// Secrets never leave this file as values: a finding carries the variable's
// NAME and a masked fingerprint; accepting one writes `api_key_env` (a pointer
// to the variable) into the config, not the key itself.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// initWizardFinding is one thing discovery found that the user can pick.
type initWizardFinding struct {
	Name     string           // agent name written to the config ("openrouter", "ollama", "claude")
	Label    string           // what the user sees ("OpenRouter")
	Detail   string           // where/how it was found, never a secret value
	Provider string           // local | bearer | claude-cli
	URL      string           // endpoint ("" for claude-cli)
	KeyEnv   string           // env var holding the key (bearer findings)
	Model    string           // suggested model ("" = ask)
	Models   []string         // models the server reported (local findings)
	Claude   *claudeAuthState // Claude Code's login state (claude-cli findings)
}

// initWizardKeySource describes one provider whose key is commonly exported.
type initWizardKeySource struct {
	Env, Name, Label, URL, Model, KeysPage string
}

// initWizardKeySources are the environment variables the scan looks for. The
// URL is the provider's OpenAI-compatible base (milk adds /v1/chat/completions).
var initWizardKeySources = []initWizardKeySource{
	{"OPENROUTER_API_KEY", "openrouter", "OpenRouter", "https://openrouter.ai/api", "qwen/qwen-2.5-coder-32b-instruct", "https://openrouter.ai/keys"},
	{"GROQ_API_KEY", "groq", "Groq", "https://api.groq.com/openai", "qwen-qwq-32b", "https://console.groq.com/keys"},
	{"TOGETHER_API_KEY", "together", "Together", "https://api.together.xyz", "Qwen/Qwen2.5-Coder-32B-Instruct", "https://api.together.xyz/settings/api-keys"},
	{"DEEPSEEK_API_KEY", "deepseek", "DeepSeek", "https://api.deepseek.com", "deepseek-chat", "https://platform.deepseek.com/api_keys"},
	{"OPENAI_API_KEY", "openai", "OpenAI", "https://api.openai.com", "gpt-4o-mini", "https://platform.openai.com/api-keys"},
	{"ANTHROPIC_API_KEY", "anthropic", "Anthropic API", "https://api.anthropic.com", "claude-haiku-4-5", "https://console.anthropic.com/settings/keys"},
}

// initWizardKeysPageFor returns the page where a user creates a key for the
// service at url ("" when unknown), for the "no key yet" hint.
func initWizardKeysPageFor(url string) string {
	for _, k := range initWizardKeySources {
		if strings.Contains(url, strings.TrimPrefix(k.URL, "https://")) {
			return k.KeysPage
		}
	}
	return ""
}

// initWizardMask is the fingerprint shown instead of a key: a short prefix and
// suffix, or just the length when the key is too short to show any of it.
func initWizardMask(key string) string {
	if len(key) < 14 {
		return fmt.Sprintf("%d chars", len(key))
	}
	return key[:4] + "…" + key[len(key)-3:]
}

// claudeAuthState is what `claude auth status` reports.
type claudeAuthState struct {
	Installed bool
	LoggedIn  bool
	Account   string // email or org, when the CLI reports one
}

// claudeAuthStatusFn reads the Claude Code CLI's login state — a var so tests
// don't shell out.
var claudeAuthStatusFn = claudeAuthStatus

func claudeAuthStatus() claudeAuthState {
	path, err := exec.LookPath("claude")
	if err != nil {
		return claudeAuthState{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	// A logged-out CLI exits non-zero but still prints the JSON: parse first.
	out, _ := exec.CommandContext(ctx, path, "auth", "status", "--json").Output()
	var st struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
		OrgName  string `json:"orgName"`
	}
	_ = json.Unmarshal(out, &st)
	acct := st.Email
	if acct == "" {
		acct = st.OrgName
	}
	return claudeAuthState{Installed: true, LoggedIn: st.LoggedIn, Account: acct}
}

// initWizardDiscoverFn runs the scan — a var so wizard tests are hermetic.
var initWizardDiscoverFn = initWizardDiscover

// initWizardDiscover scans the machine read-only and returns what it found,
// best first: a signed-in Claude Code, running local servers, exported keys.
func initWizardDiscover() []initWizardFinding {
	var (
		wg      sync.WaitGroup
		claude  claudeAuthState
		servers = make([]*initWizardFinding, len(initWizardURLPresets("local")))
	)
	wg.Add(1)
	go func() { defer wg.Done(); claude = claudeAuthStatusFn() }()
	for i, p := range initWizardURLPresets("local") {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
			defer cancel()
			models, err := initWizardListModelsFn(ctx, p.URL)
			if err != nil {
				return
			}
			port := p.URL[strings.LastIndex(p.URL, ":"):]
			servers[i] = &initWizardFinding{
				Name: strings.ToLower(strings.ReplaceAll(p.Name, " ", "")), Label: p.Name,
				Detail:   fmt.Sprintf("running on %s · %d model(s)", port, len(models)),
				Provider: "local", URL: p.URL, Models: capModels(models),
			}
		}()
	}
	wg.Wait()

	var found []initWizardFinding
	switch {
	case claude.Installed && claude.LoggedIn:
		found = append(found, initWizardFinding{Name: "claude", Label: "Claude Code", Provider: "claude-cli", Claude: &claude,
			Detail: "signed in" + accountSuffix(claude.Account) + " — no API key needed"})
	case claude.Installed:
		found = append(found, initWizardFinding{Name: "claude", Label: "Claude Code", Provider: "claude-cli", Claude: &claude,
			Detail: "installed, not signed in — I can open your browser to log you in"})
	}
	for _, s := range servers {
		if s != nil {
			found = append(found, *s)
		}
	}
	for _, k := range initWizardKeySources {
		if v := strings.TrimSpace(os.Getenv(k.Env)); v != "" {
			found = append(found, initWizardFinding{Name: k.Name, Label: k.Label, Provider: "bearer",
				URL: k.URL, KeyEnv: k.Env, Model: k.Model,
				Detail: fmt.Sprintf("%s (%s)", k.Env, initWizardMask(v))})
		}
	}
	if len(found) > initWizardMaxFindings {
		found = found[:initWizardMaxFindings]
	}
	return found
}

// initWizardMaxFindings keeps the opening question short: the best few, in
// order (Claude Code, running servers, then exported keys).
const initWizardMaxFindings = 4

func capModels(m []string) []string {
	if len(m) > initWizardMaxModelChoices {
		return m[:initWizardMaxModelChoices]
	}
	return m
}

func accountSuffix(a string) string {
	if a == "" {
		return ""
	}
	return " as " + a
}

// initWizardSearchedNames lists what the scan looks for, for the "nothing
// found" message.
func initWizardSearchedNames() string {
	var names []string
	for _, k := range initWizardKeySources {
		names = append(names, k.Env)
	}
	return strings.Join(names[:3], ", ") + ", …"
}

// runClaudeLogin signs the user in to Claude Code with its own browser flow
// (`claude auth login`), for hosts without a terminal to hand over (ACP). The
// CLI opens the browser itself. Returns a one-line verdict for the transcript.
func runClaudeLogin(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "auth", "login").CombinedOutput()
	if err != nil {
		tail := strings.TrimSpace(string(out))
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return fmt.Sprintf("%s login did not finish (%v) %s\n  run `claude auth login` yourself, then /config init again\n", milkTag(), err, tail)
	}
	return milkTag() + " " + green("✓ signed in to Claude Code") + "\n"
}
