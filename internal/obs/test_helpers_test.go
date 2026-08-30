package obs

import (
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func isolateDebugLogPaths(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(config.SetDebugLogFilenamePrefixForTest("test_"))
}
