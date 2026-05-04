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
)

const maxToolIterations = 10

// EscalationSignal is returned when the local model requests escalation to Claude.
type EscalationSignal struct {
	Reason string
}

func (e *EscalationSignal) Error() string {
	return "escalate: " + e.Reason
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Index    int              `json:"index"`
	Type     string           `json:"type"`
	Function toolCallFunction `json:"function"`
}

type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatRequest struct {
	Model       string      `json:"model"`
	Messages    []Message   `json:"Messages"`
	Tools       []map[string]any `json:"tools,omitempty"`
	Stream      bool        `json:"stream"`
	Temperature float64     `json:"temperature"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// Agent is a local LLM agent backed by a llama.cpp OpenAI-compatible server.
type Agent struct {
	baseURL string
	model   string
	client  *http.Client
}

func New(baseURL, model string) *Agent {
	return &Agent{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

// Run executes a prompt with the given conversation history, streaming tokens
// to out. Returns an EscalationSignal error if the model requests escalation.
// history is the prior turns; userPrompt is the new user Message.
func (a *Agent) Run(ctx context.Context, history []Message, userPrompt string, out io.Writer) ([]Message, error) {
	msgs := append(history, Message{Role: "user", Content: userPrompt})
	tools := schemas()

	for i := 0; i < maxToolIterations; i++ {
		resp, toolCalls, err := a.streamCompletion(ctx, msgs, tools, out)
		if err != nil {
			return msgs, err
		}

		if len(toolCalls) == 0 {
			// Final text response
			msgs = append(msgs, Message{Role: "assistant", Content: resp})
			return msgs, nil
		}

		// Accumulate assistant Message with tool calls
		msgs = append(msgs, Message{Role: "assistant", ToolCalls: toolCalls})

		// Execute each tool call
		for _, tc := range toolCalls {
			result, escalate := dispatchTool(ctx, tc.Function.Name, tc.Function.Arguments)
			if escalate {
				var escalateArgs struct {
					Reason string `json:"reason"`
				}
				json.Unmarshal([]byte(tc.Function.Arguments), &escalateArgs) //nolint:errcheck
				return msgs, &EscalationSignal{Reason: escalateArgs.Reason}
			}
			msgs = append(msgs, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
	}

	return msgs, fmt.Errorf("exceeded maximum tool iterations (%d)", maxToolIterations)
}

// streamCompletion sends a chat completion request and streams the response.
// Returns the accumulated text content and any tool calls.
func (a *Agent) streamCompletion(ctx context.Context, msgs []Message, tools []map[string]any, out io.Writer) (string, []toolCall, error) {
	req := chatRequest{
		Model:       a.model,
		Messages:    msgs,
		Tools:       tools,
		Stream:      true,
		Temperature: 0.2,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return "", nil, fmt.Errorf("llama.cpp unreachable: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(httpResp.Body)
		return "", nil, fmt.Errorf("llama.cpp error %d: %s", httpResp.StatusCode, b)
	}

	var (
		textBuf   strings.Builder
		toolCalls []toolCall
	)

	// Partial tool call accumulator indexed by tool call index
	partialTools := map[int]*toolCall{}

	scanner := bufio.NewScanner(httpResp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		for _, choice := range chunk.Choices {
			if t := choice.Delta.Content; t != "" {
				textBuf.WriteString(t)
				fmt.Fprint(out, t)
			}
			for _, tc := range choice.Delta.ToolCalls {
				pt, ok := partialTools[tc.Index]
				if !ok {
					pt = &toolCall{Type: "function"}
					partialTools[tc.Index] = pt
				}
				if tc.ID != "" {
					pt.ID = tc.ID
				}
				pt.Function.Name += tc.Function.Name
				pt.Function.Arguments += tc.Function.Arguments
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", nil, err
	}

	// Flush newline after streamed text
	if textBuf.Len() > 0 {
		fmt.Fprintln(out)
	}

	for i := 0; i < len(partialTools); i++ {
		if tc := partialTools[i]; tc != nil && tc.Function.Name != "" {
			toolCalls = append(toolCalls, *tc)
		}
	}

	return textBuf.String(), toolCalls, nil
}

// Classify asks the model to classify whether a prompt should be handled locally
// or escalated to Claude. Returns true if escalation is recommended.
func (a *Agent) Classify(ctx context.Context, prompt string) (bool, error) {
	classifyPrompt := `You are a routing classifier. Respond with exactly one word: "local" or "escalate".
Respond "escalate" only if the task clearly requires: complex multi-file refactoring, architectural design decisions, or tasks that require deep reasoning beyond coding assistance.
Respond "local" for: shell commands, file reading, grep, simple code questions, debugging, writing small functions.

Task: ` + prompt

	req := chatRequest{
		Model: a.model,
		Messages: []Message{
			{Role: "user", Content: classifyPrompt},
		},
		Stream:      false,
		Temperature: 0,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return false, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("llama.cpp unreachable: %w", err)
	}
	defer httpResp.Body.Close()

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"Message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&result); err != nil {
		return false, err
	}

	if len(result.Choices) == 0 {
		return false, nil
	}
	answer := strings.TrimSpace(strings.ToLower(result.Choices[0].Message.Content))
	return strings.HasPrefix(answer, "escalate"), nil
}

// Ping checks whether the llama.cpp server is reachable.
func (a *Agent) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("llama.cpp unreachable at %s: %w", a.baseURL, err)
	}
	resp.Body.Close()
	return nil
}
