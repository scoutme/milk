package local

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/scoutme/milk/internal/obs"
)

// responsesRequest is the request body for the OpenAI Responses API (/v1/responses).
type responsesRequest struct {
	Model       string           `json:"model"`
	Input       []responsesInput `json:"input"`
	Tools       []map[string]any `json:"tools,omitempty"`
	Stream      bool             `json:"stream"`
	Temperature float64          `json:"temperature"`
}

// responsesInput is a single item in the Responses API input array.
// It covers role-based messages (user/assistant/system), function_call items,
// and function_call_output items via the union of their fields with omitempty.
type responsesInput struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

// responsesEvent is the data payload of a Responses API SSE event.
type responsesEvent struct {
	Type        string               `json:"type"`
	Delta       string               `json:"delta,omitempty"`
	OutputIndex int                  `json:"output_index"`
	Item        *responsesOutputItem `json:"item,omitempty"`
	Response    *responsesUsageBody  `json:"response,omitempty"`
}

type responsesOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
}

type responsesUsageBody struct {
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

// messagesToResponses converts a Chat Completions message slice to Responses API input items.
// assistant tool_calls → function_call items; tool role → function_call_output items.
func messagesToResponses(msgs []Message) []responsesInput {
	items := make([]responsesInput, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			items = append(items, responsesInput{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: m.Content,
			})
		case "assistant":
			for _, tc := range m.ToolCalls {
				items = append(items, responsesInput{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
			if m.Content != "" {
				items = append(items, responsesInput{Role: "assistant", Content: m.Content})
			}
		default: // user, system
			items = append(items, responsesInput{Role: m.Role, Content: m.Content})
		}
	}
	return items
}

// convertToolsToResponsesFormat converts Chat Completions tool schemas
// (nested under "function") to the flat Responses API format.
func convertToolsToResponsesFormat(tools []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fn, ok := t["function"].(map[string]any)
		if !ok {
			result = append(result, t)
			continue
		}
		flat := map[string]any{"type": "function"}
		for k, v := range fn {
			flat[k] = v
		}
		result = append(result, flat)
	}
	return result
}

// responsesStreamCompletion sends a streaming request to the OpenAI Responses API.
func (a *Agent) responsesStreamCompletion(ctx context.Context, msgs []Message, tools []map[string]any, out io.Writer) (string, string, []toolCall, error) {
	req := responsesRequest{
		Model:       a.model,
		Input:       messagesToResponses(msgs),
		Tools:       convertToolsToResponsesFormat(tools),
		Stream:      true,
		Temperature: 0.2,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", "", nil, err
	}
	if a.logContext {
		obs.LogPayload(a.inferenceURL(), body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.inferenceURL(), bytes.NewReader(body))
	if err != nil {
		return "", "", nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	inferenceStart := time.Now()
	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		obs.Inc(ctx, inferenceScope, "milk.inference.errors",
			attribute.String("model", a.model),
			attribute.String("agent", agentRoleForMetrics(a.escalationName)),
			attribute.String("kind", "http"),
		)
		return "", "", nil, fmt.Errorf("inference server unreachable: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(httpResp.Body)
		obs.Inc(ctx, inferenceScope, "milk.inference.errors",
			attribute.String("model", a.model),
			attribute.String("agent", agentRoleForMetrics(a.escalationName)),
			attribute.String("kind", "http"),
		)
		return "", "", nil, fmt.Errorf("inference server error %d: %s", httpResp.StatusCode, b)
	}

	det := NewStreamDetector(a.detectedFormat)
	partialTools := map[int]*toolCall{}
	var textBuf strings.Builder

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	toolCalls, promptTokens, completionTokens, err := a.scanResponsesSSE(scanner, det, partialTools, &textBuf, out)
	if err != nil {
		return "", "", nil, err
	}

	role := agentRoleForMetrics(a.escalationName)
	obs.RecordDuration(ctx, inferenceScope, "milk.inference.latency_ms", time.Since(inferenceStart),
		attribute.String("model", a.model),
		attribute.String("agent", role),
		attribute.String("provider", "responses"),
	)
	obs.RecordTokens(ctx, a.model, role, promptTokens, completionTokens)
	if a.onTokens != nil {
		a.onTokens(a.model, role, promptTokens, completionTokens)
	}

	if det.Format != ToolFormatUnknown {
		a.detectedFormat = det.Format
	}
	return a.classifyStreamResult(det, toolCalls, textBuf.String(), out)
}

// scanResponsesSSE reads SSE lines from a Responses API stream, dispatching on
// the "type" field in each data payload.
func (a *Agent) scanResponsesSSE(
	scanner *bufio.Scanner,
	det *StreamDetector,
	partialTools map[int]*toolCall,
	textBuf *strings.Builder,
	out io.Writer,
) ([]toolCall, int64, int64, error) {
	dbg := a.debugLog
	var promptTokens, completionTokens int64
	for scanner.Scan() {
		line := scanner.Text()
		if dbg != nil {
			fmt.Fprintln(dbg, line) //nolint:errcheck
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var ev responsesEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			if dbg != nil {
				fmt.Fprintf(dbg, "[skip:json-error] %v | raw: %s\n", err, data) //nolint:errcheck
			}
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			processContentToken(ev.Delta, det, textBuf, out)
		case "response.output_item.added":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				partialTools[ev.OutputIndex] = &toolCall{
					ID:   ev.Item.CallID,
					Type: "function",
					Function: toolCallFunction{
						Name: ev.Item.Name,
					},
				}
			}
		case "response.function_call_arguments.delta":
			if pt, ok := partialTools[ev.OutputIndex]; ok {
				pt.Function.Arguments += ev.Delta
			}
		case "response.completed":
			if ev.Response != nil && ev.Response.Usage != nil {
				promptTokens = ev.Response.Usage.InputTokens
				completionTokens = ev.Response.Usage.OutputTokens
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, 0, err
	}
	return collectNativeToolCalls(partialTools), promptTokens, completionTokens, nil
}

// responsesClassify classifies a prompt using the Responses API (non-streaming).
func (a *Agent) responsesClassify(ctx context.Context, prompt string) (bool, error) {
	classifyPrompt := `Respond with exactly one word: "primary" or "escalate".
Use "escalate" only when the task clearly requires complex multi-file refactoring, architectural design decisions, or deep reasoning beyond coding assistance.
Use "primary" for shell commands, file reading, grep, simple code questions, debugging, and writing small functions.

Task: ` + prompt

	req := responsesRequest{
		Model:       a.model,
		Input:       []responsesInput{{Role: "user", Content: classifyPrompt}},
		Stream:      false,
		Temperature: 0,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return false, err
	}
	if a.logContext {
		obs.LogPayload(a.inferenceURL()+" [classify]", body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.inferenceURL(), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("inference server unreachable: %w", err)
	}
	defer httpResp.Body.Close()

	var result struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage,omitempty"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&result); err != nil {
		return false, err
	}
	if result.Usage != nil {
		obs.RecordTokens(ctx, a.model, "router", result.Usage.InputTokens, result.Usage.OutputTokens)
	}
	for _, item := range result.Output {
		if item.Type == "message" {
			for _, c := range item.Content {
				if c.Type == "output_text" {
					return strings.HasPrefix(strings.TrimSpace(strings.ToLower(c.Text)), "escalate"), nil
				}
			}
		}
	}
	return false, nil
}
