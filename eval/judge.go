package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"

	"github.com/scoutme/milk/internal/config"
)

// JudgePrompt is the template sent to the LLM judge for each scored turn.
var JudgePrompt = template.Must(template.New("judge").Parse(`You are evaluating an AI agent's response to a task.

## Task
{{.Description}}

## Prompt
{{.Prompt}}

## Agent Response
{{.Response}}
{{if .FileChanges}}

## File Changes Made by the Agent
{{range .FileChanges}}{{if .IsNew}}**NEW FILE: {{.Path}}**
` + "```" + `
{{.After}}` + "```" + `
{{end}}{{if .Modified}}**MODIFIED: {{.Path}}**
` + "```diff" + `
{{.Before}}` + " → " + `{{.After}}` + "```" + `
{{end}}{{end}}{{end}}

## Scoring Criteria
{{range .Criteria}}- **{{.Criterion}}** ({{.Scoring}}, weight {{.Weight}}): {{.Description}}
{{end}}
Score each criterion. Output a JSON array: [{"criterion":"...","score":0.0,"reasoning":"..."}]
For "binary" scoring: score is 0.0 or 1.0.
For "scale_1_5" scoring: score is 1.0 to 5.0.
Consider both the text response AND the file changes when scoring correctness.
In "reasoning", never use a double-quote character — use single quotes or backticks instead
when quoting code, identifiers, or messages, so the JSON stays valid.
Output ONLY the JSON array, no other text.`))

// judgeInput holds the template data for JudgePrompt.
type judgeInput struct {
	Description string
	Prompt      string
	Response    string
	FileChanges []FileChange
	Criteria    []RubricCriterion
}

// Judge sends agent responses to a local LLM for rubric-based scoring.
type Judge struct {
	// URL is the base URL of the inference server (e.g. "http://localhost:8080").
	// If empty, the URL is resolved from ~/.milk/config.json at call time.
	URL string

	// Model overrides the model name sent in the chat completion request.
	// If empty, the model from config is used.
	Model string

	// APIKey is the Bearer token for authentication. If empty, no auth header is sent.
	APIKey string

	// HTTPClient is the HTTP client used for requests. If nil, http.DefaultClient is used.
	HTTPClient *http.Client
}

// NewJudgeFromConfig creates a Judge from ~/.milk/config.json. If agentName is
// empty, it uses the primary agent (same resolution milk itself uses); otherwise
// it looks up that agent by name.
func NewJudgeFromConfig(agentName string) (*Judge, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading milk config: %w", err)
	}

	var agent config.AgentConfig
	if agentName == "" {
		agent = cfg.ActiveAgent()
	} else {
		var ok bool
		agent, ok = cfg.AgentByName(agentName)
		if !ok {
			return nil, fmt.Errorf("no agent named %q configured", agentName)
		}
	}
	if agent.URL == "" {
		return nil, fmt.Errorf("no URL configured for judge agent %q", agent.Name)
	}
	return &Judge{
		URL:    strings.TrimRight(agent.URL, "/"),
		Model:  agent.Model,
		APIKey: agent.ResolvedAPIKey(),
	}, nil
}

// chatRequest is the wire format for /v1/chat/completions.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the wire format for the OpenAI chat completions response.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Score sends each turn's response to the LLM and parses the returned scores.
// For single-turn scenarios (len(results)==1), the scenario-level Rubric is used.
// For multi-turn scenarios, each result[i] is scored against turns[i].Rubric.
func (j *Judge) Score(ctx context.Context, scenario Scenario, results []RunResult) ([]JudgeScore, error) {
	var allScores []JudgeScore

	for i, result := range results {
		// Determine the rubric for this turn.
		var rubric []RubricCriterion
		if len(scenario.Turns) > i {
			rubric = scenario.Turns[i].Rubric
		} else if i == 0 {
			rubric = scenario.Rubric
		}
		if len(rubric) == 0 {
			continue // nothing to score
		}

		// Determine the prompt for this turn.
		prompt := scenario.Prompt
		if len(scenario.Turns) > i {
			prompt = scenario.Turns[i].Prompt
		}

		scores, err := j.scoreTurn(ctx, scenario.Description, prompt, result.Response, result.FileChanges, rubric)
		if err != nil {
			return allScores, fmt.Errorf("scoring turn %d: %w", i, err)
		}
		allScores = append(allScores, scores...)
	}

	return allScores, nil
}

// scoreTurn renders the judge prompt, calls the LLM, and parses the JSON response.
// Unparseable judge output is re-asked up to judgeAttempts times with an explicit
// correction before the turn is failed — a single flaky judgement should not
// cost a run its score.
func (j *Judge) scoreTurn(ctx context.Context, description, prompt, response string, fileChanges []FileChange, rubric []RubricCriterion) ([]JudgeScore, error) {
	// Render prompt.
	var buf bytes.Buffer
	if err := JudgePrompt.Execute(&buf, judgeInput{
		Description: description,
		Prompt:      prompt,
		Response:    response,
		FileChanges: fileChanges,
		Criteria:    rubric,
	}); err != nil {
		return nil, fmt.Errorf("rendering judge prompt: %w", err)
	}

	// Attempt the judgement. Judge models routinely emit invalid JSON on the
	// first try, so a parse failure re-asks with an explicit correction up to
	// judgeAttempts times before the turn is failed.
	messages := []chatMessage{{Role: "user", Content: buf.String()}}
	var scores []JudgeScore
	var lastErr error
	for attempt := 0; attempt < judgeAttempts; attempt++ {
		if attempt > 0 {
			messages = append(messages, chatMessage{
				Role:    "user",
				Content: fmt.Sprintf(judgeCorrectionPrompt, lastErr),
			})
		}
		content, err := j.callLLM(ctx, messages)
		if err != nil {
			return nil, err
		}
		parsed, err := parseJudgeScores(content)
		if err == nil {
			scores = parsed
			lastErr = nil
			break
		}
		lastErr = err
		messages = append(messages, chatMessage{Role: "assistant", Content: content})
	}
	if lastErr != nil {
		return nil, fmt.Errorf("parsing judge scores from LLM output after %d attempts: %w", judgeAttempts, lastErr)
	}

	// Clamp scores to valid ranges based on the rubric.
	for i := range scores {
		criterion := findCriterion(scores[i].Criterion, rubric)
		if criterion == nil {
			continue
		}
		switch criterion.Scoring {
		case "binary":
			if scores[i].Score < 0 {
				scores[i].Score = 0
			}
			if scores[i].Score > 1 {
				scores[i].Score = 1
			}
		case "scale_1_5":
			if scores[i].Score < 1 {
				scores[i].Score = 1
			}
			if scores[i].Score > 5 {
				scores[i].Score = 5
			}
		}
	}

	return scores, nil
}

// judgeAttempts is how many times a turn's judgement is attempted: the first
// try plus re-asks after unparseable judge output.
const judgeAttempts = 3

// judgeCorrectionPrompt is appended to the conversation after judge output that
// could not be parsed (%v is the parse error).
const judgeCorrectionPrompt = `Your previous reply could not be parsed as JSON: %v.
Reply again with ONLY the JSON array, no other text:
[{"criterion":"...","score":0.0,"reasoning":"..."}]
Escape every double quote inside a string value as \" — use single quotes or
backticks when quoting code, identifiers, or messages. No code fences.`

// callLLM sends the conversation to the inference server and returns the first
// choice's assistant content.
func (j *Judge) callLLM(ctx context.Context, messages []chatMessage) (string, error) {
	reqBody := chatRequest{
		Model:    j.Model,
		Messages: messages,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshalling request: %w", err)
	}

	url := j.URL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if j.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+j.APIKey)
	}

	client := j.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling inference server at %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("inference server returned %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("parsing chat response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("no choices in chat response")
	}
	return chatResp.Choices[0].Message.Content, nil
}

// parseJudgeScores extracts a JSON array of JudgeScore from the LLM output.
// It handles plain arrays and arrays wrapped in markdown code fences.
func parseJudgeScores(content string) ([]JudgeScore, error) {
	content = strings.TrimSpace(content)

	// Strip markdown code fences if present.
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(content, "\n")
		var inner []string
		inBlock := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") {
				inBlock = !inBlock
				continue
			}
			if inBlock {
				inner = append(inner, line)
			}
		}
		content = strings.TrimSpace(strings.Join(inner, "\n"))
	}

	// Find the first '[' and last ']' to extract just the JSON array.
	start := strings.Index(content, "[")
	end := strings.LastIndex(content, "]")
	if start == -1 || end == -1 || end < start {
		return nil, fmt.Errorf("no JSON array found in LLM output: %s", truncate(content, 200))
	}
	jsonStr := content[start : end+1]

	var scores []JudgeScore
	if err := json.Unmarshal([]byte(jsonStr), &scores); err != nil {
		// Judge models routinely emit invalid JSON despite being told not to.
		// Salvage the common mistakes — a literal unescaped '"' inside a string
		// value (e.g. quoting a Go error message or identifier in "reasoning")
		// and trailing commas — before giving up on the reply.
		salvaged := false
		for _, candidate := range []string{
			repairUnescapedQuotes(jsonStr),
			repairTrailingCommas(jsonStr),
			repairUnescapedQuotes(repairTrailingCommas(jsonStr)),
		} {
			var repaired []JudgeScore
			if err2 := json.Unmarshal([]byte(candidate), &repaired); err2 == nil {
				scores = repaired
				salvaged = true
				break
			}
		}
		if !salvaged {
			return nil, fmt.Errorf("unmarshalling judge scores JSON: %w (input: %s)", err, truncate(jsonStr, 200))
		}
	}
	return scores, nil
}

// repairUnescapedQuotes fixes a common LLM JSON mistake: a '"' inside a
// string value that was meant literally rather than as the string's
// terminator. It walks the input tracking whether it's inside a string, and
// treats a '"' as a real terminator only when it's immediately followed (after
// optional whitespace) by a JSON structural character (',', '}', ']', ':') or
// the end of input; otherwise it escapes the quote in place.
func repairUnescapedQuotes(s string) string {
	var out strings.Builder
	inString := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString && c == '\\' && i+1 < len(s) {
			out.WriteByte(c)
			out.WriteByte(s[i+1])
			i++
			continue
		}
		if c != '"' {
			out.WriteByte(c)
			continue
		}
		if !inString {
			inString = true
			out.WriteByte(c)
			continue
		}
		j := i + 1
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
			j++
		}
		if j >= len(s) || strings.ContainsRune(",}]:", rune(s[j])) {
			inString = false
			out.WriteByte(c)
			continue
		}
		out.WriteByte('\\')
		out.WriteByte(c)
	}
	return out.String()
}

// repairTrailingCommas removes a ',' that appears just before a closing '}' or
// ']' outside of strings — another common LLM JSON mistake.
func repairTrailingCommas(s string) string {
	var out strings.Builder
	inString := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString && c == '\\' && i+1 < len(s) {
			out.WriteByte(c)
			out.WriteByte(s[i+1])
			i++
			continue
		}
		if c == '"' {
			inString = !inString
			out.WriteByte(c)
			continue
		}
		if c == ',' && !inString {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue // drop the trailing comma
			}
		}
		out.WriteByte(c)
	}
	return out.String()
}

// WeightedScore computes the weighted average: sum(score * weight) / sum(weight).
// Returns 0 if the total weight is 0.
func WeightedScore(scores []JudgeScore, rubric []RubricCriterion) float64 {
	if len(scores) == 0 || len(rubric) == 0 {
		return 0
	}

	// Build a weight lookup from the rubric.
	weights := make(map[string]int, len(rubric))
	for _, r := range rubric {
		weights[strings.ToLower(r.Criterion)] = r.Weight
	}

	var weightedSum, totalWeight float64
	for _, s := range scores {
		w := weights[strings.ToLower(s.Criterion)]
		if w == 0 {
			continue // unknown criterion or zero weight
		}
		weightedSum += s.Score * float64(w)
		totalWeight += float64(w)
	}

	if totalWeight == 0 {
		return 0
	}
	return weightedSum / totalWeight
}

// findCriterion returns the rubric entry matching the given criterion name (case-insensitive), or nil.
func findCriterion(name string, rubric []RubricCriterion) *RubricCriterion {
	lower := strings.ToLower(name)
	for i := range rubric {
		if strings.ToLower(rubric[i].Criterion) == lower {
			return &rubric[i]
		}
	}
	return nil
}

// truncate returns the first n characters of s, appending "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
