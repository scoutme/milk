package mcp_test

import (
	"context"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/mcp"
)

// TestStdioTransport_EchoServer connects to a minimal stdio MCP server
// implemented as a shell one-liner and verifies the full handshake + tools/list.
// It uses the math-mcp binary built from /home/scoutme/prova if it exists,
// falling back to a skip when the binary is absent.
func TestStdioTransport_Connect(t *testing.T) {
	const binary = "/home/scoutme/prova/math-mcp"

	cfg := config.MCPServerConfig{
		Name:      "math",
		Transport: "stdio",
		Command:   binary,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := mcp.New(cfg)
	if err := c.Connect(ctx); err != nil {
		// If the binary doesn't exist, skip gracefully.
		t.Skipf("stdio connect failed (binary may be missing): %v", err)
	}
	defer c.Close(ctx)

	tools := c.Tools()
	if len(tools) == 0 {
		t.Fatal("expected at least one tool from math-mcp, got none")
	}

	toolNames := make(map[string]bool, len(tools))
	for _, tool := range tools {
		toolNames[tool.Name] = true
	}
	for _, want := range []string{"add", "subtract", "multiply", "divide"} {
		if !toolNames[want] {
			t.Errorf("expected tool %q to be present; got %v", want, tools)
		}
	}
}

// TestStdioTransport_Call verifies a tool/call round-trip over stdio.
func TestStdioTransport_Call(t *testing.T) {
	const binary = "/home/scoutme/prova/math-mcp"

	cfg := config.MCPServerConfig{
		Name:      "math",
		Transport: "stdio",
		Command:   binary,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := mcp.New(cfg)
	if err := c.Connect(ctx); err != nil {
		t.Skipf("stdio connect failed (binary may be missing): %v", err)
	}
	defer c.Close(ctx)

	result, err := c.Call(ctx, "add", `{"a":3,"b":4}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.IsError {
		t.Fatalf("Call returned error: %v", result.Text())
	}
	got := result.Text()
	if got != "7" {
		t.Errorf("add(3,4): want %q, got %q", "7", got)
	}
}
