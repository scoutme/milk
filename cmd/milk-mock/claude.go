package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/scoutme/milk/internal/mockrecorder"
)

var (
	claudeSessionID       string
	claudeResume          string
	claudeOutputFormat    string
	claudePrint           bool
	claudeSkipPerms       bool
	claudeRecDir          string
	claudeRecName         string
	claudeVerbose         bool
	claudeIncludePartials bool
	claudeAllowedTools    string
	claudeAddDirs         []string
	claudeAppendSysPrompt []string
	claudeMCPConfig       string
)

// claudeCmd mimics the `claude --print --output-format stream-json` interface.
var claudeCmd = &cobra.Command{
	Use:                "claude [flags] -- <prompt>",
	Short:              "Drop-in mock for the claude CLI subprocess",
	DisableFlagParsing: false,
	RunE:               runClaude,
}

func init() {
	claudeCmd.Flags().StringVar(&claudeSessionID, "session-id", "", "Session ID to use (recorded but not enforced)")
	claudeCmd.Flags().StringVar(&claudeResume, "resume", "", "Session ID to resume (recorded but not enforced)")
	claudeCmd.Flags().StringVar(&claudeOutputFormat, "output-format", "stream-json", "Output format (must be stream-json)")
	claudeCmd.Flags().BoolVar(&claudePrint, "print", false, "Non-interactive mode (required for subprocess use)")
	claudeCmd.Flags().BoolVar(&claudeSkipPerms, "dangerously-skip-permissions", false, "Skip permission prompts")
	claudeCmd.Flags().StringVar(&claudeRecDir, "rec-dir", defaultRecDir(), "Directory for recording files")
	claudeCmd.Flags().StringVar(&claudeRecName, "name", "mock-claude", "Name prefix for recording files")
	claudeCmd.Flags().BoolVar(&claudeVerbose, "verbose", false, "Verbose output")
	claudeCmd.Flags().BoolVar(&claudeIncludePartials, "include-partial-messages", false, "Include partial message events")
	claudeCmd.Flags().StringVar(&claudeAllowedTools, "allowedTools", "", "Comma-separated list of allowed tools (ignored by mock)")
	claudeCmd.Flags().StringArrayVar(&claudeAddDirs, "add-dir", nil, "Extra directories to trust (ignored by mock)")
	claudeCmd.Flags().StringArrayVar(&claudeAppendSysPrompt, "append-system-prompt-file", nil, "System prompt files (recorded by mock)")
	claudeCmd.Flags().StringVar(&claudeMCPConfig, "mcp-config", "", "MCP config file (ignored by mock)")
	claudeCmd.Flags().StringVar(&claudeAllowedTools, "permission-prompt-tool", "", "Permission prompt tool (ignored by mock)")
}

func runClaude(cmd *cobra.Command, args []string) error {
	// The prompt follows -- in args.
	prompt := strings.TrimSpace(strings.Join(args, " "))

	// Also try reading from stdin if no prompt given.
	if prompt == "" {
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			sc := bufio.NewScanner(os.Stdin)
			var lines []string
			for sc.Scan() {
				lines = append(lines, sc.Text())
			}
			_ = sc.Err()
			prompt = strings.TrimSpace(strings.Join(lines, "\n"))
		}
	}

	// Determine effective session ID.
	sessID := claudeSessionID
	if claudeResume != "" {
		sessID = claudeResume
	}
	if sessID == "" {
		sessID = fmt.Sprintf("mock-%d", time.Now().UnixNano())
	}

	// Collect context summary from --append-system-prompt-file contents.
	contextSummary := readSysPromptFiles(claudeAppendSysPrompt)

	toolCalls := mockrecorder.ParseToolCalls(prompt)

	var response string
	if len(toolCalls) > 0 {
		response = fmt.Sprintf("[tool-call: %s]", toolCalls[0].Name)
	} else {
		response = buildTextResponse(prompt, mockrecorder.ExtractContextSummary([]map[string]any{
			{"role": "system", "content": contextSummary},
			{"role": "user", "content": prompt},
		}))
	}

	// Emit NDJSON stream matching claude --output-format stream-json.
	emitSystemEvent(sessID)
	if claudeIncludePartials && len(toolCalls) == 0 {
		emitTextDeltas(response)
	}
	if len(toolCalls) > 0 {
		emitToolCallEvent(sessID, toolCalls)
	}
	emitResultEvent(sessID, response, len(toolCalls) > 0)

	// Record turn.
	rec, err := mockrecorder.New(claudeRecName, claudeRecDir)
	if err == nil {
		rec.SetSessionID(sessID)
		_ = rec.WriteTurn(prompt, contextSummary, toolCalls, response)
		_ = rec.WriteMetrics((len(prompt) + len(response)) / 4)
		_ = rec.Close()
	}

	return nil
}

// emitSystemEvent writes the initial system event with the session ID.
func emitSystemEvent(sessID string) {
	writeNDJSON(map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": sessID,
	})
}

// emitTextDeltas writes incremental text_delta stream events for partial streaming.
func emitTextDeltas(text string) {
	words := strings.Fields(text)
	for i, w := range words {
		chunk := w
		if i < len(words)-1 {
			chunk += " "
		}
		writeNDJSON(map[string]any{
			"type": "stream_event",
			"event": map[string]any{
				"type": "content_block_delta",
				"delta": map[string]any{
					"type": "text_delta",
					"text": chunk,
				},
			},
		})
	}
}

// emitToolCallEvent writes a mock assistant message containing tool-use blocks.
func emitToolCallEvent(sessID string, toolCalls []mockrecorder.ToolCallRecord) {
	content := make([]map[string]any, 0, len(toolCalls))
	for i, tc := range toolCalls {
		argsJSON, _ := json.Marshal(tc.Args)
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    fmt.Sprintf("toolu_mock_%d", i),
			"name":  tc.Name,
			"input": json.RawMessage(argsJSON),
		})
	}
	writeNDJSON(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": content,
		},
		"session_id": sessID,
	})
}

// emitResultEvent writes the final result NDJSON event.
func emitResultEvent(sessID, text string, isToolCall bool) {
	result := text
	if isToolCall {
		result = ""
	}
	writeNDJSON(map[string]any{
		"type":       "result",
		"subtype":    "success",
		"result":     result,
		"session_id": sessID,
		"is_error":   false,
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": 20,
		},
	})
}

func writeNDJSON(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

// readSysPromptFiles reads the contents of the given file paths and joins them.
func readSysPromptFiles(paths []string) string {
	var parts []string
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Clean(p))
		if err == nil {
			parts = append(parts, strings.TrimSpace(string(b)))
		}
	}
	return strings.Join(parts, "\n")
}
