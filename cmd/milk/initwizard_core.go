package main

// Host-independent core of the /config init setup wizard: starting it, feeding
// it one answer per step, and committing the resulting config. Both hosts drive
// this state machine — the TUI's handleInitWizardKey (one answer per Enter)
// and the ACP server (one answer per session/prompt while acpSession.pendingInit
// is set) — so the prompts, validation and config write cannot drift apart.

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
	// OfferOpenEditor asks "open config in editor now? [y/N]" after the
	// config is written (the TUI does). ACP finishes right after the commit
	// instead: an ACP client is an editor — /config open hands the file to
	// the platform opener and always names the path as well.
	OfferOpenEditor bool
}

// initWizardResult is the outcome of feeding one answer to the wizard.
type initWizardResult struct {
	// Output is everything the host should display: mid-step notes, a
	// validation error + re-prompt, the next step's prompt, or the
	// completion summary. Never includes the answer itself — echoing it is
	// host-specific (the TUI transcript shows it, an ACP client already
	// rendered the prompt the user sent).
	Output string
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

// initWizardStart starts the wizard: fresh state plus the banner and the first
// prompt (agent name), ready for the first answer.
func initWizardStart() (*initWizardState, string) {
	st := &initWizardState{step: initStepName, escCLI: true}
	return st, milkTag() + " setup wizard — configure primary and escalation agents\n\n" +
		milkTag() + " primary agent name [local]: "
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
		"1": "local", "2": "bedrock", "3": "bearer",
		"4": "claude-cli", "5": "aider-cli", "6": "subprocess",
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
		provider, ok := providerMap[answer]
		if !ok {
			out.WriteString(milkTag() + " invalid choice — enter 1–6\n" + initWizardPrompt(st))
			return initWizardResult{Output: out.String()}
		}
		st.primary.Provider = provider

	case initStepURL:
		if answer == "" {
			out.WriteString(milkTag() + " URL is required\n" + initWizardPrompt(st))
			return initWizardResult{Output: out.String()}
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
			if isCopilotURL(st.primary.URL) {
				answer = "/chat/completions"
			} else if isAzureURL(st.primary.URL) {
				dep := azureDeployment(st.primary.URL)
				if dep == "" {
					dep = st.primary.Model
				}
				answer = "/deployments/" + dep + "/chat/completions"
			} else {
				answer = "/v1/chat/completions"
			}
		}
		// Only store if non-standard to keep config minimal.
		if answer != "/v1/chat/completions" {
			st.primary.ChatPath = answer
		}

	case initStepModel:
		if answer == "" && initWizardNeedsModel(st.primary.Provider) {
			out.WriteString(milkTag() + " model name is required\n" + initWizardPrompt(st))
			return initWizardResult{Output: out.String()}
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
			out.WriteString(milkTag() + " AWS region is required for Bedrock\n" + initWizardPrompt(st))
			return initWizardResult{Output: out.String()}
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
			out.WriteString(initWizardPrompt(st))
			return initWizardResult{Output: out.String()}
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
	out.WriteString(initWizardPrompt(st))
	return initWizardResult{Output: out.String(), Committed: committed}
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
