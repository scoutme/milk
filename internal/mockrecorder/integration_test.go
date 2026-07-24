//go:build integration

package mockrecorder_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServerIntegration starts the milk-mock server binary, sends a chat
// completion request, and asserts the response shape and recorded turn.
//
// Run with: go test -tags integration ./internal/mockrecorder/
// The test requires the milk-mock binary to be built and available as milk-mock in PATH.
func TestServerIntegration(t *testing.T) {
	mockBin := findMockBin(t)
	port := 19989 // non-default port to avoid conflicts
	recDir := t.TempDir()

	srv := startMockServer(t, mockBin, port, "inttest", recDir)
	defer srv.Process.Kill() //nolint:errcheck

	waitForServer(t, port)

	reqBody := map[string]any{
		"model":  "mock-model",
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": "hello from integration test"},
		},
	}
	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/v1/chat/completions", port),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST chat completions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := extractSSEText(string(rawBody))
	if text == "" {
		t.Errorf("expected non-empty text from SSE stream; got: %q", string(rawBody))
	}
	if !strings.Contains(text, "hello from integration test") {
		t.Errorf("response does not echo input; got: %q", text)
	}

	time.Sleep(200 * time.Millisecond)

	sessFile := filepath.Join(recDir, "inttest-sessions.jsonl")
	assertJSONLHasTurn(t, sessFile, "hello from integration test")
}

// TestToolCallServerIntegration verifies the tool-call response path.
func TestToolCallServerIntegration(t *testing.T) {
	mockBin := findMockBin(t)
	port := 19988
	recDir := t.TempDir()

	srv := startMockServer(t, mockBin, port, "tooltest", recDir)
	defer srv.Process.Kill() //nolint:errcheck

	waitForServer(t, port)

	reqBody := map[string]any{
		"model":  "mock-model",
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": "[tool:bash,{\"command\":\"echo hi\"}]"},
		},
	}
	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/v1/chat/completions", port),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	rawBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rawBody), "tool_calls") {
		t.Errorf("expected tool_calls in response; got: %q", string(rawBody))
	}
}

// --- helpers ---

func findMockBin(t *testing.T) string {
	t.Helper()
	// Check current dir, project root, and PATH.
	candidates := []string{
		"./milk-mock",
		"../../milk-mock", // from internal/mockrecorder/
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	t.Skip("milk-mock binary not found; build with 'go build -o ./milk-mock ./cmd/milk-mock'")
	return ""
}

func startMockServer(t *testing.T, bin string, port int, name, recDir string) *processWrapper {
	t.Helper()
	return startProcess(t, bin, port, name, recDir)
}

func waitForServer(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("mock server did not become ready in time")
}

func extractSSEText(raw string) string {
	var parts []string
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			text, _ := delta["content"].(string)
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}

func assertJSONLHasTurn(t *testing.T, path, expectedInput string) {
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
		if input, _ := rec["input"].(string); input == expectedInput {
			return
		}
	}
	t.Errorf("no turn with input %q found in %s", expectedInput, path)
}
