package main

import "testing"

func TestDimLines_ResetsBeforeEachNewline(t *testing.T) {
	oldIsTTY := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = oldIsTTY })

	got := dimLines("one\ntwo\n")
	want := ansiDim + "one" + ansiReset + "\n" + ansiDim + "two" + ansiReset + "\n"

	if got != want {
		t.Fatalf("dimLines() = %q, want %q", got, want)
	}
}

func TestDimLines_NonTTYLeavesTextUnchanged(t *testing.T) {
	oldIsTTY := isTTY
	isTTY = false
	t.Cleanup(func() { isTTY = oldIsTTY })

	input := "one\ntwo\n"
	if got := dimLines(input); got != input {
		t.Fatalf("dimLines() = %q, want %q", got, input)
	}
}
