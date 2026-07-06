package mcp_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/mcp"
)

const ukbURL = "https://teco-ai-ukb-mcp.apps.gen3gpazne.cloudcoll.poste.it/api/v1/mcp"

func TestLiveUKBConnect(t *testing.T) {
	if os.Getenv("MCP_LIVE_TEST") == "" {
		t.Skip("set MCP_LIVE_TEST=1 to run live UKB MCP tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := config.MCPServerConfig{
		Name:    "ukb",
		URL:     ukbURL,
		Timeout: "30s",
	}

	c := mcp.New(cfg)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close(context.Background())

	tools := c.Tools()
	if len(tools) == 0 {
		t.Fatal("expected at least one tool, got none")
	}
	t.Logf("connected to UKB MCP, %d tools:", len(tools))
	for _, tool := range tools {
		desc := tool.Description
		if len(desc) > 60 {
			desc = desc[:60]
		}
		t.Logf("  - %s: %s", tool.Name, desc)
	}

	schemas := c.Schemas(ctx)
	if len(schemas) == 0 {
		t.Fatal("expected at least one schema entry")
	}
	for _, s := range schemas {
		name, _ := s["name"].(map[string]any)
		t.Logf("  schema name field: %v", name)
		fnMap, ok := s["function"].(map[string]any)
		if !ok {
			t.Errorf("schema missing 'function' key: %v", s)
			continue
		}
		t.Logf("  function: %v", fnMap["name"])
	}
}

func TestLiveUKBToolSet(t *testing.T) {
	if os.Getenv("MCP_LIVE_TEST") == "" {
		t.Skip("set MCP_LIVE_TEST=1 to run live UKB MCP tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := config.MCPServerConfig{
		Name:    "ukb",
		URL:     ukbURL,
		Timeout: "60s",
	}

	ts := mcp.NewToolSet([]*mcp.Client{mcp.New(cfg)})
	if err := ts.ConnectAll(ctx); err != nil {
		t.Fatalf("ConnectAll: %v", err)
	}
	defer ts.Close(context.Background())

	schemas := ts.Schemas(ctx)
	t.Logf("ToolSet exposed %d tool schemas", len(schemas))
	if len(schemas) == 0 {
		t.Fatal("expected schemas from toolset")
	}

	// Verify dispatch works for a known tool (get_domain_catalog with no args)
	result, ok := ts.Dispatch(ctx, "mcp_ukb_get_domain_catalog", "{}")
	if !ok {
		t.Fatal("Dispatch: tool not found")
	}
	if result == "" {
		t.Fatal("Dispatch returned empty result")
	}
	t.Logf("get_domain_catalog result length: %d bytes", len(result))
	if len(result) < 50 {
		t.Errorf("result suspiciously short: %q", result)
	}
}
