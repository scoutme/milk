package aider

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/scoutme/milk/internal/agent/subprocess"
	"github.com/scoutme/milk/internal/config"
)

// resolveAPIKey returns the API key from ac: runs token_cmd when set,
// otherwise returns ac.APIKey directly.
func resolveAPIKey(ac config.AgentConfig) string {
	if ac.TokenCmd != "" {
		out, err := exec.Command("sh", "-c", ac.TokenCmd).Output()
		if err == nil {
			if tok := strings.TrimSpace(string(out)); tok != "" {
				return tok
			}
		}
	}
	return ac.APIKey
}

// inGitRepo returns true when the current working directory is inside a git repo.
func inGitRepo() bool {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// argBuilder implements subprocess.ArgBuilder for the aider CLI.
type argBuilder struct {
	bin       string
	model     string
	apiBase   string
	apiKey    string
	extraArgs []string
}

func newArgBuilder(ac config.AgentConfig) *argBuilder {
	bin := ac.Bin
	if bin == "" {
		bin = "aider"
	}
	return &argBuilder{
		bin:       bin,
		model:     ac.Model,
		apiBase:   ac.URL,
		apiKey:    resolveAPIKey(ac),
		extraArgs: ac.ExtraArgs,
	}
}

func (b *argBuilder) Bin() string { return b.bin }

// BaseArgs returns the aider flags that are constant across turns.
func (b *argBuilder) BaseArgs() []string {
	args := []string{"--yes-always", "--no-pretty", "--no-auto-commits", "--edit-format", "diff"}
	if b.model != "" {
		args = append(args, "--model", b.model)
	}
	if b.apiBase != "" {
		args = append(args, "--openai-api-base", b.apiBase)
	}
	if !inGitRepo() {
		args = append(args, "--no-git")
	}
	for _, extra := range b.extraArgs {
		args = append(args, strings.Fields(extra)...)
	}
	return args
}

// FirstArgs returns per-turn args for a new session: context files + prompt.
func (b *argBuilder) FirstArgs(sessionID, prompt string, contextFiles []string) []string {
	var args []string
	for _, f := range contextFiles {
		args = append(args, "--read", f)
	}
	args = append(args, "--message", prompt)
	return args
}

// ResumeArgs is identical to FirstArgs — aider is stateless per-turn.
func (b *argBuilder) ResumeArgs(sessionID, prompt string, contextFiles []string) []string {
	return b.FirstArgs(sessionID, prompt, contextFiles)
}

// EnvStrip removes conflicting API key vars; the correct key is injected via WithExtraEnv.
func (b *argBuilder) EnvStrip() []string {
	return []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
}

// Ping checks whether the aider binary is available.
func (b *argBuilder) Ping() error {
	cmd := exec.Command(b.bin, "--version")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%q not available: %w", b.bin, err)
	}
	return nil
}

// New constructs a subprocess.Agent backed by the aider CLI directly.
func New(ac config.AgentConfig) *subprocess.Agent {
	b := newArgBuilder(ac)
	agent := subprocess.NewAgent(b, &Parser{})
	// Inject OPENAI_API_KEY so aider can authenticate.
	if key := b.apiKey; key != "" {
		agent = agent.WithExtraEnv("OPENAI_API_KEY=" + key)
	}
	return agent
}
