package config

import (
	"fmt"
	"os"
	"testing"

	"github.com/scoutme/milk/internal/testutil"
)

// TestMain sandboxes the state root for the whole test binary and runs a
// canary over the real config.json (#404): this package's tests exercise
// every save path, so a single missing guard here writes the real
// ~/.milk/config.json.
func TestMain(m *testing.M) {
	canary := testutil.SandboxStateRoot()
	code := m.Run()
	if err := canary(); err != nil {
		fmt.Fprintf(os.Stderr, "\nFATAL: %v\n", err)
		os.Exit(98)
	}
	os.Exit(code)
}
