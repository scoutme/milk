package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/claudesettings"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/router"
)

// --- Wizard types ---

// switchAgentState tracks state for the /agent switch wizard.
// Both name and role may be supplied inline; missing ones are prompted.
type switchAgentState struct {
	name string // agent name chosen (may be set from inline args)
	role string // "primary" or "escalation" (may be set from inline args)
	step switchAgentStep
}

type switchAgentStep int

const (
	switchStepName switchAgentStep = iota
	switchStepRole
	switchStepDone
)

// addAgentState tracks state for the multi-step /agent add wizard.
// Fields are filled one at a time when the user doesn't supply them inline.
type addAgentState struct {
	ac          config.AgentConfig
	step        addAgentStep
	runCmdAsked bool // true once the optional run_cmd step has been shown
}

type addAgentStep int

const (
	addStepName addAgentStep = iota
	addStepProvider
	addStepURL
	addStepRunCmd    // only when provider is local (optional)
	addStepModel
	addStepAPIKey    // only when provider is bearer
	addStepAWSRegion // only when provider is bedrock
	addStepDone
)

// --- Add-agent wizard ---

// startAddAgent handles `/agent add [key=val ...]`.
// Known keys: name, url, model, provider, api_key, aws_region.
// Missing required fields (name, url, model) are prompted interactively.
func (m model) startAddAgent(inline string) model {
	ac := parseAgentInlineArgs(inline)

	// If all required fields are present, add immediately.
	isCLI := strings.ToLower(ac.Provider) == "claude-cli"
	if ac.Name != "" && (isCLI || (ac.URL != "" && ac.Model != "")) {
		return m.commitAddAgent(ac)
	}

	// Otherwise start the wizard from the first missing required field.
	// If run_cmd was supplied inline, mark it as already asked so the wizard skips it.
	st := &addAgentState{ac: ac, runCmdAsked: ac.RunCmd != ""}
	st.step = firstMissingStep(ac)
	m.pendingAdd = st
	m.appendTranscript(addAgentPrompt(st.step) + " ")
	m.ta.Reset()
	return m
}

// parseAgentInlineArgs parses "key=val key2=val2 ..." into a LocalAgentConfig.
func parseAgentInlineArgs(s string) config.AgentConfig {
	var ac config.AgentConfig
	for _, tok := range strings.Fields(s) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		switch k {
		case "name":
			ac.Name = v
		case "url":
			ac.URL = v
		case "model":
			ac.Model = v
		case "provider":
			ac.Provider = v
		case "api_key":
			ac.APIKey = v
		case "aws_region":
			ac.AWSRegion = v
		case "bin":
			ac.Bin = v
		case "run_cmd":
			ac.RunCmd = v
		}
	}
	return ac
}

// firstMissingStep returns the first wizard step that still needs input.
func firstMissingStep(ac config.AgentConfig) addAgentStep {
	if ac.Name == "" {
		return addStepName
	}
	if ac.Provider == "" {
		return addStepProvider
	}
	p := strings.ToLower(ac.Provider)
	isCLI := p == "claude-cli"
	if !isCLI && ac.URL == "" {
		return addStepURL
	}
	if !isCLI && ac.Model == "" {
		return addStepModel
	}
	if !isCLI && p != "local" && p != "bedrock" && ac.APIKey == "" {
		return addStepAPIKey
	}
	if p == "bedrock" && ac.AWSRegion == "" {
		return addStepAWSRegion
	}
	return addStepDone
}

// addAgentPrompt returns the prompt string for a wizard step.
func addAgentPrompt(step addAgentStep) string {
	switch step {
	case addStepName:
		return milkTag() + " name:"
	case addStepURL:
		return milkTag() + " url:"
	case addStepModel:
		return milkTag() + " model:"
	case addStepProvider:
		return milkTag() + " provider [local/bedrock/claude-cli/<bearer-name>, enter to skip]:"
	case addStepRunCmd:
		return milkTag() + " run_cmd (optional — command to start the server, e.g. llama-server --model ~/models/…):"
	case addStepAPIKey:
		return milkTag() + " api_key:"
	case addStepAWSRegion:
		return milkTag() + " aws_region:"
	default:
		return ""
	}
}

// handleAddAgentKey handles keypresses during the /agent add wizard.
func (m model) handleAddAgentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingAdd = nil
		m.appendTranscript("\n" + milkTag() + " cancelled\n")
		return m, nil
	case "enter":
		answer := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript(answer + "\n")

		st := m.pendingAdd
		switch st.step {
		case addStepName:
			if answer == "" {
				m.appendTranscript(milkTag() + " name is required\n" + addAgentPrompt(addStepName) + " ")
				return m, nil
			}
			st.ac.Name = answer
		case addStepURL:
			if answer == "" {
				m.appendTranscript(milkTag() + " url is required\n" + addAgentPrompt(addStepURL) + " ")
				return m, nil
			}
			st.ac.URL = answer
		case addStepModel:
			if answer == "" {
				m.appendTranscript(milkTag() + " model is required\n" + addAgentPrompt(addStepModel) + " ")
				return m, nil
			}
			st.ac.Model = answer
		case addStepProvider:
			st.ac.Provider = answer // empty = local, which is fine
		case addStepRunCmd:
			st.ac.RunCmd = answer // blank = not set (omitempty keeps config clean)
			st.runCmdAsked = true
		case addStepAPIKey:
			st.ac.APIKey = answer
		case addStepAWSRegion:
			st.ac.AWSRegion = answer
		}

		// After URL is set: inject run_cmd step for local provider before advancing.
		if st.step == addStepURL && strings.ToLower(st.ac.Provider) == "local" && !st.runCmdAsked {
			st.step = addStepRunCmd
			m.appendTranscript(addAgentPrompt(st.step) + " ")
			return m, nil
		}

		// Advance to next missing step.
		st.step = firstMissingStep(st.ac)
		if st.step == addStepDone {
			m.pendingAdd = nil
			m = m.commitAddAgent(st.ac)
		} else {
			m.appendTranscript(addAgentPrompt(st.step) + " ")
		}
		return m, nil
	}
	var cmd tea.Cmd
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// commitAddAgent appends the new agent to config, saves, and confirms.
func (m model) commitAddAgent(ac config.AgentConfig) model {
	// Check for name collision.
	for _, existing := range m.st.cfg.Agents {
		if strings.EqualFold(existing.Name, ac.Name) {
			m.appendTranscript(fmt.Sprintf("%s agent %q already exists — use /agent switch %s to activate it\n",
				milkTag(), ac.Name, ac.Name))
			return m
		}
	}
	isFirst := len(m.st.cfg.Agents) == 0
	m.st.cfg.Agents = append(m.st.cfg.Agents, ac)
	if isFirst {
		m.st.cfg.Agent = ac.Name
	}
	if err := config.Save(m.st.cfg); err != nil {
		m.appendTranscript(fmt.Sprintf("%s error saving config: %v\n", milkTag(), err))
		return m
	}
	m.hasInferenceAgent = true
	if isFirst {
		freshAC := applyFreshAWSCreds(m.st.cfg, activeLocalAgentConfig(m.st.cfg))
		newAgent := local.NewFromConfig(freshAC)
		if od, err := config.OtelDir(); err == nil {
			newAgent.WithOtelDir(od)
		}
		prog := m.st.program
		newAgent.WithOnSigV4Refresh(func(err error) {
			prog.Send(credRefreshReadyMsg{label: "AWS", err: err})
		})
		newAgent.WithLogContext(m.st.cfg.Otel.LogContext)
		ist := m.st
		newAgent.WithOnTokens(func(model, role string, prompt, completion int64) {
			ist.sess.AddTokens(model, role, prompt, completion)
		})
		m.agents.local = newAgent
		m.agents.localAvail = newAgent.Ping(m.ctx) == nil
		m.rtr = router.New(m.st.cfg, newAgent)
	}
	provider := ac.Provider
	if provider == "" {
		provider = "local"
	}
	var agentDetail string
	if strings.ToLower(provider) == "claude-cli" {
		bin := ac.Bin
		if bin == "" {
			bin = "claude"
		}
		agentDetail = fmt.Sprintf("%s added agent %s  (%s | bin=%s)\n",
			milkTag(), bold(ac.Name), provider, bin)
	} else {
		agentDetail = fmt.Sprintf("%s added agent %s  (%s | %s | %s)\n",
			milkTag(), bold(ac.Name), ac.URL, ac.Model, provider)
	}
	m.appendTranscript(agentDetail)
	if isFirst {
		m.appendTranscript(milkTag() + " activated as the default provider\n")
	} else {
		m.appendTranscript(milkTag() + " use /agent switch " + ac.Name + " to activate it\n")
	}
	return m
}

// --- Add-MCP-server wizard ---

type addMCPStep int

const (
	mcpStepName      addMCPStep = iota
	mcpStepTransport            // "http" or "stdio" — enter to skip (defaults to http)
	mcpStepURL                  // only when transport != "stdio"
	mcpStepCommand              // only when transport == "stdio"
	mcpStepAuth                 // "none", "bearer", "token_cmd" — enter to skip (defaults to none)
	mcpStepAPIKey               // only when auth == "bearer"
	mcpStepTokenCmd             // only when auth == "token_cmd"
	mcpStepDone
)

type addMCPState struct {
	sc   config.MCPServerConfig
	step addMCPStep
}

// startAddMCP handles `/mcp add [key=val ...]`.
// Missing required fields are prompted interactively.
// For http transport the required field is url; for stdio it is command.
func (m model) startAddMCP(inline string) model {
	sc := parseMCPInlineArgs(inline)
	isStdio := strings.ToLower(sc.Transport) == "stdio"
	if sc.Name != "" && ((isStdio && sc.Command != "") || (!isStdio && sc.URL != "")) {
		return m.commitAddMCP(sc)
	}
	st := &addMCPState{sc: sc}
	st.step = mcpFirstMissingStep(sc)
	m.pendingMCPAdd = st
	m.appendTranscript(mcpAddPrompt(st.step) + " ")
	m.ta.Reset()
	return m
}

// parseMCPInlineArgs parses "key=val key2=val2 ..." into an MCPServerConfig.
func parseMCPInlineArgs(s string) config.MCPServerConfig {
	var sc config.MCPServerConfig
	for _, tok := range strings.Fields(s) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		switch k {
		case "name":
			sc.Name = v
		case "url":
			sc.URL = v
		case "auth":
			sc.Auth = v
		case "api_key":
			sc.APIKey = v
		case "token_cmd":
			sc.TokenCmd = v
		case "transport":
			sc.Transport = v
		case "timeout":
			sc.Timeout = v
		case "connect_timeout":
			sc.ConnectTimeout = v
		case "command":
			sc.Command = v
		case "args":
			if v != "" {
				sc.Args = strings.Split(v, ",")
			}
		}
	}
	return sc
}

// mcpFirstMissingStep returns the first wizard step still needing input.
func mcpFirstMissingStep(sc config.MCPServerConfig) addMCPStep {
	if sc.Name == "" {
		return mcpStepName
	}
	// Transport defaults to http; only ask when not yet set.
	if sc.Transport == "" {
		return mcpStepTransport
	}
	isStdio := strings.ToLower(sc.Transport) == "stdio"
	if isStdio {
		if sc.Command == "" {
			return mcpStepCommand
		}
	} else {
		if sc.URL == "" {
			return mcpStepURL
		}
	}
	if sc.Auth == "" {
		return mcpStepAuth
	}
	auth := strings.ToLower(sc.Auth)
	if auth == "bearer" && sc.APIKey == "" {
		return mcpStepAPIKey
	}
	if auth == "token_cmd" && sc.TokenCmd == "" {
		return mcpStepTokenCmd
	}
	return mcpStepDone
}

// mcpAddPrompt returns the prompt string for a wizard step.
func mcpAddPrompt(step addMCPStep) string {
	switch step {
	case mcpStepName:
		return milkTag() + " name:"
	case mcpStepTransport:
		return milkTag() + " transport [http/stdio, enter for http]:"
	case mcpStepURL:
		return milkTag() + " url:"
	case mcpStepCommand:
		return milkTag() + " command:"
	case mcpStepAuth:
		return milkTag() + " auth [none/bearer/token_cmd, enter to skip]:"
	case mcpStepAPIKey:
		return milkTag() + " api_key:"
	case mcpStepTokenCmd:
		return milkTag() + " token_cmd:"
	default:
		return ""
	}
}

// handleAddMCPKey handles keypresses during the /mcp add wizard.
func (m model) handleAddMCPKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingMCPAdd = nil
		m.appendTranscript("\n" + milkTag() + " cancelled\n")
		return m, nil
	case "enter":
		answer := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript(answer + "\n")

		st := m.pendingMCPAdd
		switch st.step {
		case mcpStepName:
			if answer == "" {
				m.appendTranscript(milkTag() + " name is required\n" + mcpAddPrompt(mcpStepName) + " ")
				return m, nil
			}
			st.sc.Name = answer
		case mcpStepTransport:
			if answer == "" {
				answer = "http"
			}
			st.sc.Transport = answer
		case mcpStepURL:
			if answer == "" {
				m.appendTranscript(milkTag() + " url is required\n" + mcpAddPrompt(mcpStepURL) + " ")
				return m, nil
			}
			st.sc.URL = answer
		case mcpStepCommand:
			if answer == "" {
				m.appendTranscript(milkTag() + " command is required\n" + mcpAddPrompt(mcpStepCommand) + " ")
				return m, nil
			}
			st.sc.Command = answer
		case mcpStepAuth:
			if answer == "" {
				answer = "none"
			}
			st.sc.Auth = answer
		case mcpStepAPIKey:
			st.sc.APIKey = answer
		case mcpStepTokenCmd:
			st.sc.TokenCmd = answer
		}

		st.step = mcpFirstMissingStep(st.sc)
		if st.step == mcpStepDone {
			m.pendingMCPAdd = nil
			m = m.commitAddMCP(st.sc)
		} else {
			m.appendTranscript(mcpAddPrompt(st.step) + " ")
		}
		return m, nil
	}
	var cmd tea.Cmd
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// commitAddMCP appends the new MCP server to config, saves, and confirms.
func (m model) commitAddMCP(sc config.MCPServerConfig) model {
	for _, existing := range m.st.cfg.MCPServers {
		if strings.EqualFold(existing.Name, sc.Name) {
			m.appendTranscript(fmt.Sprintf("%s MCP server %q already exists — use /mcp enable/disable or /mcp remove first\n",
				milkTag(), sc.Name))
			return m
		}
	}
	// Normalise auth="none" → empty (canonical form).
	if strings.ToLower(sc.Auth) == "none" {
		sc.Auth = ""
	}
	m.st.cfg.MCPServers = append(m.st.cfg.MCPServers, sc)
	if err := config.Save(m.st.cfg); err != nil {
		m.appendTranscript(fmt.Sprintf("%s error saving config: %v\n", milkTag(), err))
		return m
	}
	m.appendTranscript(fmt.Sprintf("%s MCP server %q added — use /mcp assign %s for <agent> to expose it\n",
		milkTag(), sc.Name, sc.Name))
	return m
}

// isCopilotURL returns true when the URL looks like a GitHub Copilot API endpoint.
func isCopilotURL(u string) bool {
	lower := strings.ToLower(u)
	return strings.Contains(lower, "copilot-api") ||
		strings.Contains(lower, "copilot.githubusercontent.com") ||
		strings.Contains(lower, "github.com/copilot") ||
		strings.Contains(lower, "api.githubcopilot.com")
}

// isAzureURL returns true when the URL looks like an Azure OpenAI endpoint.
func isAzureURL(u string) bool {
	lower := strings.ToLower(u)
	return strings.Contains(lower, ".cognitiveservices.azure.com") ||
		strings.Contains(lower, ".openai.azure.com")
}

// azureDeployment extracts the deployment name from an Azure OpenAI URL, or "".
// Handles both /openai and /openai/deployments/<name> patterns.
func azureDeployment(u string) string {
	lower := strings.ToLower(u)
	const marker = "/deployments/"
	if idx := strings.Index(lower, marker); idx >= 0 {
		rest := u[idx+len(marker):]
		if sl := strings.Index(rest, "/"); sl >= 0 {
			rest = rest[:sl]
		}
		return rest
	}
	return ""
}

// copilotHostname extracts the GHE hostname from a Copilot API URL for use in
// "gh auth token --hostname". For github.com Copilot it returns "".
func copilotHostname(u string) string {
	lower := strings.ToLower(u)
	// GHE pattern: copilot-api.<org>.ghe.com  →  hostname = <org>.ghe.com
	if idx := strings.Index(lower, "copilot-api."); idx >= 0 {
		rest := u[idx+len("copilot-api."):]
		if sl := strings.Index(rest, "/"); sl >= 0 {
			rest = rest[:sl]
		}
		return rest
	}
	return ""
}

// --- Init wizard ---

// handleConfigInitCmd starts the /config init TUI wizard.
func (m model) handleConfigInitCmd() (tea.Model, tea.Cmd) {
	m.pendingInit = &initWizardState{step: initStepName, escCLI: true}
	m.appendTranscript(milkTag() + " setup wizard — configure primary and escalation agents\n\n" +
		milkTag() + " primary agent name [local]: ")
	m.ta.Reset()
	return m, nil
}

// initWizardNeedsURL reports whether the provider requires a URL.
func initWizardNeedsURL(provider string) bool {
	switch strings.ToLower(provider) {
	case "claude-cli", "aider-cli", "subprocess":
		return false
	default:
		return true
	}
}

// initWizardNeedsModel reports whether the provider requires a model name.
func initWizardNeedsModel(provider string) bool {
	switch strings.ToLower(provider) {
	case "claude-cli":
		return false
	default:
		return true
	}
}

// initWizardNeedsAuth reports whether the provider requires api_key / token_cmd.
func initWizardNeedsAuth(provider string) bool {
	switch strings.ToLower(provider) {
	case "", "local", "bedrock", "claude-cli", "aider-cli", "subprocess":
		return false
	default: // bearer or any custom bearer-style name
		return true
	}
}

// initWizardNeedsAWSRegion reports whether the provider requires aws_region.
func initWizardNeedsAWSRegion(provider string) bool {
	return strings.ToLower(provider) == "bedrock"
}

// initWizardNeedsChatPath reports whether the provider may need a non-standard chat path.
// Always true for bearer — Copilot and Azure both deviate from the standard path.
func initWizardNeedsChatPath(provider string) bool {
	return strings.ToLower(provider) == "bearer"
}

// initWizardNextStep advances past the current step based on what fields are needed.
func initWizardNextStep(st *initWizardState) initWizardStep {
	p := st.primary.Provider
	switch st.step {
	case initStepName:
		return initStepProvider
	case initStepProvider:
		if initWizardNeedsURL(p) {
			return initStepURL
		}
		if initWizardNeedsModel(p) {
			return initStepModel
		}
		return initStepEscalation
	case initStepURL:
		if initWizardNeedsChatPath(p) {
			return initStepChatPath
		}
		if p == "local" || p == "" {
			return initStepRunCmd
		}
		return initStepModel
	case initStepChatPath:
		if p == "local" || p == "" {
			return initStepRunCmd
		}
		return initStepModel
	case initStepRunCmd:
		return initStepModel
	case initStepModel:
		if initWizardNeedsAuth(p) {
			return initStepAuth
		}
		if initWizardNeedsAWSRegion(p) {
			return initStepAWSRegion
		}
		return initStepLimits
	case initStepAuth:
		// blank api_key → go ask for token_cmd instead
		if st.primary.APIKey == "" {
			return initStepTokenCmd
		}
		if initWizardNeedsAWSRegion(p) {
			return initStepAWSRegion
		}
		return initStepLimits
	case initStepTokenCmd:
		if initWizardNeedsAWSRegion(p) {
			return initStepAWSRegion
		}
		return initStepLimits
	case initStepAWSRegion:
		return initStepLimits
	case initStepLimits:
		return initStepEscalation
	case initStepEscalation:
		return initStepAgentTools
	case initStepAgentTools:
		return initStepOpenConfig
	case initStepOpenConfig:
		return initStepDone
	}
	return initStepDone
}

// initWizardPrompt returns the prompt text for a given wizard step.
func initWizardPrompt(st *initWizardState) string {
	switch st.step {
	case initStepProvider:
		return milkTag() + " provider — select:\n" +
			"  1) local       llama.cpp · Ollama · vLLM · LM Studio (plain HTTP)\n" +
			"  2) bedrock     AWS Bedrock Converse API\n" +
			"  3) bearer      OpenRouter · Together.ai · Groq · GitHub Copilot · any Bearer-token API\n" +
			"  4) claude-cli  Claude Code CLI (no HTTP server needed)\n" +
			"  5) aider-cli   aider subprocess\n" +
			"  6) subprocess  generic NDJSON subprocess\n" +
			milkTag() + " choice [1]: "
	case initStepURL:
		hint := ""
		if st.primary.Provider == "bearer" {
			hint = " (e.g. https://openrouter.ai/api/v1  ·  https://copilot-api.<org>.ghe.com  ·  https://<res>.cognitiveservices.azure.com/openai)"
		} else if st.primary.Provider == "local" {
			hint = " (e.g. http://localhost:8080)"
		}
		return milkTag() + " server URL" + hint + ": "
	case initStepChatPath:
		defPath := "/v1/chat/completions"
		if isCopilotURL(st.primary.URL) {
			defPath = "/chat/completions"
		} else if isAzureURL(st.primary.URL) {
			dep := azureDeployment(st.primary.URL)
			if dep == "" {
				dep = "<deployment>"
			}
			defPath = "/deployments/" + dep + "/chat/completions"
		}
		return milkTag() + fmt.Sprintf(" chat path [%s]: ", defPath)
	case initStepModel:
		hint := ""
		if isCopilotURL(st.primary.URL) {
			hint = " (e.g. claude-sonnet-4.6  or  gpt-4o)"
		} else if isAzureURL(st.primary.URL) {
			hint = " (Azure deployment name, e.g. gpt-4.1)"
		} else if st.primary.Provider == "bearer" {
			hint = " (e.g. meta-llama/llama-3.1-8b-instruct)"
		} else if st.primary.Provider == "bedrock" {
			hint = " (model ARN)"
		} else if st.primary.Provider == "aider-cli" {
			hint = " (e.g. gpt-4o  or  claude-3-5-sonnet-20241022)"
		} else if st.primary.Provider == "subprocess" {
			hint = " (forwarded to subprocess via --model)"
		}
		return milkTag() + " model name" + hint + ": "
	case initStepAuth:
		hint := ""
		if isAzureURL(st.primary.URL) {
			hint = "\n" + dim("  hint: Azure uses an 'api-key' header, not Bearer — paste your Azure API key here")
		} else if isCopilotURL(st.primary.URL) {
			host := copilotHostname(st.primary.URL)
			if host != "" {
				hint = "\n" + dim(fmt.Sprintf("  hint: leave blank and use token_cmd = 'gh auth token --hostname %s'", host))
			} else {
				hint = "\n" + dim("  hint: leave blank and use token_cmd = 'gh auth token'")
			}
		}
		return milkTag() + " API key (leave blank to use token_cmd instead)" + hint + "\n" + milkTag() + " API key: "
	case initStepTokenCmd:
		hint := ""
		if isCopilotURL(st.primary.URL) {
			host := copilotHostname(st.primary.URL)
			if host != "" {
				hint = fmt.Sprintf(" [gh auth token --hostname %s]", host)
			} else {
				hint = " [gh auth token]"
			}
		} else {
			hint = " (e.g. 'gh auth token' or 'op read op://vault/item/field')"
		}
		return milkTag() + " token command" + hint + ": "
	case initStepRunCmd:
		return milkTag() + " server start command (leave blank to skip)\n" +
			dim("  e.g. llama-server -m ~/models/qwen2.5-coder-7b-q4_k_m.gguf --port 8080 -ngl 99") + "\n" +
			milkTag() + " run_cmd: "
	case initStepAWSRegion:
		return milkTag() + " AWS region (e.g. us-east-1): "
	case initStepLimits:
		switch st.limitsSubStep {
		case 0:
			return milkTag() + " does this agent have a large context window? [y/N]: "
		case 1:
			return milkTag() + fmt.Sprintf(" max_tool_iterations [100]: ")
		case 2:
			return milkTag() + fmt.Sprintf(" message_budget_chars [3000000]: ")
		case 3:
			return milkTag() + fmt.Sprintf(" context_budget_chars [200000]: ")
		}
		return ""
	case initStepEscalation:
		return milkTag() + " use Claude Code CLI as escalation agent? [Y/n]: "
	case initStepAgentTools:
		return milkTag() + " enable any agents as tools? Enter agent names (comma-separated) or leave blank to skip: "
	case initStepOpenConfig:
		return milkTag() + " open config in editor now? [y/N]: "
	}
	return ""
}

// handleInitWizardKey handles keypresses during the /init wizard.
func (m model) handleInitWizardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingInit = nil
		m.appendTranscript("\n" + milkTag() + " cancelled\n")
		return m, nil
	case "enter":
		answer := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript(answer + "\n")

		st := m.pendingInit
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
				m.appendTranscript(milkTag() + " invalid choice — enter 1–6\n" + initWizardPrompt(st))
				return m, nil
			}
			st.primary.Provider = provider

		case initStepURL:
			if answer == "" {
				m.appendTranscript(milkTag() + " URL is required\n" + initWizardPrompt(st))
				return m, nil
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
				m.appendTranscript(dim("  (GitHub Copilot detected — headers preset automatically)\n"))
			} else if isAzureURL(answer) {
				m.appendTranscript(dim("  (Azure OpenAI detected — api-key header will be used)\n"))
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
				m.appendTranscript(milkTag() + " model name is required\n" + initWizardPrompt(st))
				return m, nil
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
				m.appendTranscript(milkTag() + " AWS region is required for Bedrock\n" + initWizardPrompt(st))
				return m, nil
			}
			st.primary.AWSRegion = answer

		case initStepEscalation:
			lower := strings.ToLower(answer)
			st.escCLI = lower != "n" && lower != "no"

		case initStepAgentTools:
			// answer is a comma-separated list of agent names to enable as tools.
			// Blank means skip. We record the choices; they will be applied in commitInitWizard.
			if strings.TrimSpace(answer) != "" {
				st.toolAgentNames = splitCommaNames(answer)
			}

		case initStepLimits:
			switch st.limitsSubStep {
			case 0:
				// large context window?
				lower := strings.ToLower(answer)
				st.largeCtx = lower == "y" || lower == "yes"
				if !st.largeCtx {
					// skip to escalation directly
					st.step = initStepEscalation
					m.appendTranscript(initWizardPrompt(st))
					return m, nil
				}
				st.limitsSubStep = 1
				m.appendTranscript(initWizardPrompt(st))
				return m, nil
			case 1:
				v := 100
				if answer != "" {
					if n, err := strconv.Atoi(answer); err == nil && n > 0 {
						v = n
					}
				}
				st.limitToolIter = v
				st.limitsSubStep = 2
				m.appendTranscript(initWizardPrompt(st))
				return m, nil
			case 2:
				v := 3000000
				if answer != "" {
					if n, err := strconv.Atoi(answer); err == nil && n > 0 {
						v = n
					}
				}
				st.limitMsgBudget = v
				st.limitsSubStep = 3
				m.appendTranscript(initWizardPrompt(st))
				return m, nil
			case 3:
				v := 200000
				if answer != "" {
					if n, err := strconv.Atoi(answer); err == nil && n > 0 {
						v = n
					}
				}
				st.limitCtxBudget = v
				// all limit sub-steps done — fall through to next step
			}

		case initStepOpenConfig:
			m.pendingInit = nil
			lower := strings.ToLower(answer)
			if lower == "y" || lower == "yes" {
				newM, _ := m.handleConfigOpenCmd()
				m = newM.(model)
			}
			return m, nil
		}

		st.step = initWizardNextStep(st)
		// Skip the agent-tools step when the config has only one agent
		// (no peer agents to expose as tools yet).
		if st.step == initStepAgentTools && len(m.st.cfg.Agents) <= 1 {
			st.step = initStepOpenConfig
		}
		if st.step == initStepOpenConfig {
			// Write config before asking about opening editor.
			m = m.commitInitWizard(st)
		}
		if st.step == initStepDone {
			m.pendingInit = nil
			return m, nil
		}
		m.appendTranscript(initWizardPrompt(st))
		return m, nil
	}
	var cmd tea.Cmd
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// commitInitWizard writes the config and shows next steps.
func (m model) commitInitWizard(st *initWizardState) model {
	var escalation *config.AgentConfig
	if st.escCLI {
		e := config.AgentConfig{Name: "claude", Provider: "claude-cli"}
		escalation = &e
	}
	// Apply per-agent limits if the user chose large context window.
	if st.largeCtx && (st.limitToolIter > 0 || st.limitMsgBudget > 0 || st.limitCtxBudget > 0) {
		lim := &config.AgentLimits{}
		if st.limitToolIter > 0 {
			v := st.limitToolIter
			lim.MaxToolIterations = &v
		}
		if st.limitMsgBudget > 0 {
			v := st.limitMsgBudget
			lim.MessageBudgetChars = &v
		}
		if st.limitCtxBudget > 0 {
			v := st.limitCtxBudget
			lim.ContextBudgetChars = &v
		}
		st.primary.Limits = lim
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
	if err := config.Save(cfg); err != nil {
		m.appendTranscript(fmt.Sprintf("%s error saving config: %v\n", milkTag(), err))
		return m
	}

	// Apply the new config to the live state so the session picks it up immediately.
	m.st.cfg = cfg
	m.hasInferenceAgent = cfg.HasInferenceAgent()

	provider := st.primary.Provider
	if provider == "" {
		provider = "local"
	}
	m.appendTranscript("\n" + milkTag() + " config written to ~/.milk/config.json\n\n")
	if strings.ToLower(provider) == "claude-cli" {
		m.appendTranscript(fmt.Sprintf("%s primary: %s  (%s)\n", milkTag(), bold(st.primary.Name), provider))
	} else {
		chatPath := st.primary.ChatPath
		if chatPath == "" {
			chatPath = "/v1/chat/completions"
		}
		m.appendTranscript(fmt.Sprintf("%s primary: %s  (%s%s | %s | %s)\n",
			milkTag(), bold(st.primary.Name), st.primary.URL, chatPath, st.primary.Model, provider))
	}
	if escalation != nil {
		m.appendTranscript(fmt.Sprintf("%s escalation: %s  (claude-cli)\n", milkTag(), bold(escalation.Name)))
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
		m.appendTranscript("\n")
		for _, h := range hints {
			m.appendTranscript(h + "\n")
		}
	}
	m.appendTranscript("\n" + milkTag() + " ready — type a message to start, or /help for all commands\n")
	return m
}

// --- Switch-agent wizard ---

// parseSwitchInlineArgs parses "name [as role]" or "name role" inline args.
// Returns name and role (either may be empty).
func parseSwitchInlineArgs(s string) (name, role string) {
	// Accept "name as role" or just "name" or "name role"
	fields := strings.Fields(s)
	switch len(fields) {
	case 0:
	case 1:
		name = fields[0]
	case 2:
		name = fields[0]
		role = strings.ToLower(fields[1])
	case 3:
		name = fields[0]
		// "name as role"
		if strings.ToLower(fields[1]) == "as" {
			role = strings.ToLower(fields[2])
		}
	}
	return
}

// switchAgentPrompt returns the prompt string for the given switch wizard step.
func switchAgentPrompt(step switchAgentStep, names []string) string {
	switch step {
	case switchStepName:
		return milkTag() + " agent name [" + strings.Join(names, ", ") + "]:"
	case switchStepRole:
		return milkTag() + " role [primary/escalation]:"
	}
	return ""
}

// startSwitchAgent starts or immediately executes /agent switch.
// inline is everything after "switch" — may contain name and/or "as role".
func (m model) startSwitchAgent(inline string) (model, tea.Cmd) {
	name, role := parseSwitchInlineArgs(inline)

	// Validate name if provided.
	if name != "" {
		found := false
		for _, a := range m.st.cfg.Agents {
			if strings.EqualFold(a.Name, name) {
				found = true
				break
			}
		}
		if !found {
			var names []string
			for _, a := range m.st.cfg.Agents {
				names = append(names, a.Name)
			}
			m.appendTranscript(fmt.Sprintf("%s unknown agent %q — available: %s\n",
				milkTag(), name, strings.Join(names, ", ")))
			return m, nil
		}
	}
	// Validate role if provided.
	if role != "" && role != "primary" && role != "escalation" {
		m.appendTranscript(fmt.Sprintf("%s unknown role %q — use primary or escalation\n", milkTag(), role))
		return m, nil
	}

	st := &switchAgentState{name: name, role: role}
	// Determine which step to start from.
	if name == "" {
		st.step = switchStepName
	} else if role == "" {
		st.step = switchStepRole
	} else {
		st.step = switchStepDone
		return m.commitSwitchAgent(st)
	}

	var names []string
	for _, a := range m.st.cfg.Agents {
		names = append(names, a.Name)
	}
	m.pendingSwitch = st
	m.appendTranscript(switchAgentPrompt(st.step, names) + " ")
	m.ta.Reset()
	m.syncLayout()
	return m, nil
}

// handleSwitchAgentKey handles keypresses during the /agent switch wizard.
func (m model) handleSwitchAgentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingSwitch = nil
		m.appendTranscript("\n" + milkTag() + " cancelled\n")
		return m, nil
	case "enter":
		answer := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript(answer + "\n")

		st := m.pendingSwitch
		switch st.step {
		case switchStepName:
			if answer == "" {
				var names []string
				for _, a := range m.st.cfg.Agents {
					names = append(names, a.Name)
				}
				m.appendTranscript(switchAgentPrompt(switchStepName, names) + " ")
				return m, nil
			}
			found := false
			for _, a := range m.st.cfg.Agents {
				if strings.EqualFold(a.Name, answer) {
					found = true
					break
				}
			}
			if !found {
				var names []string
				for _, a := range m.st.cfg.Agents {
					names = append(names, a.Name)
				}
				m.appendTranscript(fmt.Sprintf("%s unknown agent %q — available: %s\n", milkTag(), answer, strings.Join(names, ", ")))
				m.appendTranscript(switchAgentPrompt(switchStepName, names) + " ")
				return m, nil
			}
			st.name = answer
			st.step = switchStepRole
			m.appendTranscript(switchAgentPrompt(switchStepRole, nil) + " ")
			return m, nil

		case switchStepRole:
			role := strings.ToLower(answer)
			if role != "primary" && role != "escalation" {
				m.appendTranscript(fmt.Sprintf("%s unknown role %q — use primary or escalation\n", milkTag(), answer))
				m.appendTranscript(switchAgentPrompt(switchStepRole, nil) + " ")
				return m, nil
			}
			st.role = role
			st.step = switchStepDone
			m.pendingSwitch = nil
			return m.commitSwitchAgent(st)
		}
		return m, nil
	}
	var cmd tea.Cmd
	cmd = m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// commitSwitchAgent applies the name+role switch: updates config and rebuilds the live agent.
func (m model) commitSwitchAgent(st *switchAgentState) (model, tea.Cmd) {
	m.pendingSwitch = nil
	name := st.name
	role := st.role

	switch role {
	case "primary":
		m.st.cfg.Agent = name
		if err := config.Save(m.st.cfg); err != nil {
			m.appendTranscript(fmt.Sprintf("%s warning: could not persist switch: %v\n", milkTag(), err))
		}
		newAgent := local.NewFromConfig(activeLocalAgentConfig(m.st.cfg))
		if od, err := config.OtelDir(); err == nil {
			newAgent.WithOtelDir(od)
		}
		prog := m.st.program
		newAgent.WithOnSigV4Refresh(func(err error) {
			prog.Send(credRefreshReadyMsg{label: "AWS", err: err})
		})
		newAgent.WithLogContext(m.st.cfg.Otel.LogContext)
		ist := m.st
		newAgent.WithOnTokens(func(model, role string, prompt, completion int64) {
			ist.sess.AddTokens(model, role, prompt, completion)
		})
		m.agents.local = newAgent
		m.agents.localAvail = newAgent.Ping(m.ctx) == nil
		m.agents.primary = newLocalRunner(newAgent, name)
		m.rtr = router.New(m.st.cfg, newAgent)
		m.credStatus = ""
		m.credLabel = ""
		m.credOK = false
		m.appendTranscript(fmt.Sprintf("%s primary agent → %s\n", milkTag(), bold(name)))
		m.appendTranscript(execAgent(m.st) + "\n")
		if newAgent.HasTokenCmd() {
			m.credRefreshing = true
			m.credLabel = "token"
			return m, func() tea.Msg {
				err := newAgent.WarmToken()
				return credRefreshReadyMsg{label: "token", err: err}
			}
		}
		if needsAWSRefresh(m.st.cfg) {
			m.credRefreshing = true
			m.credLabel = "AWS"
			ctx := m.ctx
			return m, func() tea.Msg {
				cmd := claudesettings.AWSAuthRefreshCommand()
				refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				creds, err := claude.ResolveAWSCredsContext(refreshCtx, cmd)
				return credRefreshReadyMsg{label: "AWS", creds: creds, err: err}
			}
		}
		m.credRefreshing = false

	case "escalation":
		m.st.cfg.EscalationAgent = name
		if err := config.Save(m.st.cfg); err != nil {
			m.appendTranscript(fmt.Sprintf("%s warning: could not persist switch: %v\n", milkTag(), err))
		}
		escAC := applyFreshAWSCreds(m.st.cfg, m.st.cfg.EscalationAgentConfig())
		if escAC.IsCLI() {
			// claude-cli: rebuild cliRunner from current cliAgent.
			m.agents.escalationLocal = nil
			m.agents.subprocessAgent = nil
			m.agents.escalationAvail = true
			m.agents.escalation = newCLIRunner(m.agents.cliAgent, name,
				permContext{cs: m.st.cs, cwd: m.st.cwd}, func() inputReader { return newStdinInputReader() })
		} else if escAC.URL != "" {
			newEsc := local.NewFromConfig(escAC).AsEscalationTarget(escAC.Name)
			if od, err := config.OtelDir(); err == nil {
				newEsc.WithOtelDir(od)
			}
			newEsc.WithLogContext(m.st.cfg.Otel.LogContext)
			ist := m.st
			newEsc.WithOnTokens(func(model, role string, prompt, completion int64) {
				ist.sess.AddTokens(model, role, prompt, completion)
			})
			m.agents.escalationLocal = newEsc
			m.agents.escalationAvail = newEsc.Ping(m.ctx) == nil
			m.agents.escalation = newLocalRunner(newEsc, name)
		}
		m.appendTranscript(fmt.Sprintf("%s escalation agent → %s\n", milkTag(), bold(name)))
		m.appendTranscript(execAgent(m.st) + "\n")
	}

	return m, nil
}
