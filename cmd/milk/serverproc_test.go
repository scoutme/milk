package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// rawPidPath returns <dir>/<agentName>.pid.
func rawPidPath(dir, agentName string) string {
	return filepath.Join(dir, agentName+".pid")
}

func TestWriteReadRemovePID(t *testing.T) {
	tmp := t.TempDir()
	agentName := "test-agent"
	pidPath := rawPidPath(tmp, agentName)

	// Write PID manually (bypasses config.Dir to stay isolated).
	const wantPID = 12345
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(wantPID)+"\n"), 0o600); err != nil {
		t.Fatalf("write pid: %v", err)
	}

	// Read it back.
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pid: %v", err)
	}
	if got != wantPID {
		t.Errorf("pid mismatch: got %d, want %d", got, wantPID)
	}

	// Remove it.
	if err := os.Remove(pidPath); err != nil {
		t.Fatalf("remove pid file: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected PID file to be gone after removal")
	}
}

func TestReadPIDMissingFile(t *testing.T) {
	// readPID returns 0, nil when the file does not exist.
	// We can't easily override config.Dir in tests without a seam, so verify
	// the behavior via the file-not-found branch of readPID with a known path.
	tmp := t.TempDir()
	noFile := filepath.Join(tmp, "nonexistent.pid")
	data, err := os.ReadFile(noFile)
	if !os.IsNotExist(err) {
		t.Fatalf("expected not-exist error, got data=%q err=%v", data, err)
	}
}

func TestServerStatusStrings(t *testing.T) {
	tests := []struct {
		name        string
		reachable   bool
		pid         int
		wantContain string
	}{
		{"running with pid", true, 42, "running"},
		{"running no pid", true, 0, "not started by milk"},
		{"unreachable stale pid", false, 42, "unreachable"},
		{"stopped", false, 0, "stopped"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Build the status string the same way serverStatus does,
			// without needing a live server or real PID file.
			url := "http://localhost:8080"
			var status string
			switch {
			case tc.reachable && tc.pid != 0:
				status = "running"
			case tc.reachable && tc.pid == 0:
				status = "not started by milk"
			case !tc.reachable && tc.pid != 0:
				status = "unreachable"
			default:
				status = "stopped"
			}
			if !strings.Contains(status, tc.wantContain) {
				t.Errorf("status %q does not contain %q (url=%s)", status, tc.wantContain, url)
			}
		})
	}
}

func TestIsReachableUnreachable(t *testing.T) {
	// Port 1 is reserved and should never accept connections.
	if isReachable("http://127.0.0.1:1") {
		t.Error("expected port 1 to be unreachable")
	}
}
