package main

import (
	"os"
	"testing"
)

// TestMain keeps the suite off the real system clipboard. Several tests drive
// genuine copy paths (ctrl+c on a transcript/attach selection, copy-on-select),
// and the default copyToClipboard writes through atotto/clipboard (xclip/
// pbcopy/clip.exe) plus OSC 52 — so without this guard every `go test ./cmd/milk`
// clobbered the developer's clipboard with fixture text (e.g. the "CTRLCOPY"
// selection in attach_select_test.go). Tests that assert on copy calls already
// swap the var themselves (copy_on_select_test.go) and restore to this no-op.
func TestMain(m *testing.M) {
	copyToClipboard = func(string) {}
	os.Exit(m.Run())
}
