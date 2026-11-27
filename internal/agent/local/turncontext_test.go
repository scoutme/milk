package local

import (
	"strings"
	"testing"
)

func TestTurnContextBlock(t *testing.T) {
	if got := turnContextBlock("", "  ", ""); got != "" {
		t.Fatalf("empty segments must yield no block, got %q", got)
	}
	got := turnContextBlock("orientation", "", "percepts")
	if !strings.HasPrefix(got, "<milk-context>\n") || !strings.HasSuffix(got, "</milk-context>\n\n") {
		t.Fatalf("block must be delimited and end with a blank line, got %q", got)
	}
	if !strings.Contains(got, "orientation\n\npercepts") {
		t.Errorf("segments must be joined by a blank line in order, got %q", got)
	}
}

func TestSwapUserBlockShedsOnlyTheOptionalSegment(t *testing.T) {
	full := turnContextBlock("PROJECT MAP", "PERCEPTS")
	reduced := turnContextBlock("PERCEPTS")
	m := Message{Role: "user", Content: full + "do the thing"}

	got := swapUserBlock(m, full, reduced)
	if strings.Contains(got.Content, "PROJECT MAP") || !strings.Contains(got.Content, "PERCEPTS") || !strings.HasSuffix(got.Content, "do the thing") {
		t.Errorf("reduction must drop the project map only, got %q", got.Content)
	}
	if m.Content != full+"do the thing" {
		t.Error("swapUserBlock must not mutate its input")
	}

	// Multipart: the text part carries the block.
	mp := Message{Role: "user", ContentParts: []ContentPart{{Type: "text", Text: full + "look"}, {Type: "image_url"}}}
	gp := swapUserBlock(mp, full, reduced)
	if strings.Contains(gp.ContentParts[0].Text, "PROJECT MAP") || len(gp.ContentParts) != 2 {
		t.Errorf("multipart text part must be reduced and parts preserved, got %+v", gp.ContentParts)
	}
	if !strings.Contains(mp.ContentParts[0].Text, "PROJECT MAP") {
		t.Error("swapUserBlock must not mutate the input's parts")
	}

	// A message that doesn't start with the block is untouched.
	other := Message{Role: "user", Content: "plain"}
	if swapUserBlock(other, full, reduced).Content != "plain" {
		t.Error("message without the leading block must be unchanged")
	}
}
