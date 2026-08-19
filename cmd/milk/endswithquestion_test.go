package main

import "testing"

func TestEndsWithQuestion(t *testing.T) {
	if !endsWithQuestion("Want me to implement this?") {
		t.Error("expected true for text ending with '?'")
	}
	if endsWithQuestion("Done implementing this.") {
		t.Error("expected false for text not ending with '?'")
	}
	if endsWithQuestion("") {
		t.Error("expected false for empty text")
	}
	if !endsWithQuestion("Multi-line answer.\nShould I continue?  ") {
		t.Error("expected true when trailing whitespace follows the '?' on the last line")
	}
}
