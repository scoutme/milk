package obs

import (
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/testutil"
)

func isolateDebugLogPaths(t *testing.T) {
	t.Helper()
	testutil.SandboxHome(t)
	t.Cleanup(config.SetDebugLogFilenamePrefixForTest("test_"))
}
