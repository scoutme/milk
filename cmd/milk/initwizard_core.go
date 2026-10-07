package main

// Host-independent core of the /config init setup wizard: starting it, feeding
// it one answer per step, and committing the resulting config. All three
// drives share this state machine — the TUI's handleInitWizardKey (one answer
// per Enter), the ACP server's chat flow (one answer per session/prompt while
// acpSession.pendingInit is set), and the ACP server's elicitation form flow
// (one form dialog per step, see acp_initwizard.go) — so the prompts,
// validation and config write cannot drift apart. The step's structured
// description (initWizardFieldFor: choices, default, required, secret) is
// shared the same way, so form fields mirror the bracketed prompt defaults.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/modelsdev"
)

// initWizardOpts selects the host-specific parts of one wizard step.
type initWizardOpts struct {
	// NumAgents is len(cfg.Agents) of the config being replaced: with one
	// agent or fewer there are no peer agents to expose as tools, so the
	// agent-tools step is skipped (as in the TUI).
	NumAgents int
	// AgentNames are the configured agent names, offered as the agent-tools
	// step's choices by hosts that can render a picker (the ACP form flow).
	AgentNames []string
	// OfferOpenEditor asks "open config in editor now? [y/N]" after the
	// config is written (the TUI does). ACP finishes right after the commit
	// instead: an ACP client is an editor — /config open hands the file to
	// the platform opener and always names the path as well.
	OfferOpenEditor bool
}

// initWizardResult is the outcome of feeding one answer to the wizard.
type initWizardResult struct {
	// Output is host-displayed text for this answer: mid-step notes, a
	// validation error, or the completion summary. Never includes the
	// answer itself — echoing it is host-specific (the TUI transcript shows
	// it, an ACP client already rendered the prompt the user sent) — and
	// never the next question, which lives in Prompt so a form-based host
	// can show Output while asking Prompt as a dialog.
	Output string
	// Prompt is the question to ask next ("" once the wizard is done).
	Prompt string
	// Done means the wizard is finished and pendingInit should be cleared.
	Done bool
	// Committed is non-nil when this answer wrote the config file; the host
	// applies it to its live session state (TUI: st.cfg/hasInferenceAgent,
	// ACP: acpSession.applyConfig rebuilds the session's runners).
	Committed *config.Config
	// OpenEditor is set when the user answered "yes" to the TUI's final
	// open-editor question; the caller runs /config open itself.
	OpenEditor bool
}

// initWizardStart starts the wizard: fresh state plus the banner, ready for
// the first question — initWizardPrompt(st). Hosts append that however they
// ask it: the TUI and ACP's chat fallback echo it as the next transcript
// line, ACP's form flow passes it as the first dialog's message.
func initWizardStart() (*initWizardState, string) {
	st := &initWizardState{step: initStepName, escCLI: true}
	return st, milkTag() + " setup wizard — configure primary and escalation agents\n\n"
}

// initWizardCancelWord reports whether a plain answer aborts the wizard. The
// TUI cancels with esc/ctrl+c; over ACP there is no key to press, so the
// wizard accepts an explicit cancel word as well (plus any slash command,
// which the ACP side handles before calling this — see acpSession.runTurn).
func initWizardCancelWord(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "cancel", "quit", "abort":
		return true
	}
	return false
}

// initWizardApply feeds one answer to the wizard's current step: validates and
// applies it (a rejected answer re-prompts the same step), advances the state
// machine, and commits the config when the flow reaches the end.
func initWizardApply(st *initWizardState, answer string, opts initWizardOpts) initWizardResult {
	answer = strings.TrimSpace(answer)
	// 'default' (or '-', 'enter') stands in for the empty answer that means
	// "the step's default": the TUI can send an empty line, but ACP chat
	// clients refuse to send an empty prompt and some form renderers refuse
	// to submit an empty field. Mapping here — in the shared core — keeps
	// every input channel (TUI, ACP chat, ACP forms, ACP choice buttons)
	// honest at once.
	if initWizardDefaultWord(answer) {
		answer = ""
	}
	var out strings.Builder

	// The TUI's trailing question, asked only after OfferOpenEditor's commit:
	// its answer ends the wizard (optionally launching the editor).
	if st.step == initStepOpenConfig && opts.OfferOpenEditor {
		res := initWizardResult{Done: true}
		lower := strings.ToLower(answer)
		if lower == "y" || lower == "yes" {
			res.OpenEditor = true
		}
		return res
	}

	providerMap := map[string]string{
		"1": "local", "local": "local",
		"2": "bedrock", "bedrock": "bedrock",
		"3": "bearer", "bearer": "bearer",
		"4": "claude-cli", "claude-cli": "claude-cli",
		"5": "aider-cli", "aider-cli": "aider-cli",
		"6": "subprocess", "subprocess": "subprocess",
	}

	switch st.step {
	case initStepName:
		if answer == "" {
			answer = "local"
		}
		st.primary.Name = answer

	case initStepProvider:
		if answer == "" {
			answer = "1"
		}
		provider, ok := providerMap[strings.ToLower(answer)]
		if !ok {
			out.WriteString(milkTag() + " invalid choice — enter 1–6 or a provider name\n")
			return initWizardResult{Output: out.String(), Prompt: initWizardPrompt(st)}
		}
		st.primary.Provider = provider

	case initStepURL:
		if answer == "" {
			out.WriteString(milkTag() + " URL is required\n")
			return initWizardResult{Output: out.String(), Prompt: initWizardPrompt(st)}
		}
		st.primary.URL = answer
		// GitHub Copilot: preset the standard headers automatically.
		if isCopilotURL(answer) {
			st.primary.Headers = map[string]string{
				"Copilot-Integration-Id": "vscode-chat",
				"Editor-Plugin-Version":  "copilot-chat/0.49.0",
				"Editor-Version":         "vscode/1.121.0",
				"X-GitHub-Api-Version":   "2026-01-09",
			}
			out.WriteString(dim("  (GitHub Copilot detected — headers preset automatically)\n"))
		} else if isAzureURL(answer) {
			out.WriteString(dim("  (Azure OpenAI detected — api-key header will be used)\n"))
		}

	case initStepChatPath:
		if answer == "" {
			// apply the suggested default shown in the prompt
			answer = initWizardChatPathDefault(st.primary.URL, st.primary.Model)
		}
		// Only store if non-standard to keep config minimal.
		if answer != "/v1/chat/completions" {
			st.primary.ChatPath = answer
		}

	case initStepModel:
		if answer == "" && initWizardNeedsModel(st.primary.Provider) {
			out.WriteString(milkTag() + " model name is required\n")
			return initWizardResult{Output: out.String(), Prompt: initWizardPrompt(st)}
		}
		st.primary.Model = answer

	case initStepAuth:
		// Azure: store key in headers["api-key"], not as a Bearer token.
		if answer != "" && isAzureURL(st.primary.URL) {
			if st.primary.Headers == nil {
				st.primary.Headers = map[string]string{}
			}
			st.primary.Headers["api-key"] = answer
		} else {
			st.primary.APIKey = answer
		}
		// blank → next step will be initStepTokenCmd (handled by nextStep logic)

	case initStepRunCmd:
		st.primary.RunCmd = answer // blank = not set (omitempty keeps config clean)

	case initStepTokenCmd:
		// blank → apply the suggested default shown in brackets
		if answer == "" && isCopilotURL(st.primary.URL) {
			host := copilotHostname(st.primary.URL)
			if host != "" {
				answer = "gh auth token --hostname " + host
			} else {
				answer = "gh auth token"
			}
		}
		st.primary.TokenCmd = answer

	case initStepAWSRegion:
		if answer == "" {
			out.WriteString(milkTag() + " AWS region is required for Bedrock\n")
			return initWizardResult{Output: out.String(), Prompt: initWizardPrompt(st)}
		}
		st.primary.AWSRegion = answer

	case initStepEscalation:
		lower := strings.ToLower(answer)
		st.escCLI = lower != "n" && lower != "no"

	case initStepAgentTools:
		// answer is a comma-separated list of agent names to enable as tools.
		// Blank means skip. We record the choices; they will be applied in
		// the commit.
		if strings.TrimSpace(answer) != "" {
			st.toolAgentNames = splitCommaNames(answer)
		}

	case initStepLimits:
		v := 0
		if answer == "" {
			if catalogV, ok := modelsdev.Lookup(st.primary.Model); ok {
				v = catalogV
			}
		} else if n, err := strconv.Atoi(answer); err == nil && n > 0 {
			v = n
		}
		st.contextWindowTokens = v

	case initStepOpenConfig:
		// Reached with OfferOpenEditor=false only if something went out of
		// band — finish rather than loop on a question nobody will answer.
		return initWizardResult{Done: true}
	}

	st.step = initWizardNextStep(st)
	// Skip the agent-tools step when the config has only one agent
	// (no peer agents to expose as tools yet).
	if st.step == initStepAgentTools && opts.NumAgents <= 1 {
		st.step = initStepOpenConfig
	}

	var committed *config.Config
	if st.step == initStepOpenConfig {
		// Write config before asking about opening the editor.
		cfg, summary, err := commitInitWizardConfig(st)
		if err != nil {
			out.WriteString(fmt.Sprintf("%s error saving config: %v\n", milkTag(), err))
			if !opts.OfferOpenEditor {
				return initWizardResult{Output: out.String(), Done: true}
			}
			// TUI parity: the editor question follows even a failed save.
			return initWizardResult{Output: out.String(), Prompt: initWizardPrompt(st)}
		}
		committed = &cfg
		out.WriteString(summary)
		if !opts.OfferOpenEditor {
			st.step = initStepDone
			return initWizardResult{Output: out.String(), Done: true, Committed: committed}
		}
	}
	if st.step == initStepDone {
		return initWizardResult{Output: out.String(), Done: true, Committed: committed}
	}
	return initWizardResult{Output: out.String(), Prompt: initWizardPrompt(st), Committed: committed}
}

// commitInitWizardConfig builds the config from the wizard's answers and writes
// it (global or local scope, like every other config save — see
// saveLocalOrGlobal). Returns the saved config and the completion summary to
// display, or the save error.
func commitInitWizardConfig(st *initWizardState) (config.Config, string, error) {
	var escalation *config.AgentConfig
	if st.escCLI {
		e := config.AgentConfig{Name: "claude", Provider: "claude-cli"}
		escalation = &e
	}
	// message_budget_chars/max_tool_iterations are auto-derived from
	// context_window_tokens by AgentMessageBudget/AgentContextWindowTokens —
	// no separate AgentLimits needed here.
	if st.contextWindowTokens > 0 {
		st.primary.ContextWindowTokens = st.contextWindowTokens
	}
	cfg := config.InitConfig(st.primary, escalation)
	// Add tool-agent entries from wizard step.
	for _, name := range st.toolAgentNames {
		if strings.EqualFold(name, st.primary.Name) {
			continue // skip self-reference
		}
		cfg.AgentTools = append(cfg.AgentTools, config.AgentToolEntry{
			Agent:       name,
			Description: "Specialist agent. Describe its capabilities here.",
		})
	}
	if err := saveLocalOrGlobal(cfg); err != nil {
		return cfg, "", err
	}

	scopeLabel := "~/.milk/config.json"
	if config.HasLocalConfig() {
		scopeLabel = ".milk/config.json"
	}
	provider := st.primary.Provider
	if provider == "" {
		provider = "local"
	}

	var sb strings.Builder
	sb.WriteString("\n" + milkTag() + " config written to " + scopeLabel + "\n\n")
	if strings.ToLower(provider) == "claude-cli" {
		sb.WriteString(fmt.Sprintf("%s primary: %s  (%s)\n", milkTag(), bold(st.primary.Name), provider))
	} else {
		chatPath := st.primary.ChatPath
		if chatPath == "" {
			chatPath = "/v1/chat/completions"
		}
		sb.WriteString(fmt.Sprintf("%s primary: %s  (%s%s | %s | %s)\n",
			milkTag(), bold(st.primary.Name), st.primary.URL, chatPath, st.primary.Model, provider))
	}
	if escalation != nil {
		sb.WriteString(fmt.Sprintf("%s escalation: %s  (claude-cli)\n", milkTag(), bold(escalation.Name)))
	}
	// Post-completion hints for fields the wizard doesn't ask for.
	var hints []string
	if st.primary.Provider == "bedrock" {
		hints = append(hints, dim("  tip: if you use short-lived STS credentials, add aws_refresh_cmd to your agent config to auto-renew on 403"))
	}
	if isCopilotURL(st.primary.URL) || isAzureURL(st.primary.URL) {
		hints = append(hints, dim("  tip: set limits.message_budget_chars in your agent config to cap context size (e.g. 800000 for Copilot/Azure)"))
	}
	if len(hints) > 0 {
		sb.WriteString("\n")
		for _, h := range hints {
			sb.WriteString(h + "\n")
		}
	}
	sb.WriteString("\n" + milkTag() + " ready — type a message to start, or /help for all commands\n")
	return cfg, sb.String(), nil
}

// initWizardChatPathDefault is the chat-path default proposed for a URL:
// the provider-specific standard endpoint. model is the Azure deployment
// fallback for URLs without a /deployments/<name>/ segment (the prompt
// passes a "<deployment>" placeholder there, apply passes the model name
// once known).
func initWizardChatPathDefault(url, model string) string {
	if isCopilotURL(url) {
		return "/chat/completions"
	}
	if isAzureURL(url) {
		dep := azureDeployment(url)
		if dep == "" {
			dep = model
		}
		return "/deployments/" + dep + "/chat/completions"
	}
	return "/v1/chat/completions"
}

// initWizardChoice is one titled choice of a select/multiselect field.
type initWizardChoice struct{ Value, Label string }

// initWizardField describes one wizard step as a structured input field so
// hosts can offer more than a bare text prompt: ACP turns it into an
// elicitation form field (titled choices, pre-populated defaults — see
// acp_initwizard.go), and the Required/Default flags document exactly which
// steps accept a blank answer and what it becomes. Every Default mirrors the
// bracketed value initWizardPrompt shows and initWizardApply applies on a
// blank — one source, no drift.
type initWizardField struct {
	Name     string // stable form-field key ("agent_name", "provider", ...)
	Label    string // human-readable title
	Kind     string // "text" | "select" | "multiselect"
	Options  []initWizardChoice
	Default  string   // suggested default ("" = none)
	Required bool     // a blank answer is not accepted
	Secret   bool     // a credential — must not go through form channels
	Examples []string // one-click example answers (mirrors the prompt's "e.g." hints)
}

// initWizardFieldFor describes the wizard's current step.
func initWizardFieldFor(st *initWizardState, opts initWizardOpts) initWizardField {
	switch st.step {
	case initStepName:
		return initWizardField{Name: "agent_name", Label: "primary agent name", Kind: "text", Default: "local"}
	case initStepProvider:
		return initWizardField{Name: "provider", Label: "provider", Kind: "select", Default: "local", Options: []initWizardChoice{
			{"local", "local — llama.cpp · Ollama · vLLM · LM Studio (plain HTTP)"},
			{"bedrock", "bedrock — AWS Bedrock Converse API"},
			{"bearer", "bearer — OpenRouter · Together.ai · Groq · GitHub Copilot · any Bearer-token API"},
			{"claude-cli", "claude-cli — Claude Code CLI (no HTTP server needed)"},
			{"aider-cli", "aider-cli — aider subprocess"},
			{"subprocess", "subprocess — generic NDJSON subprocess"},
		}}
	case initStepURL:
		f := initWizardField{Name: "url", Label: "server URL", Kind: "text", Required: true}
		// Only directly-usable examples become one-click suggestions — the
		// prompt's placeholder-shaped ones ("<org>", "<res>") stay text-only.
		switch st.primary.Provider {
		case "local":
			f.Examples = []string{"http://localhost:8080"}
		case "bearer":
			f.Examples = []string{"https://openrouter.ai/api/v1"}
		}
		return f
	case initStepChatPath:
		return initWizardField{Name: "chat_path", Label: "chat path", Kind: "text",
			Default: initWizardChatPathDefault(st.primary.URL, st.primary.Model)}
	case initStepRunCmd:
		return initWizardField{Name: "run_cmd", Label: "server start command (optional)", Kind: "text"}
	case initStepModel:
		f := initWizardField{Name: "model", Label: "model name", Kind: "text",
			Required: initWizardNeedsModel(st.primary.Provider)}
		// One-click examples mirror the prompt's "e.g." hints (placeholders
		// like "<deployment>" stay text-only).
		switch {
		case isCopilotURL(st.primary.URL):
			f.Examples = []string{"claude-sonnet-4.6", "gpt-4o"}
		case isAzureURL(st.primary.URL):
			f.Examples = []string{"gpt-4.1"}
		case st.primary.Provider == "bearer":
			f.Examples = []string{"meta-llama/llama-3.1-8b-instruct"}
		case st.primary.Provider == "aider-cli":
			f.Examples = []string{"gpt-4o"}
		case st.primary.Provider == "local":
			f.Examples = []string{"qwen2.5-coder"}
		}
		return f
	case initStepAuth:
		// A credential: elicitation form mode MUST NOT carry secrets (API
		// keys included), so the wizard asks this one as typed input (with
		// skip/type clicks first where the client has buttons) — see
		// acp_initwizard.go's runInitDialogs.
		return initWizardField{Name: "api_key", Label: "API key", Kind: "text", Secret: true}
	case initStepTokenCmd:
		f := initWizardField{Name: "token_cmd", Label: "token command", Kind: "text"}
		if isCopilotURL(st.primary.URL) {
			if host := copilotHostname(st.primary.URL); host != "" {
				f.Default = "gh auth token --hostname " + host
			} else {
				f.Default = "gh auth token"
			}
		} else {
			f.Examples = []string{"gh auth token", "op read op://vault/item/field"}
		}
		return f
	case initStepAWSRegion:
		return initWizardField{Name: "aws_region", Label: "AWS region", Kind: "text", Required: true,
			Examples: []string{"us-east-1", "eu-west-1"}}
	case initStepLimits:
		f := initWizardField{Name: "context_window_tokens", Label: "context window in tokens", Kind: "text"}
		if v, ok := modelsdev.Lookup(st.primary.Model); ok {
			f.Default = strconv.Itoa(v)
		}
		return f
	case initStepEscalation:
		return initWizardField{Name: "escalation", Label: "use Claude Code CLI as escalation agent?",
			Kind: "select", Default: "y",
			Options: []initWizardChoice{{"y", "yes"}, {"n", "no"}}}
	case initStepAgentTools:
		f := initWizardField{Name: "agent_tools", Label: "agents to enable as tools", Kind: "multiselect"}
		for _, n := range opts.AgentNames {
			f.Options = append(f.Options, initWizardChoice{Value: n, Label: n})
		}
		return f
	}
	return initWizardField{}
}

// initWizardPick is one clickable answer for a wizard step, offered by hosts
// that can render buttons (the ACP choice prompt): a discrete choice, a
// one-click example, or the step's blank answer ("use default" — the click
// that stands in for the empty turn ACP chat can't send). Custom marks the
// "type it myself" escape hatch, which hands the step to typed input.
type initWizardPick struct {
	Value  string // the answer fed to initWizardApply ("" = the blank answer)
	Label  string
	Custom bool
}

// initWizardPicks returns the clickable answers for a field. Selects are a
// closed set of titled choices (default marked); text fields offer the blank
// answer (the default, or skip) plus one-click examples; secrets never carry
// a value — only "skip" vs "type it". Every open-ended list ends with the
// Custom escape. A lone Custom entry means "nothing worth clicking" — hosts
// ask for typed input directly.
func initWizardPicks(f initWizardField) []initWizardPick {
	var picks []initWizardPick
	switch f.Kind {
	case "select":
		for _, o := range f.Options {
			label := o.Label
			if o.Value == f.Default {
				label += " (default)"
			}
			picks = append(picks, initWizardPick{Value: o.Value, Label: label})
		}
		return picks // closed set — no typed escape
	case "multiselect":
		picks = append(picks, initWizardPick{Label: "skip — none"})
		if len(f.Options) > 0 {
			var all []string
			for _, o := range f.Options {
				all = append(all, o.Value)
			}
			picks = append(picks, initWizardPick{Value: strings.Join(all, ","), Label: "enable all — " + strings.Join(all, ", ")})
		}
	default:
		switch {
		case f.Secret:
			picks = append(picks, initWizardPick{Label: "skip — no credential"})
		case f.Default != "":
			picks = append(picks, initWizardPick{Label: "use default — " + f.Default})
		case !f.Required:
			picks = append(picks, initWizardPick{Label: "skip"})
		}
		for _, ex := range f.Examples {
			picks = append(picks, initWizardPick{Value: ex, Label: ex})
		}
	}
	picks = append(picks, initWizardPick{Custom: true, Label: "type my own value…"})
	return picks
}

// initWizardDefaultWord reports whether a plain answer stands in for the
// empty answer that means "take the default" in the TUI. ACP chat clients
// generally refuse to send an empty message, so over ACP the words "default"
// and "-" play that role: they feed initWizardApply as blank, which applies
// the step's default where there is one and re-prompts where there isn't.
func initWizardDefaultWord(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "default", "-":
		return true
	}
	return false
}

// initWizardFieldValue extracts the answer string for one field from an
// accepted form's content (the elicitation spec's content values:
// string/number/boolean/string[]). A missing key means the untouched
// default was submitted — the empty answer initWizardApply already treats
// as "take the default".
func initWizardFieldValue(content map[string]any, f initWizardField) string {
	v, ok := content[f.Name]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "y"
		}
		return "n"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprintf("%v", v)
}
