package ansi

import "testing"

func TestColorize_ResetBeforeTrailingNewline(t *testing.T) {
	oldTTY := IsTTY
	IsTTY = true
	t.Cleanup(func() { IsTTY = oldTTY })

	got := Colorize("[⚠ consumption: 5000 reasoning chunks this turn (limit 5000)]\n", CodeDim)
	want := CodeDim + "[⚠ consumption: 5000 reasoning chunks this turn (limit 5000)]" + CodeReset + "\n"

	if got != want {
		t.Fatalf("Colorize() = %q, want %q", got, want)
	}
}

func TestDim_ResetsBeforeEachNewline(t *testing.T) {
	oldTTY := IsTTY
	IsTTY = true
	t.Cleanup(func() { IsTTY = oldTTY })

	got := Dim("one\ntwo\n")
	want := CodeDim + "one" + CodeReset + "\n" + CodeDim + "two" + CodeReset + "\n"

	if got != want {
		t.Fatalf("Dim() = %q, want %q", got, want)
	}
}

func TestDim_NonTTYLeavesTextUnchanged(t *testing.T) {
	oldTTY := IsTTY
	IsTTY = false
	t.Cleanup(func() { IsTTY = oldTTY })

	input := "one\ntwo\n"
	if got := Dim(input); got != input {
		t.Fatalf("Dim() = %q, want %q", got, input)
	}
}

func TestColorize_EmptyStringUnchanged(t *testing.T) {
	oldTTY := IsTTY
	IsTTY = true
	t.Cleanup(func() { IsTTY = oldTTY })

	if got := Colorize("", CodeDim); got != "" {
		t.Fatalf("Colorize(\"\") = %q, want empty", got)
	}
}
