package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/config"
)

// pidDir returns ~/.milk/servers, creating it if needed.
func pidDir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "servers")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// pidFile returns the path to the PID file for the named agent.
func pidFile(agentName string) (string, error) {
	d, err := pidDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, agentName+".pid"), nil
}

// writePID writes the process PID to the agent's PID file.
func writePID(agentName string, pid int) error {
	path, err := pidFile(agentName)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// readPID reads the PID stored in the agent's PID file.
// Returns 0 and nil when the file does not exist (server not tracked).
func readPID(agentName string) (int, error) {
	path, err := pidFile(agentName)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("malformed PID file: %w", err)
	}
	return pid, nil
}

// removePIDFile deletes the agent's PID file, ignoring not-found errors.
func removePIDFile(agentName string) error {
	path, err := pidFile(agentName)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ensureServerRunning launches runCmd in the background if the inference server
// at url is not already reachable. It polls until the server responds or the
// deadline elapses, returning nil on success and an error if the server never
// became reachable within the timeout.
//
// When runCmd is empty or url is empty the function is a no-op (returns nil).
// The launched process is intentionally detached — it outlives milk so the
// server stays up between sessions.
func ensureServerRunning(ctx context.Context, url, runCmd, agentName string) error {
	if url == "" || runCmd == "" {
		return nil
	}
	if isReachable(url) {
		return nil
	}

	// Redirect server stdout/stderr to a log file so startup errors are visible.
	logPath, _ := serverLogPath(agentName)
	var logFile *os.File
	if logPath != "" {
		logFile, _ = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	}

	cmd := exec.Command("sh", "-c", runCmd)
	cmd.SysProcAttr = detachedSysProcAttr()
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return fmt.Errorf("run_cmd: %w", err)
	}
	if logFile != nil {
		// Close our copy of the fd — the child process keeps its own.
		logFile.Close()
	}

	// Record PID so the server can be stopped later.
	if agentName != "" {
		_ = writePID(agentName, cmd.Process.Pid)
	}

	// Poll until the server is reachable or context/timeout fires.
	deadline := time.Now().Add(60 * time.Second)
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		select {
		case <-pollCtx.Done():
			return fmt.Errorf("server did not become reachable within 60s (run_cmd: %s)", runCmd)
		case <-time.After(1 * time.Second):
			if isReachable(url) {
				return nil
			}
		}
	}
}

// serverStop terminates the tracked server process for agentName.
// Returns (stopped bool, err error): stopped=true when a live process was
// terminated, stopped=false when no PID file exists (not started by milk).
func serverStop(agentName string) (stopped bool, err error) {
	pid, err := readPID(agentName)
	if err != nil {
		return false, err
	}
	if pid == 0 {
		return false, nil
	}

	if err := killProcess(pid); err != nil {
		// Process may already be gone; clean up the PID file and treat as stopped.
		_ = removePIDFile(agentName)
		return false, fmt.Errorf("kill pid %d: %w", pid, err)
	}
	_ = removePIDFile(agentName)
	return true, nil
}

// serverStatus returns a human-readable status string for the named agent's
// server process.
func serverStatus(agentName, url string) string {
	reachable := url != "" && isReachable(url)
	pid, _ := readPID(agentName)

	switch {
	case reachable && pid != 0:
		return fmt.Sprintf("running  pid=%d  url=%s", pid, url)
	case reachable && pid == 0:
		return fmt.Sprintf("running  (not started by milk)  url=%s", url)
	case !reachable && pid != 0:
		logPath, _ := serverLogPath(agentName)
		return fmt.Sprintf("unreachable  pid=%d (stale?)  url=%s  log=%s", pid, url, logPath)
	default:
		return fmt.Sprintf("stopped  url=%s", url)
	}
}

// isReachable returns true when the inference server at url is up and ready.
// It first tries GET <url>/health expecting {"status":"ok"} (llama-server);
// if that endpoint is absent (404/405) it falls back to a HEAD on the base URL.
func isReachable(url string) bool {
	base := strings.TrimRight(url, "/")
	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(base + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// llama-server: 200 with {"status":"ok"} when model is loaded and ready.
	if resp.StatusCode == http.StatusOK {
		var body struct {
			Status string `json:"status"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Status != "" {
			return body.Status == "ok"
		}
		return true // non-JSON 200 from another server — treat as up
	}

	// /health not implemented (404/405): fall back to base URL HEAD.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		r2, err := client.Head(base)
		if err != nil {
			return false
		}
		r2.Body.Close()
		return true
	}

	// Any other non-2xx (e.g. 503 loading) → not ready.
	return false
}

// isConnectionRefused reports whether err indicates the server is not running.
// The syscall-level check is in the platform-specific files.
func isConnectionRefused(err error) bool {
	if connRefusedErrno(err) {
		return true
	}
	return strings.Contains(err.Error(), "connection refused")
}

// serverLogPath returns the path for the server's stdout/stderr log file.
func serverLogPath(agentName string) (string, error) {
	d, err := pidDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, agentName+".log"), nil
}
