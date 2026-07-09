package aider

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
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

// saneDefaults are aider flags applied before extra_args so they can be
// overridden by the user's extra_args config.
var saneDefaults = []string{
	"--map-tokens", "2048",
	"--max-chat-history-tokens", "4096",
	"--map-refresh", "files",
	"--no-show-model-warnings",
}

// BaseArgs returns the aider flags that are constant across turns.
// Sane defaults are applied first; extra_args from config override them.
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
	// Sane defaults — applied before extra_args so user config can override.
	args = append(args, saneDefaults...)
	for _, extra := range b.extraArgs {
		args = append(args, strings.Fields(extra)...)
	}
	return args
}

// filePathRe matches bare file paths in text: tokens that contain a slash and
// optionally end with a line number suffix (e.g. "cmd/milk/repl.go:42").
var filePathRe = regexp.MustCompile(`\b([\w./-]+/[\w./-]+)\b`)

// extractFilePaths returns unique existing file paths found in text.
func extractFilePaths(text string) []string {
	seen := map[string]bool{}
	var paths []string
	for _, m := range filePathRe.FindAllString(text, -1) {
		// Strip trailing line-number suffix.
		p, _, _ := strings.Cut(m, ":")
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}
	return paths
}

// FirstArgs returns per-turn args for a new session: context files + prompt.
// File paths found in the prompt are added as --file (editable) args so aider
// can edit them without prompting the user to /add them manually.
func (b *argBuilder) FirstArgs(sessionID, prompt string, contextFiles []string) []string {
	var args []string
	for _, f := range contextFiles {
		args = append(args, "--read", f)
	}
	for _, f := range extractFilePaths(prompt) {
		args = append(args, "--file", f)
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
