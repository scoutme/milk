package main

// Question flow and wording of the /config init wizard — the part a first-time
// user actually reads. initwizard_core.go owns the answer handling; this file
// owns which question comes next (initWizardNextStep), what it says
// (initWizardPrompt) and the safe defaults a blank answer stands for. The
// shape follows other first-run setups (Claude Code, Codex): one decision per
// screen, plain words, an accept-blindly default, and a single gate in front
// of everything optional.

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/scoutme/milk/internal/modelsdev"
)

// initWizardNextStep advances past the current step based on what fields are
// needed. The first-run path is provider → connection → model → credentials →
// escalation, then one optional-tuning gate; the advanced knobs (run_cmd,
// context window, agents-as-tools) only come up when the user opts in.
func initWizardNextStep(st *initWizardState) initWizardStep {
	p := st.primary.Provider
	switch st.step {
	case initStepFound:
		if !st.foundPicked {
			return initStepProvider
		}
		fallthrough
	case initStepProvider:
		if p == "claude-cli" && initWizardNeedsLogin(st) {
			return initStepClaudeLogin
		}
		if initWizardNeedsURL(p) && st.primary.URL == "" {
			return initStepURL
		}
		if initWizardNeedsModel(p) {
			return initStepModel
		}
		return initStepEscalation
	case initStepURL:
		return initStepModel
	case initStepModel:
		if initWizardNeedsAuth(p) && st.primary.APIKeyEnv == "" {
			return initStepAuth
		}
		if initWizardNeedsAWSRegion(p) {
			return initStepAWSRegion
		}
		return initStepEscalation
	case initStepAuth:
		// blank api_key → go ask for token_cmd instead
		if st.primary.APIKey == "" && st.primary.Headers["api-key"] == "" {
			return initStepTokenCmd
		}
		if initWizardNeedsAWSRegion(p) {
			return initStepAWSRegion
		}
		return initStepEscalation
	case initStepTokenCmd:
		if initWizardNeedsAWSRegion(p) {
			return initStepAWSRegion
		}
		return initStepEscalation
	case initStepAWSRegion:
		return initStepEscalation
	case initStepClaudeLogin:
		if p == "claude-cli" {
			return initStepEscalation // skipped for a claude-cli primary → tuning
		}
		return initStepTuning
	case initStepEscalation:
		if st.escCLI && initWizardNeedsLogin(st) {
			return initStepClaudeLogin
		}
		return initStepTuning
	case initStepTuning:
		if !st.tuning {
			return initStepOpenConfig
		}
		if p == "local" || p == "" {
			return initStepRunCmd
		}
		return initStepLimits
	case initStepRunCmd:
		return initStepLimits
	case initStepLimits:
		return initStepAgentTools
	case initStepAgentTools:
		return initStepOpenConfig
	}
	return initStepDone
}

// claudeOnPath reports whether the Claude Code CLI is installed — it decides
// the escalation question's safe default (a missing binary must not default to
// an escalation target that cannot run). A var so tests can pin it.
var claudeOnPath = func() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

// initWizardEscalationDefault is the escalation answer a blank reply means.
func initWizardEscalationDefault() string {
	if claudeOnPath() {
		return "y"
	}
	return "n"
}

// initWizardURLPreset is one well-known server address offered as a numbered
// shortcut (and as a one-click example over ACP).
type initWizardURLPreset struct{ Name, URL string }

// initWizardURLPresets lists the common addresses for a provider, most likely
// first — the first one is what a blank answer means. A wrong guess is caught
// by the server probe right after, and by the post-setup smoke check.
func initWizardURLPresets(provider string) []initWizardURLPreset {
	switch provider {
	case "local":
		return []initWizardURLPreset{
			{"Ollama", "http://localhost:11434"},
			{"llama.cpp", "http://localhost:8080"},
			{"LM Studio", "http://localhost:1234"},
		}
	case "bearer":
		return []initWizardURLPreset{
			{"OpenRouter", "https://openrouter.ai/api"},
			{"Groq", "https://api.groq.com/openai"},
			{"Together", "https://api.together.xyz"},
		}
	}
	return nil
}

// initWizardURLDefault is the URL a blank answer means ("" = the URL is
// required).
func initWizardURLDefault(provider string) string {
	if ps := initWizardURLPresets(provider); len(ps) > 0 {
		return ps[0].URL
	}
	return ""
}

// initWizardProviderDefaultName names the primary agent after its provider —
// the wizard no longer asks, since a newcomer has no reason to pick a name.
func initWizardProviderDefaultName(provider string) string {
	switch provider {
	case "bearer":
		return "cloud"
	case "claude-cli":
		return "claude"
	case "aider-cli":
		return "aider"
	}
	return provider
}

// initWizardProviderMenu is the single provider menu: Title is the choice,
// Examples what it covers (drawn next to the title in another color).
var initWizardProviderMenu = []struct{ Value, Title, Examples string }{
	{"local", "A model on this computer", "Ollama · llama.cpp · LM Studio · vLLM"},
	{"bearer", "A cloud API key", "OpenRouter · Groq · Together · Copilot · any OpenAI-compatible API"},
	{"bedrock", "AWS Bedrock", ""},
	{"claude-cli", "Claude Code", "uses the claude CLI you already have"},
	{"aider-cli", "aider", "advanced"},
	{"subprocess", "Custom subprocess", "advanced"},
}

// initWizardMainProviders is how many menu entries the typed prompt shows; the
// rest (aider, a custom subprocess) are accepted by name or number but kept off
// the screen so a first-timer sees four choices, not six.
const initWizardMainProviders = 4

// initWizardRule is the thin separator the TUI draws before each question, so
// one question reads apart from the answer echo and the text above it.
func initWizardRule() string {
	return "\n" + dim(strings.Repeat("─", 56)) + "\n"
}

// initWizardProviderChoices is the same menu flattened for the ACP form flow.
var initWizardProviderChoices = func() []initWizardChoice {
	var cs []initWizardChoice
	for _, m := range initWizardProviderMenu {
		label := m.Title
		if m.Examples != "" {
			label += " — " + m.Examples
		}
		cs = append(cs, initWizardChoice{m.Value, label})
	}
	return cs
}()

// initWizardExample draws example text next to a choice: a color of its own,
// so the decision (bold) and what it covers (cyan) read apart at a glance.
func initWizardExample(s string) string { return colorize(s, ansiCyan) }

// initWizardClaude is the Claude Code login state, read once per wizard run.
func initWizardClaude(st *initWizardState) claudeAuthState {
	if st.claude == nil {
		s := claudeAuthStatusFn()
		st.claude = &s
	}
	return *st.claude
}

// initWizardNeedsLogin reports whether Claude Code is installed but signed
// out, and the login has not been offered yet.
func initWizardNeedsLogin(st *initWizardState) bool {
	c := initWizardClaude(st)
	return c.Installed && !c.LoggedIn && !st.loginDone
}

// initWizardTake applies a discovered finding as the primary agent and
// returns the confirmation line. A key is only ever referenced by its
// variable name (api_key_env) — its value is never read into the config.
func initWizardTake(st *initWizardState, f initWizardFinding) string {
	st.foundPicked = true
	st.primary.Provider = f.Provider
	st.primary.Name = f.Name
	st.primary.URL = f.URL
	st.primary.APIKeyEnv = f.KeyEnv
	st.modelDefault = f.Model
	st.modelChoices = f.Models
	if f.Claude != nil {
		st.claude = f.Claude
	}
	switch {
	case f.KeyEnv != "":
		return green("  ✓ using "+f.Label) + dim(" — the key stays in $"+f.KeyEnv+"; nothing secret is copied into config.json") + "\n"
	case f.Provider == "claude-cli":
		return green("  ✓ using Claude Code") + "\n"
	}
	return green("  ✓ using "+f.Label+" at "+f.URL) + "\n"
}

// initWizardPath lists the questions a provider's first-run path asks, in
// order — the denominator of the "step n/m" counter. Optional tuning questions
// are not counted (they only appear on request).
func initWizardPath(st *initWizardState) []initWizardStep {
	provider := st.primary.Provider
	var path []initWizardStep
	if len(st.found) > 0 {
		path = append(path, initStepFound)
	}
	if !st.foundPicked {
		path = append(path, initStepProvider)
	}
	if initWizardNeedsURL(provider) && !st.foundPicked {
		path = append(path, initStepURL)
	}
	if initWizardNeedsModel(provider) {
		path = append(path, initStepModel)
	}
	if initWizardNeedsAuth(provider) && st.primary.APIKeyEnv == "" {
		path = append(path, initStepAuth)
	}
	if initWizardNeedsAWSRegion(provider) {
		path = append(path, initStepAWSRegion)
	}
	if c := st.claude; c != nil && c.Installed && !c.LoggedIn && (!st.loginDone || st.step == initStepClaudeLogin) {
		path = append(path, initStepClaudeLogin)
	}
	if provider != "claude-cli" {
		path = append(path, initStepEscalation)
	}
	return append(path, initStepTuning)
}

// initWizardStepLabel is the dim "step n/m" prefix of a question ("" for the
// optional tuning follow-ups; no total before the provider is known).
func initWizardStepLabel(st *initWizardState) string {
	step := st.step
	if step == initStepTokenCmd {
		step = initStepAuth // the token command is the other half of the credential question
	}
	path := initWizardPath(st)
	for i, s := range path {
		if s == step {
			if st.primary.Provider == "" {
				return boldGold(fmt.Sprintf("step %d", i+1)) + dim("  ·  ") // the path is not known until a provider is
			}
			return boldGold(fmt.Sprintf("step %d/%d", i+1, len(path))) + dim("  ·  ")
		}
	}
	return ""
}

// initWizardProviderFromAnswer maps a menu number or provider name to the
// provider value ("" = not a choice).
func initWizardProviderFromAnswer(answer string) string {
	answer = strings.ToLower(strings.TrimSpace(answer))
	for i, c := range initWizardProviderChoices {
		if answer == fmt.Sprint(i+1) || answer == c.Value {
			return c.Value
		}
	}
	return ""
}

// initWizardPrompt returns the prompt text for a given wizard step.
func initWizardPrompt(st *initWizardState) string {
	lbl := initWizardStepLabel(st)
	switch st.step {
	case initStepFound:
		var b strings.Builder
		b.WriteString(milkTag() + " " + lbl + bold("I looked around your computer and found these — use one?") + "\n")
		for i, f := range st.found {
			fmt.Fprintf(&b, "  %d) %s %s\n", i+1, bold(fmt.Sprintf("%-14s", f.Label)), initWizardExample(f.Detail))
		}
		fmt.Fprintf(&b, "  %d) %s\n", len(st.found)+1, bold("None of these")+dim(" — let me choose"))
		b.WriteString(milkTag() + " choice " + dim("[1]") + ": ")
		return b.String()
	case initStepClaudeLogin:
		return milkTag() + " " + lbl + bold("Claude Code isn't signed in yet. Open your browser to log in now?") + "\n" +
			dim("         It uses your Claude account (subscription or Console) — no API key to find or copy.") + "\n" +
			milkTag() + " log in now? " + dim("[Y/n]") + ": "
	case initStepProvider:
		var b strings.Builder
		b.WriteString(milkTag() + " " + lbl + bold("Which AI should answer your everyday prompts?") + "\n")
		b.WriteString(dim("         This is your \"primary\": the fast, cheap one that handles most prompts.") + "\n")
		for i, m := range initWizardProviderMenu[:initWizardMainProviders] {
			// Pad before styling: escape codes would break %-Ns alignment.
			if m.Examples == "" {
				fmt.Fprintf(&b, "  %d) %s", i+1, bold(m.Title))
			} else {
				fmt.Fprintf(&b, "  %d) %s  %s", i+1, bold(fmt.Sprintf("%-26s", m.Title)), initWizardExample(m.Examples))
			}
			b.WriteString("\n")
		}
		b.WriteString(dim("  other: type aider or subprocess") + "\n")
		b.WriteString(milkTag() + " choice " + dim("[1]") + ": ")
		return b.String()
	case initStepURL:
		presets := initWizardURLPresets(st.primary.Provider)
		if len(presets) == 0 {
			return milkTag() + " " + lbl + bold("Server URL") + ": "
		}
		q := "Where is it running?"
		if st.primary.Provider == "bearer" {
			q = "Which service?"
		}
		var b strings.Builder
		b.WriteString(milkTag() + " " + lbl + bold(q) + "\n")
		for i, p := range presets {
			def := ""
			if i == 0 {
				def = dim("  (default)")
			}
			fmt.Fprintf(&b, "  %d) %s %s%s\n", i+1, bold(fmt.Sprintf("%-10s", p.Name)), initWizardExample(p.URL), def)
		}
		b.WriteString(dim("  …or type your own address (a Copilot or Azure URL works too)") + "\n")
		b.WriteString(milkTag() + " choice or URL " + dim("[1]") + ": ")
		return b.String()
	case initStepModel:
		if len(st.modelChoices) > 0 {
			var b strings.Builder
			b.WriteString(milkTag() + " " + lbl + bold("Which model should it use?") + "  " + green("✓ found on your server") + "\n")
			for i, m := range st.modelChoices {
				def := ""
				if i == 0 {
					def = dim("  (default)")
				}
				fmt.Fprintf(&b, "  %d) %s%s\n", i+1, initWizardExample(m), def)
			}
			b.WriteString(dim("  …or type another model name") + "\n")
			b.WriteString(milkTag() + " choice or model name " + dim("[1]") + ": ")
			return b.String()
		}
		hint := ""
		switch {
		case isCopilotURL(st.primary.URL):
			hint = "e.g. claude-sonnet-4.6 · gpt-4o"
		case isAzureURL(st.primary.URL):
			hint = "Azure deployment name, e.g. gpt-4.1"
		case st.primary.Provider == "bearer":
			hint = "e.g. qwen/qwen-2.5-coder-32b-instruct"
		case st.primary.Provider == "bedrock":
			hint = "model ARN"
		case st.primary.Provider == "aider-cli":
			hint = "e.g. gpt-4o · claude-3-5-sonnet-20241022"
		case st.primary.Provider == "subprocess":
			hint = "forwarded to the subprocess via --model"
		case st.primary.Provider == "local":
			hint = "the one you pulled or loaded, e.g. qwen2.5-coder"
		}
		out := milkTag() + " " + lbl + bold("Which model should it use?") + "\n"
		if hint != "" {
			out += "  " + initWizardExample(hint) + "\n"
		}
		def := ""
		if st.modelDefault != "" {
			def = " " + dim("["+st.modelDefault+"]")
		}
		return out + milkTag() + " model name" + def + ": "
	case initStepAuth:
		hint := ""
		if isAzureURL(st.primary.URL) {
			hint = "\n" + dim("  hint: Azure uses an 'api-key' header, not Bearer — paste your Azure API key here")
		} else if isCopilotURL(st.primary.URL) {
			if host := copilotHostname(st.primary.URL); host != "" {
				hint = "\n" + dim(fmt.Sprintf("  hint: leave blank and use token_cmd = 'gh auth token --hostname %s'", host))
			} else {
				hint = "\n" + dim("  hint: leave blank and use token_cmd = 'gh auth token'")
			}
		}
		if page := initWizardKeysPageFor(st.primary.URL); page != "" {
			hint += "\n" + dim("  no key yet? create one at "+page)
		}
		return milkTag() + " " + lbl + bold("API key") + dim(" — saved only in your config.json, never sent to the model (leave blank or type 'default' to use a token command instead)") + hint + "\n" + milkTag() + " API key: "
	case initStepTokenCmd:
		hint := " (e.g. 'gh auth token' or 'op read op://vault/item/field')"
		if isCopilotURL(st.primary.URL) {
			if host := copilotHostname(st.primary.URL); host != "" {
				hint = fmt.Sprintf(" [gh auth token --hostname %s]", host)
			} else {
				hint = " [gh auth token]"
			}
		}
		return milkTag() + " " + lbl + bold("Command that prints your token") + hint + ": "
	case initStepRunCmd:
		return milkTag() + " Command that starts your model server, if milk should launch it (leave blank or type 'default' to skip)\n" +
			dim("  e.g. llama-server -m ~/models/qwen2.5-coder-7b-q4_k_m.gguf --port 8080 -ngl 99") + "\n" +
			milkTag() + " run_cmd: "
	case initStepAWSRegion:
		return milkTag() + " " + lbl + bold("AWS region") + " " + initWizardExample("e.g. us-east-1") + ": "
	case initStepLimits:
		if v, ok := modelsdev.Lookup(st.primary.Model); ok {
			return milkTag() + fmt.Sprintf(" context window in tokens (matched %q in the models.dev catalog) [%d]: ", st.primary.Model, v)
		}
		return milkTag() + " context window in tokens, if known (leave blank or type 'default' to skip): "
	case initStepEscalation:
		yn, note := "[Y/n]", ""
		if initWizardEscalationDefault() == "n" {
			yn, note = "[y/N]", "\n"+dim("  the claude CLI was not found on your PATH — install and sign in to Claude Code first, or answer n")
		}
		return milkTag() + " " + lbl + bold("When a task is too hard for the primary, hand it to Claude Code?") + "\n" +
			dim("         This is the \"escalation\": a stronger AI milk switches to for the hard prompts.") + note + "\n" +
			milkTag() + " use Claude Code as escalation agent? " + dim(yn) + ": "
	case initStepTuning:
		return milkTag() + " " + lbl + bold("Fine-tune optional settings now?") + dim(" (server start command, context size, agents as tools)") + "\n" +
			dim("  Skip to use sensible defaults — change anything later with /config.") + "\n" +
			milkTag() + " tune now? " + dim("[y/N]") + ": "
	case initStepAgentTools:
		return milkTag() + " enable any agents as tools? Enter agent names (comma-separated), or leave blank / type 'default' to skip: "
	case initStepOpenConfig:
		return milkTag() + " open config in editor now? [y/N]: "
	}
	return ""
}
