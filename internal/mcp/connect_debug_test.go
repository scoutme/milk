package mcp

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
)

func TestLiveConnect(t *testing.T) {
	const testURL = "https://teco-ai-ukb-mcp.apps.gen3gpazne.cloudsvil.poste.it/api/v1/mcp"

	// Skip if server is unreachable (e.g. CI without VPN access).
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer checkCancel()
	req, _ := http.NewRequestWithContext(checkCtx, http.MethodHead, testURL, nil)
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Skipf("skipping: MCP server unreachable (%v)", err)
	}

	cfg := config.MCPServerConfig{
		Name:    "ukb-svil",
		URL:     testURL,
		Timeout: "60s",
	}
	c := New(cfg)
	err := c.Connect(context.Background())
	if err != nil {
		t.Fatalf("CONNECT ERROR: %v", err)
	}
	fmt.Printf("Connected! Tools:\n")
	for _, tool := range c.Tools() {
		fmt.Printf("  - %s: %s\n", tool.Name, tool.Description)
	}
}
