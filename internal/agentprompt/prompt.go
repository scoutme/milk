// Package agentprompt renders per-agent custom system prompts with placeholder
// substitution. It is used by both the local HTTP agent path and the claude-cli
// subprocess path to prepend a user-defined behaviour block to the default system
// prompt assembled by milk.
package agentprompt

import (
	"fmt"
	"os"
	"strings"

	"github.com/scoutme/milk/internal/config"
)

// PlaceholderVars holds the values substituted for {{milk:*}} placeholders.
// An empty string means the placeholder is absent — the placeholder text is
// removed from the output without replacement.
type PlaceholderVars struct {
	Memory     string // substituted for {{milk:memory}}
	Need       string // substituted for {{milk:need}}
	Escalation string // substituted for {{milk:escalation}}
	Tools      string // substituted for {{milk:tools}}
}

// milkFooter is the compact suffix auto-appended when no {{milk:*}} placeholder
// is present in the prompt text. It ensures milk's core context reaches the agent
// even when the user writes a fully custom prompt without placeholders.
const milkFooter = "\n\n---\n*(milk context injected below)*"

// Render loads and renders the custom agent prompt for ac.
// Resolution order: PromptFile → Prompt → "" (no custom prompt).
// Returns ("", nil) when neither is set — callers should skip prepending.
//
// Placeholder substitution:
//
//	{{milk:memory}}     → vars.Memory     (omitted when empty)
//	{{milk:need}}       → vars.Need       (omitted when empty)
//	{{milk:escalation}} → vars.Escalation (omitted when empty)
//	{{milk:tools}}      → vars.Tools      (omitted when empty)
//
// If no {{milk:*}} placeholder is present in the source text, a compact
// milkFooter block is appended so consumers can still inject their own context.
func Render(ac config.AgentConfig, vars PlaceholderVars) (string, error) {
	var src string

	switch {
	case ac.PromptFile != "":
		data, err := os.ReadFile(ac.PromptFile)
		if err != nil {
			return "", fmt.Errorf("agentprompt: reading prompt_file %q: %w", ac.PromptFile, err)
		}
		src = string(data)
	case ac.Prompt != "":
		src = ac.Prompt
	default:
		return "", nil
	}

	hasPlaceholder := strings.Contains(src, "{{milk:")

	src = substitute(src, "{{milk:memory}}", vars.Memory)
	src = substitute(src, "{{milk:need}}", vars.Need)
	src = substitute(src, "{{milk:escalation}}", vars.Escalation)
	src = substitute(src, "{{milk:tools}}", vars.Tools)

	if !hasPlaceholder {
		src += milkFooter
	}

	return strings.TrimSpace(src), nil
}

// substitute replaces all occurrences of placeholder with value.
// When value is empty, the placeholder is removed entirely (replaced with "").
func substitute(src, placeholder, value string) string {
	return strings.ReplaceAll(src, placeholder, value)
}
