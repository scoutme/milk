package local

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// When a tool result is cut to fit the cap, the full output is written to a
// file so the model can grep or page through it instead of re-running the
// command (OpenCode and MiMo-Code do the same, keeping the files 7 days).
const spillRetention = 7 * 24 * time.Hour

var (
	spillSweepMu sync.Mutex
	spillSwept   = map[string]bool{}
	spillIDChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)

// spillSkipsTool reports tools whose output is never spilled: read_file's
// source is already a file the model can page with offset/limit.
func spillSkipsTool(name string) bool { return name == "read_file" }

// spillToolOutput writes the full Output of result to dir and returns the
// file path, or "" when there is nothing to spill (within the cap, no dir,
// not JSON, write failed). A failed spill only costs the recovery path — the
// result is capped exactly as before.
func spillToolOutput(dir, tool, callID, result string, maxBytes int) string {
	if dir == "" || maxBytes <= 0 || spillSkipsTool(tool) {
		return ""
	}
	var r toolResult
	if json.Unmarshal([]byte(result), &r) != nil || len(r.Output) <= maxBytes {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	sweepSpillDir(dir)
	id := spillIDChars.ReplaceAllString(callID, "")
	if len(id) > 40 {
		id = id[:40]
	}
	name := fmt.Sprintf("%s-%s-%s", time.Now().Format("20060102-150405"), tool, id)
	f, err := os.CreateTemp(dir, name+"-*.txt")
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.WriteString(r.Output); err != nil {
		os.Remove(f.Name()) //nolint:errcheck
		return ""
	}
	return filepath.Clean(f.Name())
}

// sweepSpillDir removes spill files past the retention window, once per
// directory per process.
func sweepSpillDir(dir string) {
	spillSweepMu.Lock()
	done := spillSwept[dir]
	spillSwept[dir] = true
	spillSweepMu.Unlock()
	if done {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > spillRetention {
			os.Remove(filepath.Join(dir, e.Name())) //nolint:errcheck
		}
	}
}

// spillCutHint is the omission-marker hint for a result whose full output
// was saved at path.
func spillCutHint(path string) string {
	return "full output saved to " + path + " — grep it, or read_file with offset/limit"
}
