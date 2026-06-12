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

// argBuilder implements subprocess.ArgBuilder for the milk-aider CLI.
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
		bin = "milk-aider"
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

// BaseArgs returns model/backend flags that are constant across turns.
func (b *argBuilder) BaseArgs() []string {
	var args []string
	if b.model != "" {
		args = append(args, "--model", b.model)
	}
	if b.apiBase != "" {
		args = append(args, "--api-base", b.apiBase)
	}
	if b.apiKey != "" {
		args = append(args, "--api-key", b.apiKey)
	}
	for _, extra := range b.extraArgs {
		args = append(args, strings.Fields(extra)...)
	}
	return args
}

// FirstArgs returns per-turn args for a new session.
func (b *argBuilder) FirstArgs(sessionID string, contextFiles []string) []string {
	args := []string{"--session-id", sessionID}
	for _, f := range contextFiles {
		args = append(args, "--append-system-prompt-file", f)
	}
	return args
}

// ResumeArgs returns per-turn args to continue an existing session.
// milk-aider is stateless (reset each turn); resume re-injects context files.
func (b *argBuilder) ResumeArgs(sessionID string, contextFiles []string) []string {
	return b.FirstArgs(sessionID, contextFiles)
}

// EnvStrip returns env var prefixes to remove from the subprocess environment.
func (b *argBuilder) EnvStrip() []string { return nil }

// Ping checks whether the milk-aider binary is available.
func (b *argBuilder) Ping() error {
	cmd := exec.Command(b.bin, "--help")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%q not available: %w", b.bin, err)
	}
	return nil
}

// New constructs a subprocess.Agent backed by milk-aider.
func New(ac config.AgentConfig) *subprocess.Agent {
	return subprocess.NewAgent(newArgBuilder(ac), &Parser{})
}
