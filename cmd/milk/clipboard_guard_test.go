package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/scoutme/milk/internal/testutil"
)

// TestMain keeps the suite off the system clipboard AND off the real milk
// state root. Several tests drive genuine copy paths (ctrl+c on a
// transcript/attach selection, copy-on-select), and the default
// copyToClipboard writes through atotto/clipboard (xclip/pbcopy/clip.exe)
// plus OSC 52 — so without this guard every `go test ./cmd/milk` clobbered
// the developer's clipboard with fixture text (e.g. the "CTRLCOPY" selection
// in attach_select_test.go). Tests that assert on copy calls already swap
// the var themselves (copy_on_select_test.go) and restore to this no-op.
//
// The state-root sandbox (#404) redirects HOME/USERPROFILE/MILK_CONFIG_ROOT
// for the whole binary: session.New("/repo", …) and friends resolve
// ~/.milk through config.Dir, which made test runs litter the real
// ~/.milk/sessions with hundreds of fixture sessions. The canary fails the
// run loudly if any test ever writes the real config.json anyway.
func TestMain(m *testing.M) {
	copyToClipboard = func(string) {}
	canary := testutil.SandboxStateRoot()
	code := m.Run()
	if err := canary(); err != nil {
		fmt.Fprintf(os.Stderr, "\nFATAL: %v\n", err)
		os.Exit(98)
	}
	os.Exit(code)
}
