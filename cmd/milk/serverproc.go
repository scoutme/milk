package main

import (
	"context"
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

	cmd := exec.Command("sh", "-c", runCmd)
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("run_cmd: %w", err)
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
		return fmt.Sprintf("unreachable  pid=%d (stale?)  url=%s", pid, url)
	default:
		return fmt.Sprintf("stopped  url=%s", url)
	}
}

// isReachable returns true when the HTTP server at url responds to a HEAD
// request within 2 seconds. Any non-connection-error response counts as up.
func isReachable(url string) bool {
	base := strings.TrimRight(url, "/")
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Head(base)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}
