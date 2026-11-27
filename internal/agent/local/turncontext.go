package local

import (
	"context"
	"strings"
)

// Stable prompt layout (AgentConfig.PromptLayout, the default).
//
// A prompt cache only pays off when the request is a byte-for-byte extension of
// an earlier one, so the stable prefix — system prompt, tools, settled history —
// must not change from turn to turn. Everything that DOES change per turn (the
// project orientation block, recalled percepts, the current goal, the
// escalation hand-off orientation) is therefore delivered in one
// <milk-context> block prepended to the NEW user message: after everything a
// provider can have cached, at a position that stays fixed through the tool
// loop. The persisted user turn never carries the block, so history stays
// append-only and re-rendering it later is deterministic.

type turnContextKey struct{}

// WithTurnContext returns ctx carrying the caller-assembled per-turn context
// (percepts, current goal, orientation) that Run delivers in the user
// message's <milk-context> block under the stable layout. It travels in the
// context — not on the Agent — so overlapping calls on a shared agent can't
// race (the same discipline as WithImageParts, issue #297). Ignored under the
// classic layout, where the runner still ships these as leading history
// messages.
func WithTurnContext(ctx context.Context, text string) context.Context {
	return context.WithValue(ctx, turnContextKey{}, text)
}

func turnContextFromContext(ctx context.Context) string {
	s, _ := ctx.Value(turnContextKey{}).(string)
	return s
}

// turnContextBlock wraps the non-empty segments in the delimited
// <milk-context> block (same wording as the claude-cli path's
// claude.WithTurnContext, so every agent family sees one convention). Returns
// "" when there is nothing to say; otherwise the block ends with a blank line
// so the user's own text follows directly.
func turnContextBlock(segments ...string) string {
	parts := make([]string, 0, len(segments))
	for _, s := range segments {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "<milk-context>\n" +
		"[Context from milk for this turn — not typed by the user. The user's message follows the closing tag.]\n" +
		strings.Join(parts, "\n\n") + "\n" +
		"</milk-context>\n\n"
}

// swapUserBlock replaces the leading full context block of a user message with
// the reduced one (context-reduction ladder: the optional project orientation
// is shed under pressure while percepts and the goal stay). Messages that don't
// start with the full block are returned unchanged.
func swapUserBlock(m Message, full, reduced string) Message {
	if full == "" || full == reduced {
		return m
	}
	if strings.HasPrefix(m.Content, full) {
		m.Content = reduced + strings.TrimPrefix(m.Content, full)
	}
	if len(m.ContentParts) > 0 && m.ContentParts[0].Type == "text" && strings.HasPrefix(m.ContentParts[0].Text, full) {
		parts := append([]ContentPart(nil), m.ContentParts...)
		parts[0].Text = reduced + strings.TrimPrefix(parts[0].Text, full)
		m.ContentParts = parts
	}
	return m
}

// StablePromptLayout reports whether this agent delivers per-turn dynamic
// context in the user message (the default) rather than the classic leading
// system messages. The runner consults it to decide how to assemble history.
func (a *Agent) StablePromptLayout() bool { return a != nil && !a.promptLayoutClassic }
