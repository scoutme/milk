package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/mcp"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := config.MCPServerConfig{
		Name:    "ukb-svil",
		URL:     "https://teco-ai-ukb-mcp.apps.gen3gpazne.cloudsvil.poste.it/api/v1/mcp",
		Timeout: "60s",
	}

	ts := mcp.NewToolSet([]*mcp.Client{mcp.New(cfg)})
	if err := ts.ConnectAll(ctx); err != nil {
		fmt.Printf("FAIL: %v\n", err)
		return
	}
	defer ts.Close(context.Background())

	raw, _ := ts.Dispatch(ctx, "mcp_ukb_svil_solve_task", `{"query":"tipi di libretti di risparmio postale","domain":"STORYTELLER","answer_mode":"answer_and_sources"}`)

	// raw is {"output":"<json-string>"} — unwrap
	var wrapper struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
		fmt.Println(raw)
		return
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(wrapper.Output), &result); err != nil {
		fmt.Println(wrapper.Output)
		return
	}
	fmt.Printf("Answer: %v\n\nConfidence: %v\nBand: %v\n\nSources:\n", result["text"], result["confidence"], result["confidence_band"])
	if srcs, ok := result["sources"].([]any); ok {
		for _, s := range srcs {
			if sm, ok := s.(map[string]any); ok {
				fmt.Printf("  - %v (score: %v)\n", sm["source_id"], sm["score"])
			}
		}
	}
}
