package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/scoutme/milk/internal/mockrecorder"
)

var (
	serverPort     int
	serverName     string
	serverProvider string
	serverRecDir   string
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start an OpenAI-compatible SSE mock server",
	Long: `Starts an HTTP server that speaks the OpenAI chat completions protocol.

Responses are generated from the input text:
  "response for: <input>, context received: <context_summary>"

To trigger a tool-call response instead, include [tool:<name>,<args>] in the
prompt body. The server will emit a tool-call object in the response.

Recording is written to --rec-dir/<name>-sessions.jsonl and
--rec-dir/<name>-metrics.jsonl.`,
	RunE: runServer,
}

func init() {
	serverCmd.Flags().IntVar(&serverPort, "port", 19999, "Port to listen on")
	serverCmd.Flags().StringVar(&serverName, "name", "mock", "Name prefix for recording files")
	serverCmd.Flags().StringVar(&serverProvider, "provider", "openai", "Provider label (informational only)")
	serverCmd.Flags().StringVar(&serverRecDir, "rec-dir", defaultRecDir(), "Directory for recording files")
}

func defaultRecDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/milk-mock"
	}
	return filepath.Join(home, ".milk", "mock-recordings")
}

func runServer(_ *cobra.Command, _ []string) error {
	rec, err := mockrecorder.New(serverName, serverRecDir)
	if err != nil {
		return fmt.Errorf("recorder: %w", err)
	}
	defer rec.Close() //nolint:errcheck

	mux := http.NewServeMux()

	// Health check.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	// OpenAI-compatible chat completions.
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleChatCompletions(w, r, rec)
	})

	addr := fmt.Sprintf(":%d", serverPort)
	fmt.Printf("milk-mock server listening on %s (name=%s, rec-dir=%s)\n", addr, serverName, serverRecDir)
	return http.ListenAndServe(addr, mux)
}

// chatRequest is the incoming OpenAI chat completions request body.
type chatRequest struct {
	Model    string               `json:"model"`
	Messages []map[string]any     `json:"messages"`
	Stream   bool                 `json:"stream"`
	Tools    []map[string]any     `json:"tools,omitempty"`
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request, rec *mockrecorder.Recorder) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}

	// Extract the last user message as the input.
	input := lastUserMessage(req.Messages)
	contextSummary := mockrecorder.ExtractContextSummary(req.Messages)
	toolCalls := mockrecorder.ParseToolCalls(input)

	if req.Stream {
		serveSSE(w, r, req, rec, input, contextSummary, toolCalls)
	} else {
		serveSync(w, req, rec, input, contextSummary, toolCalls)
	}
}

func serveSSE(w http.ResponseWriter, _ *http.Request, req chatRequest, rec *mockrecorder.Recorder, input, contextSummary string, toolCalls []mockrecorder.ToolCallRecord) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	id := fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixMilli())
	model := req.Model
	if model == "" {
		model = "mock-model"
	}

	var response string
	if len(toolCalls) > 0 {
		// Emit a tool-call delta for each parsed tool call.
		for i, tc := range toolCalls {
			argsJSON, _ := json.Marshal(tc.Args)
			toolCallChunk := map[string]any{
				"id":      id,
				"object":  "chat.completion.chunk",
				"model":   model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": i,
							"id":    fmt.Sprintf("call_%d", i),
							"type":  "function",
							"function": map[string]any{
								"name":      tc.Name,
								"arguments": string(argsJSON),
							},
						}},
					},
					"finish_reason": nil,
				}},
			}
			sendSSEChunk(w, flusher, toolCallChunk)
		}
		// Final finish chunk.
		finishChunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
		}
		sendSSEChunk(w, flusher, finishChunk)
		response = fmt.Sprintf("[tool-call: %s]", toolCalls[0].Name)
	} else {
		// Stream the text response word by word.
		response = buildTextResponse(input, contextSummary)
		words := strings.Fields(response)
		for i, word := range words {
			text := word
			if i < len(words)-1 {
				text += " "
			}
			chunk := map[string]any{
				"id":      id,
				"object":  "chat.completion.chunk",
				"model":   model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
			}
			sendSSEChunk(w, flusher, chunk)
		}
		// Final chunk.
		doneChunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		}
		sendSSEChunk(w, flusher, doneChunk)
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()

	_ = rec.WriteTurn(input, contextSummary, toolCalls, response)
	_ = rec.WriteMetrics((len(input) + len(contextSummary) + len(response)) / 4)
}

func serveSync(w http.ResponseWriter, req chatRequest, rec *mockrecorder.Recorder, input, contextSummary string, toolCalls []mockrecorder.ToolCallRecord) {
	w.Header().Set("Content-Type", "application/json")
	model := req.Model
	if model == "" {
		model = "mock-model"
	}

	var response string
	var choices []map[string]any

	if len(toolCalls) > 0 {
		response = fmt.Sprintf("[tool-call: %s]", toolCalls[0].Name)
		tcList := make([]map[string]any, 0, len(toolCalls))
		for i, tc := range toolCalls {
			argsJSON, _ := json.Marshal(tc.Args)
			tcList = append(tcList, map[string]any{
				"id":   fmt.Sprintf("call_%d", i),
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(argsJSON),
				},
			})
		}
		choices = []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": nil, "tool_calls": tcList},
			"finish_reason": "tool_calls",
		}}
	} else {
		response = buildTextResponse(input, contextSummary)
		choices = []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": response},
			"finish_reason": "stop",
		}}
	}

	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"model":   model,
		"choices": choices,
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
	}
	json.NewEncoder(w).Encode(resp) //nolint:errcheck

	_ = rec.WriteTurn(input, contextSummary, toolCalls, response)
	_ = rec.WriteMetrics((len(input) + len(contextSummary) + len(response)) / 4)
}

func sendSSEChunk(w http.ResponseWriter, flusher http.Flusher, v map[string]any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", b)
	flusher.Flush()
}

func buildTextResponse(input, contextSummary string) string {
	if contextSummary != "" {
		return fmt.Sprintf("response for: %s, context received: %s", input, contextSummary)
	}
	return fmt.Sprintf("response for: %s", input)
}

func lastUserMessage(messages []map[string]any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		role, _ := messages[i]["role"].(string)
		if role == "user" {
			content, _ := messages[i]["content"].(string)
			return content
		}
	}
	return ""
}
