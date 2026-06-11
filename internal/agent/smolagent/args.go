package smolagent

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/scoutme/milk/internal/config"
)

// argBuilder implements subprocess.ArgBuilder for the milk-smolagent CLI.
type argBuilder struct {
	bin              string
	modelType        string
	modelID          string
	apiBase          string
	apiKey           string
	actionType       string
	tools            []string
	authorizedImports []string
	maxSteps         int
}

func newArgBuilder(ac config.AgentConfig) *argBuilder {
	bin := ac.SmolagentBin
	if bin == "" {
		bin = "milk-smolagent"
	}
	modelType := ac.ModelType
	if modelType == "" {
		modelType = "OpenAIModel"
	}
	actionType := ac.ActionType
	if actionType == "" {
		actionType = "code"
	}
	maxSteps := ac.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 15
	}
	return &argBuilder{
		bin:              bin,
		modelType:        modelType,
		modelID:          ac.Model,
		apiBase:          ac.URL,
		apiKey:           ac.APIKey,
		actionType:       actionType,
		tools:            ac.SmolagentTools,
		authorizedImports: ac.AuthorizedImports,
		maxSteps:         maxSteps,
	}
}

func (b *argBuilder) Bin() string { return b.bin }

// BaseArgs returns the model/backend flags that are constant across turns.
func (b *argBuilder) BaseArgs() []string {
	var args []string
	args = append(args, "--model-type", b.modelType)
	if b.modelID != "" {
		args = append(args, "--model-id", b.modelID)
	}
	if b.apiBase != "" {
		args = append(args, "--api-base", b.apiBase)
	}
	if b.apiKey != "" {
		args = append(args, "--api-key", b.apiKey)
	}
	args = append(args, "--action-type", b.actionType)
	if len(b.tools) > 0 {
		args = append(args, "--tools")
		args = append(args, b.tools...)
	}
	if len(b.authorizedImports) > 0 {
		args = append(args, "--authorized-imports")
		args = append(args, b.authorizedImports...)
	}
	args = append(args, "--max-steps", fmt.Sprintf("%d", b.maxSteps))
	return args
}

// FirstArgs returns per-turn args for a new session: session ID + context files.
func (b *argBuilder) FirstArgs(sessionID string, contextFiles []string) []string {
	args := []string{"--session-id", sessionID}
	for _, f := range contextFiles {
		args = append(args, "--append-system-prompt-file", f)
	}
	return args
}

// ResumeArgs returns per-turn args to continue an existing session.
// milk-smolagent is stateless (reset=True each turn), so resume is identical
// to first: history is re-injected via context files on every turn.
func (b *argBuilder) ResumeArgs(sessionID string, contextFiles []string) []string {
	return b.FirstArgs(sessionID, contextFiles)
}

// EnvStrip returns env var prefixes to remove from the subprocess environment.
func (b *argBuilder) EnvStrip() []string { return nil }

// Ping checks whether the milk-smolagent binary is available.
func (b *argBuilder) Ping() error {
	// Use --help rather than a bare call; some Python scripts exit non-zero with no args.
	cmd := exec.Command(b.bin, "--help")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%q not available: %w", b.bin, err)
	}
	return nil
}

// WithAPIKey returns a copy of the argBuilder with apiKey set.
func (b *argBuilder) withAPIKey(key string) *argBuilder {
	c := *b
	c.apiKey = key
	return &c
}

// labelString returns a display-friendly string for the agent.
func (b *argBuilder) labelString() string {
	return strings.ToLower(b.actionType) + "-agent"
}
