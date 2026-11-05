package session

import (
	"fmt"
	"os"
	"testing"

	"github.com/scoutme/milk/internal/testutil"
)

// TestMain sandboxes the state root for the whole test binary (#404): the
// store resolves ~/.milk/sessions through config.Dir, and without the
// redirect this package's session fixtures (test-session.json, empty-ID
// files) landed in the developer's real state root.
func TestMain(m *testing.M) {
	canary := testutil.SandboxStateRoot()
	code := m.Run()
	if err := canary(); err != nil {
		fmt.Fprintf(os.Stderr, "\nFATAL: %v\n", err)
		os.Exit(98)
	}
	os.Exit(code)
}
