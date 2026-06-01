package local

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/scoutme/milk/internal/obs"
)

// --- Bedrock Converse API request/response types ---

type bedrockRequest struct {
	Messages   []bedrockMessage   `json:"messages"`
	System     []bedrockSystem    `json:"system,omitempty"`
	ToolConfig *bedrockToolConfig `json:"toolConfig,omitempty"`
}

type bedrockSystem struct {
	Text string `json:"text"`
}

type bedrockMessage struct {
	Role    string                `json:"role"`
	Content []bedrockContentBlock `json:"content"`
}

type bedrockContentBlock struct {
	Text       string             `json:"text,omitempty"`
	ToolUse    *bedrockToolUse    `json:"toolUse,omitempty"`
	ToolResult *bedrockToolResult `json:"toolResult,omitempty"`
}

type bedrockToolUse struct {
	ToolUseID string         `json:"toolUseId"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
}

type bedrockToolResult struct {
	ToolUseID string                     `json:"toolUseId"`
	Content   []bedrockToolResultContent `json:"content"`
}

type bedrockToolResultContent struct {
	Text string `json:"text"`
}

type bedrockToolConfig struct {
	Tools []bedrockTool `json:"tools"`
}

type bedrockTool struct {
	ToolSpec bedrockToolSpec `json:"toolSpec"`
}

type bedrockToolSpec struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	InputSchema bedrockInputSchema `json:"inputSchema"`
}

type bedrockInputSchema struct {
	JSON map[string]any `json:"json"`
}

// Synchronous Converse response (used for classification).
type bedrockConverseResponse struct {
	Output struct {
		Message bedrockMessage `json:"message"`
	} `json:"output"`
	StopReason string `json:"stopReason"`
	Usage      struct {
		InputTokens  int64 `json:"inputTokens"`
		OutputTokens int64 `json:"outputTokens"`
	} `json:"usage"`
}

// --- Streaming event structs ---

type bedrockContentBlockStartEvent struct {
	ContentBlockIndex int `json:"contentBlockIndex"`
	Start             struct {
		ToolUse *struct {
			ToolUseID string `json:"toolUseId"`
			Name      string `json:"name"`
		} `json:"toolUse,omitempty"`
	} `json:"start"`
}

type bedrockContentBlockDeltaEvent struct {
	ContentBlockIndex int `json:"contentBlockIndex"`
	Delta             struct {
		Text    string `json:"text,omitempty"`
		ToolUse *struct {
			Input string `json:"input"`
		} `json:"toolUse,omitempty"`
	} `json:"delta"`
}

type bedrockMetadataEvent struct {
	Usage struct {
		InputTokens  int64 `json:"inputTokens"`
		OutputTokens int64 `json:"outputTokens"`
	} `json:"usage"`
}

// --- Conversion helpers ---

// convertMessagesToConverse translates OpenAI-format messages to Bedrock Converse format.
// System messages (any position) are extracted into a separate slice.
// Consecutive tool-result messages are merged into a single user message (Bedrock requirement).
func convertMessagesToConverse(msgs []Message) ([]bedrockMessage, []bedrockSystem) {
	var system []bedrockSystem
	var result []bedrockMessage

	for _, m := range msgs {
		switch m.Role {
		case "system":
			if m.Content != "" {
				system = append(system, bedrockSystem{Text: m.Content})
			}

		case "user":
			if m.Content != "" {
				result = append(result, bedrockMessage{
					Role:    "user",
					Content: []bedrockContentBlock{{Text: m.Content}},
				})
			}

		case "tool":
			block := bedrockContentBlock{
				ToolResult: &bedrockToolResult{
					ToolUseID: m.ToolCallID,
					Content:   []bedrockToolResultContent{{Text: m.Content}},
				},
			}
			// Merge consecutive tool results into one user message (Bedrock requires this).
			if n := len(result); n > 0 &&
				result[n-1].Role == "user" &&
				len(result[n-1].Content) > 0 &&
				result[n-1].Content[0].ToolResult != nil {
				result[n-1].Content = append(result[n-1].Content, block)
			} else {
				result = append(result, bedrockMessage{
					Role:    "user",
					Content: []bedrockContentBlock{block},
				})
			}

		case "assistant":
			var content []bedrockContentBlock
			if m.Content != "" {
				content = append(content, bedrockContentBlock{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var input map[string]any
				json.Unmarshal([]byte(tc.Function.Arguments), &input) //nolint:errcheck
				if input == nil {
					input = map[string]any{}
				}
				content = append(content, bedrockContentBlock{
					ToolUse: &bedrockToolUse{
						ToolUseID: tc.ID,
						Name:      tc.Function.Name,
						Input:     input,
					},
				})
			}
			if len(content) > 0 {
				result = append(result, bedrockMessage{Role: "assistant", Content: content})
			}
		}
	}

	return result, system
}

// convertToolsToConverse translates OpenAI tool schemas to Bedrock ToolSpec format.
func convertToolsToConverse(tools []map[string]any) []bedrockTool {
	var result []bedrockTool
	for _, t := range tools {
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		params, _ := fn["parameters"].(map[string]any)
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		result = append(result, bedrockTool{
			ToolSpec: bedrockToolSpec{
				Name:        name,
				Description: desc,
				InputSchema: bedrockInputSchema{JSON: params},
			},
		})
	}
	return result
}

// converseEndpoint constructs the Bedrock Converse API URL.
// stream=true → converse-stream (AWS Event Stream); stream=false → converse (JSON).
// The model ID (which may be an ARN containing colons) is encoded with awsURIEncodeModel
// so that colons become %3A but slashes remain as path separators. The SigV4 transport
// then re-encodes each segment for the canonical URI (e.g. %3A → %253A).
func (a *Agent) converseEndpoint(stream bool) string {
	encodedModel := awsURIEncodeModel(a.model)
	if stream {
		return a.baseURL + "/model/" + encodedModel + "/converse-stream"
	}
	return a.baseURL + "/model/" + encodedModel + "/converse"
}

// bedrockStreamCompletion implements streamCompletion using the Bedrock Converse streaming API.
func (a *Agent) bedrockStreamCompletion(ctx context.Context, msgs []Message, tools []map[string]any, out io.Writer) (string, string, []toolCall, error) {
	bedrockMsgs, system := convertMessagesToConverse(msgs)
	bedrockTools := convertToolsToConverse(tools)

	reqBody := bedrockRequest{
		Messages: bedrockMsgs,
		System:   system,
	}
	if len(bedrockTools) > 0 {
		reqBody.ToolConfig = &bedrockToolConfig{Tools: bedrockTools}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", nil, err
	}
	if a.logContext {
		obs.LogPayload(a.converseEndpoint(true), body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.converseEndpoint(true), bytes.NewReader(body))
	if err != nil {
		return "", "", nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return "", "", nil, fmt.Errorf("bedrock unreachable: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(httpResp.Body)
		return "", "", nil, fmt.Errorf("bedrock error %d: %s", httpResp.StatusCode, b)
	}

	type partialTC struct {
		toolUseID string
		name      string
		inputBuf  strings.Builder
	}
	toolBlocks := map[int]*partialTC{}
	var textBuf strings.Builder

	done := false
	for !done {
		eventType, payload, err := readBedrockEvent(httpResp.Body)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return "", "", nil, fmt.Errorf("bedrock stream: %w", err)
		}

		switch eventType {
		case "contentBlockStart":
			// Payload is the inner struct directly (not wrapped in an outer key).
			var ev bedrockContentBlockStartEvent
			if json.Unmarshal(payload, &ev) == nil && ev.Start.ToolUse != nil {
				toolBlocks[ev.ContentBlockIndex] = &partialTC{
					toolUseID: ev.Start.ToolUse.ToolUseID,
					name:      ev.Start.ToolUse.Name,
				}
			}

		case "contentBlockDelta":
			// Payload is the inner struct directly (not wrapped in an outer key).
			var ev bedrockContentBlockDeltaEvent
			if json.Unmarshal(payload, &ev) != nil {
				continue
			}
			if ev.Delta.Text != "" {
				textBuf.WriteString(ev.Delta.Text)
				if out != nil {
					fmt.Fprint(out, ev.Delta.Text)
				}
			}
			if ev.Delta.ToolUse != nil {
				if tc := toolBlocks[ev.ContentBlockIndex]; tc != nil {
					tc.inputBuf.WriteString(ev.Delta.ToolUse.Input)
				}
			}

		case "messageStop":
			done = true

		case "metadata":
			var ev bedrockMetadataEvent
			if json.Unmarshal(payload, &ev) == nil {
				role := agentRoleForMetrics(a.escalationName)
				obs.RecordTokens(ctx, a.model, role, ev.Usage.InputTokens, ev.Usage.OutputTokens)
				if a.onTokens != nil {
					a.onTokens(a.model, role, ev.Usage.InputTokens, ev.Usage.OutputTokens)
				}
			}

		default:
			// exception variants — surface them as errors
			if strings.Contains(eventType, "Exception") || strings.Contains(eventType, "exception") {
				return "", "", nil, fmt.Errorf("bedrock %s: %s", eventType, string(payload))
			}
		}
	}

	// Collect tool calls ordered by content block index.
	type indexedTC struct {
		idx int
		tc  toolCall
	}
	var indexed []indexedTC
	for idx, tc := range toolBlocks {
		indexed = append(indexed, indexedTC{
			idx: idx,
			tc: toolCall{
				ID:   tc.toolUseID,
				Type: "function",
				Function: toolCallFunction{
					Name:      tc.name,
					Arguments: tc.inputBuf.String(),
				},
			},
		})
	}
	sort.Slice(indexed, func(i, j int) bool { return indexed[i].idx < indexed[j].idx })
	var tcList []toolCall
	for _, itc := range indexed {
		tcList = append(tcList, itc.tc)
	}

	if len(tcList) > 0 {
		if out != nil && textBuf.Len() > 0 {
			fmt.Fprintln(out)
		}
		return textBuf.String(), "", tcList, nil
	}
	if out != nil && textBuf.Len() > 0 {
		fmt.Fprintln(out)
	}
	return textBuf.String(), "", nil, nil
}

// bedrockClassify uses the synchronous Bedrock Converse API for routing classification.
func (a *Agent) bedrockClassify(ctx context.Context, prompt string) (bool, error) {
	classifyPrompt := `You are a routing classifier. Respond with exactly one word: "primary" or "escalate".
Respond "escalate" only if the task clearly requires: complex multi-file refactoring, architectural design decisions, or tasks that require deep reasoning beyond coding assistance.
Respond "primary" for: shell commands, file reading, grep, simple code questions, debugging, writing small functions.

Task: ` + prompt

	reqBody := bedrockRequest{
		Messages: []bedrockMessage{
			{Role: "user", Content: []bedrockContentBlock{{Text: classifyPrompt}}},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return false, err
	}
	if a.logContext {
		obs.LogPayload(a.converseEndpoint(false)+" [classify]", body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.converseEndpoint(false), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("bedrock unreachable: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(httpResp.Body)
		return false, fmt.Errorf("bedrock error %d: %s", httpResp.StatusCode, b)
	}

	var result bedrockConverseResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&result); err != nil {
		return false, err
	}
	obs.RecordTokens(ctx, a.model, "router", result.Usage.InputTokens, result.Usage.OutputTokens)
	for _, block := range result.Output.Message.Content {
		if block.Text != "" {
			return strings.HasPrefix(strings.TrimSpace(strings.ToLower(block.Text)), "escalate"), nil
		}
	}
	return false, nil
}

// --- AWS Event Stream decoder ---

// readBedrockEvent reads one event from an AWS Event Stream (binary framing).
//
// Frame layout (all integers big-endian):
//
//	[0:4]   total byte length (includes all fields including itself)
//	[4:8]   headers byte length
//	[8:12]  prelude CRC32
//	[12:12+headersLen] headers
//	[12+headersLen : totalLen-4] payload (JSON)
//	[totalLen-4:totalLen] message CRC32
func readBedrockEvent(r io.Reader) (eventType string, payload []byte, err error) {
	var prelude [8]byte
	if _, err = io.ReadFull(r, prelude[:]); err != nil {
		return
	}
	totalLen := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])

	// Skip prelude CRC (4 bytes).
	if _, err = io.ReadFull(r, make([]byte, 4)); err != nil {
		return
	}

	headerBytes := make([]byte, headersLen)
	if _, err = io.ReadFull(r, headerBytes); err != nil {
		return
	}

	// payloadLen = total - prelude(8) - preludeCRC(4) - headers(headersLen) - messageCRC(4)
	payloadLen := int(totalLen) - 16 - int(headersLen)
	if payloadLen < 0 {
		err = fmt.Errorf("invalid bedrock event: negative payload (%d)", payloadLen)
		return
	}
	payload = make([]byte, payloadLen)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}

	// Skip message CRC (4 bytes).
	if _, err = io.ReadFull(r, make([]byte, 4)); err != nil {
		return
	}

	eventType = parseBedrockHeader(headerBytes, ":event-type")
	if eventType == "" {
		if parseBedrockHeader(headerBytes, ":message-type") == "exception" {
			eventType = parseBedrockHeader(headerBytes, ":exception-type")
			if eventType == "" {
				eventType = "exception"
			}
		}
	}
	return
}

// parseBedrockHeader extracts a named string header value from an encoded headers block.
// Only handles value type 7 (string); stops at the first unrecognised type.
func parseBedrockHeader(data []byte, target string) string {
	i := 0
	for i < len(data) {
		nameLen := int(data[i])
		i++
		if i+nameLen > len(data) {
			return ""
		}
		name := string(data[i : i+nameLen])
		i += nameLen
		if i >= len(data) {
			return ""
		}
		vtype := data[i]
		i++
		if vtype != 7 { // only string type supported; can't skip unknown types safely
			return ""
		}
		if i+2 > len(data) {
			return ""
		}
		vlen := int(binary.BigEndian.Uint16(data[i : i+2]))
		i += 2
		if i+vlen > len(data) {
			return ""
		}
		val := string(data[i : i+vlen])
		i += vlen
		if name == target {
			return val
		}
	}
	return ""
}
