//go:build integration

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
)

// TestBuildToolRunner_CLIIntegration wires a mock-claude subprocess as a
// claude-cli tool-agent, invokes a turn via RunToolCall, and asserts that the
// response was recorded in the mock's sessions file.
//
// Run with: go test -tags integration ./cmd/milk/ -run TestBuildToolRunner_CLIIntegration -v
//
// Requirements:
//   - milk-mock binary built and present at ../../milk-mock (project root)
func TestBuildToolRunner_CLIIntegration(t *testing.T) {
	mockBin := findMockBinForCLITest(t)

	recDir := t.TempDir()

	// Create a shell wrapper that forwards all args to `milk-mock claude` and
	// bakes in --rec-dir / --name so the recording lands in our temp dir.
	// The claude.Agent subprocess calls `<bin> --print --output-format stream-json ...`
	// at the top level; the wrapper routes those flags into the `claude` subcommand.
	wrapperPath := filepath.Join(t.TempDir(), "mock-claude-wrapper")
	wrapperContent := fmt.Sprintf(
		"#!/bin/sh\nexec %s claude --name inttest-cli --rec-dir %s \"$@\"\n",
		mockBin, recDir,
	)
	if err := os.WriteFile(wrapperPath, []byte(wrapperContent), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}

	// Build a claude-cli AgentConfig pointing at the wrapper.
	ac := config.AgentConfig{
		Name:                       "mock-claude-tool",
		Provider:                   "claude-cli",
		Bin:                        wrapperPath,
		DangerouslySkipPermissions: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	runner, err := buildToolRunner(ctx, ac, config.Config{})
	if err != nil {
		t.Fatalf("buildToolRunner: %v", err)
	}
	if runner == nil {
		t.Fatal("expected non-nil runner")
	}
	if _, ok := runner.(*cliRunner); !ok {
		t.Fatalf("expected *cliRunner, got %T", runner)
	}

	// Invoke a single tool call.
	var out bytes.Buffer
	response, err := runner.RunToolCall(ctx, config.Config{}, "hello from integration test", &out)
	if err != nil {
		t.Fatalf("RunToolCall: %v (output: %q)", err, out.String())
	}
	if response == "" {
		t.Errorf("expected non-empty response, got empty string (raw output: %q)", out.String())
	}
	if !strings.Contains(response, "hello from integration test") {
		t.Errorf("response %q does not contain input echo", response)
	}

	// Give the mock a moment to flush its recording file.
	time.Sleep(200 * time.Millisecond)

	// Assert that a turn was recorded in the mock's sessions file.
	sessFile := filepath.Join(recDir, "inttest-cli-sessions.jsonl")
	assertSessionFileHasTurn(t, sessFile, "hello from integration test")
}

// --- helpers ---

func findMockBinForCLITest(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../milk-mock", // from cmd/milk/ → project root
		"./milk-mock",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, err := filepath.Abs(c)
			if err == nil {
				return abs
			}
			return c
		}
	}
	t.Skip("milk-mock binary not found; build with 'go build -o ./milk-mock ./cmd/milk-mock'")
	return ""
}

func assertSessionFileHasTurn(t *testing.T, path, expectedInput string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sessions file %s: %v", path, err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		input, _ := rec["input"].(string)
		if input == expectedInput {
			return
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	t.Errorf("no turn with input %q found in %s\nfile contents:\n%s", expectedInput, path, string(data))
}
