package local

import (
	"fmt"
	"os"
	"testing"

	"github.com/scoutme/milk/internal/testutil"
)

// TestMain sandboxes the state root for the whole test binary (#404):
// project_map caches outlines under config.Dir()/outline-cache, and without
// the redirect every test run wrote real ~/.milk/outline-cache files.
func TestMain(m *testing.M) {
	canary := testutil.SandboxStateRoot()
	code := m.Run()
	if err := canary(); err != nil {
		fmt.Fprintf(os.Stderr, "\nFATAL: %v\n", err)
		os.Exit(98)
	}
	os.Exit(code)
}
