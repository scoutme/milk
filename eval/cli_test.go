package eval

import "testing"

func TestParseCommaList_Plain(t *testing.T) {
	got := parseCommaList("claude-code,milk-tui")
	want := []string{"claude-code", "milk-tui"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseCommaList_BracketArgsNotSplit(t *testing.T) {
	got := parseCommaList("milk-tui[--agent,mimo-local]")
	want := []string{"milk-tui[--agent,mimo-local]"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseCommaList_MixedBracketAndPlain(t *testing.T) {
	got := parseCommaList("claude-code[--cache-cooldown,5m],milk-tui[--agent,mimo-local]")
	want := []string{"claude-code[--cache-cooldown,5m]", "milk-tui[--agent,mimo-local]"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseCommaList_Empty(t *testing.T) {
	if got := parseCommaList(""); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}
