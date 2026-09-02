package interp

import (
	"bytes"
	"fmt"
	"text/template"
)

// renderTemplate renders body as a Go text/template against vars. Missing
// keys resolve to the zero value (nil) rather than erroring or emitting
// "<no value>" — every prompt in the built-in definitions guards optional
// vars with {{if .x}}, where a nil interface is falsy.
func renderTemplate(name, body string, vars map[string]any) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=zero").Parse(body)
	if err != nil {
		return "", fmt.Errorf("parsing template %q: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("executing template %q: %w", name, err)
	}
	return buf.String(), nil
}

// maxVarChars is the maximum number of characters a template variable can
// contribute to a rendered prompt. Variables exceeding this limit are
// truncated with a marker indicating omitted content. This prevents large
// spec documents (e.g. a 2000-line plan) from filling the agent's context
// window and causing repetition loops.
//
// The limit is per-variable, not per-prompt — a prompt can still be large
// if it uses many variables, but each individual variable is bounded.
const maxVarChars = 8000

// truncateLargeVars returns a copy of vars where any string value exceeding
// maxVarChars is truncated to preserve the first and last portions with a
// marker in between. This keeps the beginning (which typically contains the
// most important context like acceptance criteria) and the end (which often
// contains verdict instructions or summary) while dropping the middle.
//
// Non-string values are passed through unchanged.
func truncateLargeVars(vars map[string]any) map[string]any {
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		if s, ok := v.(string); ok && len(s) > maxVarChars {
			keepHead := maxVarChars * 2 / 3 // ~5300 chars
			keepTail := maxVarChars / 4     // ~2000 chars
			out[k] = s[:keepHead] +
				fmt.Sprintf("\n\n[... %d chars omitted for context management ...]\n\n", len(s)-keepHead-keepTail) +
				s[len(s)-keepTail:]
		} else {
			out[k] = v
		}
	}
	return out
}

// truncatePromptSections applies targeted truncation to the rendered prompt
// itself, not just individual variables. This catches cases where multiple
// moderate-sized variables combine into an oversized prompt. It preserves
// the beginning (task context, sprint requirements) and end (verdict
// instructions) while compressing the middle (detailed specs, prior output).
func truncatePromptSections(prompt string, maxPromptChars int) string {
	if len(prompt) <= maxPromptChars {
		return prompt
	}
	// Keep the first 60% and last 20% of the prompt.
	keepHead := maxPromptChars * 3 / 5
	keepTail := maxPromptChars / 5
	return prompt[:keepHead] +
		fmt.Sprintf("\n\n[... %d chars omitted for context management ...]\n\n", len(prompt)-keepHead-keepTail) +
		prompt[len(prompt)-keepTail:]
}

// sectionCharBudget is the soft limit for a single template variable's
// contribution. Variables exceeding this are truncated. The value is chosen
// to keep the total prompt (with ~3-5 variables) under ~30K chars, which
// leaves room for the agent's tool-call loop without hitting context limits
// on typical 128K-token models.
const sectionCharBudget = 12000

// truncateLargeVarsWithBudget is like truncateLargeVars but uses
// sectionCharBudget and preserves more of the tail (where verdict
// instructions typically live).
func truncateLargeVarsWithBudget(vars map[string]any) map[string]any {
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		if s, ok := v.(string); ok && len(s) > sectionCharBudget {
			keepHead := sectionCharBudget * 3 / 5 // 7200 chars
			keepTail := sectionCharBudget / 3     // 4000 chars — enough for verdict instructions
			out[k] = s[:keepHead] +
				fmt.Sprintf("\n\n[... %d chars omitted for context management ...]\n\n", len(s)-keepHead-keepTail) +
				s[len(s)-keepTail:]
		} else {
			out[k] = v
		}
	}
	return out
}

// summarizeLongOutput produces a condensed version of a long agent output
// for use in subsequent prompt injections (e.g. {{.sprint_output}} in the
// evaluator prompt). Keeps the first and last portions which typically
// contain the summary and verdict.
func summarizeLongOutput(output string, maxChars int) string {
	if len(output) <= maxChars {
		return output
	}
	keepHead := maxChars * 2 / 3
	keepTail := maxChars / 3
	return output[:keepHead] +
		fmt.Sprintf("\n\n[... %d chars omitted ...]\n\n", len(output)-keepHead-keepTail) +
		output[len(output)-keepTail:]
}
