package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
)

var (
	flagEscalate bool
	flagLocal    bool
	flagNew      bool
	flagSession  string
	flagContinue bool
	flagList     bool
	flagListAll  bool
	flagDrop     bool
)

const errGettingCWD = "getting cwd: %w"

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "milk [flags] [prompt]",
	Short: "Local-first agentic orchestrator",
	Long: `milk routes prompts between a local LLM (Qwen2.5 via llama.cpp) and
Claude Code CLI, maintaining session state and supporting context promotion.`,
	Args:         cobra.ArbitraryArgs,
	SilenceUsage: true,
	RunE:         run,
}

func init() {
	rootCmd.Flags().BoolVar(&flagEscalate, "escalate", false, "Force route to Claude for this turn")
	rootCmd.Flags().BoolVar(&flagLocal, "local", false, "Force route to local model for this turn")
	rootCmd.Flags().BoolVar(&flagNew, "new", false, "Start a new session")
	rootCmd.Flags().StringVar(&flagSession, "session", "", "Target session by name")
	rootCmd.Flags().BoolVarP(&flagContinue, "continue", "c", false, "Resume current session (default behavior, explicit alias)")
	rootCmd.Flags().BoolVar(&flagList, "list", false, "List sessions for current cwd")
	rootCmd.Flags().BoolVar(&flagListAll, "all", false, "With --list: show all sessions across all directories")
	rootCmd.Flags().BoolVar(&flagDrop, "drop", false, "Delete the current session")

	rootCmd.AddCommand(configCmd)
}

func run(cmd *cobra.Command, args []string) error {
	if flagList {
		return runList(flagListAll)
	}
	if flagDrop {
		return runDrop()
	}

	prompt := strings.TrimSpace(strings.Join(args, " "))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf(errGettingCWD, err)
	}

	if prompt == "" {
		return runREPL(cfg, cwd, flagNew, flagSession)
	}

	sess, err := loadSessionForRun(cwd)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}

	obsShutdown := initObs(cfg)
	defer func() { obsShutdown(context.Background()) }() //nolint:errcheck

	memDir, err := memoryDir()
	if err != nil {
		return err
	}
	mem, err := memory.NewStore(memDir, sess.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: memory store unavailable: %v\n", milkTag(), err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	localAgent := local.New(cfg.LlamaURL, cfg.LlamaModel)
	if od, err := config.OtelDir(); err == nil {
		localAgent.WithOtelDir(od)
	}
	claudeAgent := claude.NewWithOpts(cfg.ClaudeBin, cfg.DangerouslySkipPermissions, cfg.AllowedTools, cfg.AddDirs, cfg.EffectivePermissionPhrases(), cfg.EffectiveDirRestrictionPhrases())

	localAvail, claudeAvail, err := checkAgentAvailability(ctx, localAgent, claudeAgent)
	if err != nil {
		return err
	}

	// Router uses nil localAgent when llama.cpp is down (skips classifier)
	var routeLocalAgent *local.Agent
	if localAvail {
		routeLocalAgent = localAgent
	}
	rtr := router.New(cfg, routeLocalAgent)

	decision, err := rtr.Route(ctx, sess, prompt, flagEscalate, flagLocal)
	if err != nil {
		return fmt.Errorf("routing: %w", err)
	}

	target := resolveTarget(decision.Target, localAvail, claudeAvail)

	switch target {
	case router.TargetLocal:
		if mem != nil {
			defer mem.Consolidate() //nolint:errcheck
		}
		return runLocal(ctx, cfg, sess, localAgent, mem, prompt)
	case router.TargetClaude:
		return runClaude(ctx, sess, claudeAgent, prompt)
	default:
		return fmt.Errorf("unknown routing target: %s", target)
	}
}

func memoryDir() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", fmt.Errorf("memory dir: %w", err)
	}
	return dir + "/memory", nil
}

// initObs bootstraps OTel, prints any file-size warning, and returns a
// shutdown function. It never returns an error — OTel failures are non-fatal.
func initObs(cfg config.Config) (shutdown func(context.Context) error) {
	otelDir, err := config.OtelDir()
	if err != nil {
		return func(context.Context) error { return nil }
	}

	if warn, exceeded := obs.CheckFileSizes(cfg.Otel, otelDir); exceeded {
		fmt.Fprintln(os.Stderr, milkTag()+" "+warn)
		// Hard cap exceeded — skip OTel for this session.
		return func(context.Context) error { return nil }
	} else if warn != "" {
		fmt.Fprintln(os.Stderr, milkTag()+" warning: "+warn)
	}

	shutdown, err = obs.Init(cfg.Otel, otelDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: OTel init failed: %v\n", milkTag(), err)
		return func(context.Context) error { return nil }
	}
	return shutdown
}

func loadSessionForRun(cwd string) (*session.Session, error) {
	if flagNew {
		return session.New(cwd, flagSession)
	}
	return session.Resume(cwd, flagSession)
}

func checkAgentAvailability(ctx context.Context, localAgent *local.Agent, claudeAgent *claude.Agent) (bool, bool, error) {
	localAvail := localAgent.Ping(ctx) == nil
	claudeAvail := claudeAgent.Ping() == nil

	if !localAvail && !claudeAvail {
		return false, false, fmt.Errorf("neither llama.cpp nor claude CLI is available")
	}
	if !localAvail {
		fmt.Fprintln(os.Stderr, milkTag()+" warning: llama.cpp unreachable — routing all to Claude")
	}
	if !claudeAvail {
		fmt.Fprintln(os.Stderr, milkTag()+" warning: claude CLI unavailable — local only")
	}

	return localAvail, claudeAvail, nil
}

func resolveTarget(target router.Target, localAvail, claudeAvail bool) router.Target {
	if target == router.TargetLocal && !localAvail {
		return router.TargetClaude
	}
	if target == router.TargetClaude && !claudeAvail {
		return router.TargetLocal
	}
	return target
}

const claudeLabel = "claude:"
const localLabel = "local:"

func runLocal(ctx context.Context, cfg config.Config, sess *session.Session, agent *local.Agent, mem *memory.Store, prompt string, outs ...io.Writer) error {
	out := io.Writer(os.Stdout)
	if len(outs) > 0 && outs[0] != nil {
		out = outs[0]
	}
	fmt.Fprint(out, bold(green(localLabel))+" ")
	aw := newActivityWriter(out)
	history := sessionToMessages(sess)

	sess.ForceState(session.StateLocal)

	updatedHistory, err := agent.Run(ctx, history, prompt, aw, sess, mem)
	aw.Done()
	if err != nil {
		if esc, ok := err.(*local.EscalationSignal); ok {
			fmt.Fprintf(out, "\n%s local model requested escalation: %s\n", milkTag(), esc.Reason)
			// Commit the user turn before escalating so Claude has context
			sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentLocal, Content: prompt})
			sess.ForceState(session.StateRouting)
			session.Save(sess) //nolint:errcheck
			claudeAgent := claude.NewWithOpts(cfg.ClaudeBin, cfg.DangerouslySkipPermissions, cfg.AllowedTools, cfg.AddDirs, cfg.EffectivePermissionPhrases(), cfg.EffectiveDirRestrictionPhrases())
			return runClaudeWith(ctx, sess, claudeAgent, prompt, newStdinInputReader(), out)
		}
		return err
	}

	// Only commit user+assistant turns when we have a real response.
	// Saving an orphaned user turn (empty assistant) causes two consecutive user
	// messages on the next resume, which breaks Gemma 4's chat template.
	assistantContent := ""
	if len(updatedHistory) > 0 {
		last := updatedHistory[len(updatedHistory)-1]
		if last.Role == "assistant" {
			assistantContent = last.Content
		}
	}
	if assistantContent != "" {
		sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentLocal, Content: prompt})
		sess.AddTurn(session.Turn{
			Role:    session.RoleAssistant,
			Agent:   session.AgentLocal,
			Content: assistantContent,
		})
	}

	sess.ForceState(session.StateRouting)
	return session.Save(sess)
}

// inputReader abstracts user input for permission prompts.
// In single-shot mode it reads os.Stdin directly.
type inputReader interface {
	readLine(prompt string) (string, error)
}

// stdinInputReader reads from os.Stdin using a bufio.Scanner (line-buffered).
type stdinInputReader struct {
	s *bufio.Scanner
}

func newStdinInputReader() *stdinInputReader {
	return &stdinInputReader{s: bufio.NewScanner(os.Stdin)}
}

func (r *stdinInputReader) readLine(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	if r.s.Scan() {
		return strings.TrimSpace(r.s.Text()), nil
	}
	return "", io.EOF
}

func runClaude(ctx context.Context, sess *session.Session, agent *claude.Agent, prompt string) error {
	return runClaudeWith(ctx, sess, agent, prompt, newStdinInputReader())
}

func runClaudeWith(ctx context.Context, sess *session.Session, agent *claude.Agent, prompt string, input inputReader, outs ...io.Writer) error {
	out := io.Writer(os.Stdout)
	if len(outs) > 0 && outs[0] != nil {
		out = outs[0]
	}
	fmt.Fprint(out, bold(blue(claudeLabel))+" ")
	aw := newActivityWriter(out)
	resuming := sess.State == session.StateClaudeWaiting && sess.ClaudeSessionID != ""
	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentClaude, Content: prompt})
	sess.ForceState(session.StateClaude)

	agent = agent.WithPermissionHandler(makePermissionHandler(input))

	var (
		res claude.ParseResult
		err error
	)
	if resuming {
		res, err = agent.RunResume(ctx, sess.ClaudeSessionID, prompt, aw)
	} else {
		sysContext := escalation.BuildContext(sess)
		var claudeSessionID string
		claudeSessionID, res, err = agent.RunFirst(ctx, sysContext, prompt, aw)
		if err == nil {
			sess.ClaudeSessionID = claudeSessionID
		}
	}
	aw.Done()
	if err != nil {
		return err
	}

	// Structured signal: permission_denials in the result event is language-neutral.
	// Takes priority over phrase detection.
	if len(res.PermissionDenials) > 0 && sess.ClaudeSessionID != "" {
		res = handleStructuredDenials(ctx, sess, agent, res, input, out)
	} else if res.PermissionDenied && sess.ClaudeSessionID != "" {
		res = handlePhrasePermission(ctx, sess, agent, res, input, out)
	} else if res.DirRestricted && sess.ClaudeSessionID != "" {
		res = handlePhraseDir(ctx, sess, agent, input, out)
	}

	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentClaude, Content: res.Text})
	if res.EndsWithQ {
		sess.ForceState(session.StateClaudeWaiting)
	} else {
		sess.ForceState(session.StateRouting)
	}
	return session.Save(sess)
}

// handleStructuredDenials handles permission_denials from the result event —
// language-neutral, fires regardless of Claude's response language.
// For each denied tool it shows the attempted command/input, then asks:
//  1. allow the tool (adds to --allowedTools)
//  2. allow a directory (adds to --add-dir, shown for Bash or file tools)
func handleStructuredDenials(ctx context.Context, sess *session.Session, agent *claude.Agent, res claude.ParseResult, input inputReader, out io.Writer) claude.ParseResult {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s Claude was blocked from using:\n", milkTag())

	retryAgent := agent
	changed := false

	// Deduplicate by tool name — Claude may report the same tool blocked multiple times.
	seen := map[string]bool{}
	var denials []claude.PermissionDenialRecord
	for _, d := range res.PermissionDenials {
		if !seen[d.ToolName] {
			seen[d.ToolName] = true
			denials = append(denials, d)
		}
	}

	for _, d := range denials {
		fmt.Fprintf(out, "  • %s", bold(d.ToolName))
		if cmd, ok := d.ToolInput["command"].(string); ok {
			fmt.Fprintf(out, " → %s", dim(cmd))
		} else if path, ok := d.ToolInput["path"].(string); ok {
			fmt.Fprintf(out, " → %s", dim(path))
		}
		fmt.Fprintln(out)

		yn, _ := input.readLine(fmt.Sprintf("    allow tool %s? [y/n] ", bold(d.ToolName)))
		if strings.EqualFold(yn, "y") {
			retryAgent = retryAgent.WithExtraAllowedTool(d.ToolName)
			changed = true
		}

		if dir := askDir(input, out, suggestDir(d.ToolInput)); dir != "" {
			retryAgent = retryAgent.WithExtraDir(dir)
			changed = true
		}
	}

	if !changed {
		return res
	}
	fmt.Fprint(out, bold(blue(claudeLabel))+" ")
	retried, err := retryAgent.RunResume(ctx, sess.ClaudeSessionID, "Please continue with the approved permissions.", out)
	if err != nil {
		return res
	}
	return retried
}

// handlePhrasePermission handles a tool permission denial detected via phrase scanning.
// Asks the user y/n and retries via --resume with the tool added to allowed list.
func handlePhrasePermission(ctx context.Context, sess *session.Session, agent *claude.Agent, res claude.ParseResult, input inputReader, out io.Writer) claude.ParseResult {
	tool := res.DeniedTool
	var prompt string
	if tool != "" {
		prompt = fmt.Sprintf("%s Claude needs permission to use %s. Allow? [y/n] ", milkTag(), bold(tool))
	} else {
		prompt = fmt.Sprintf("%s Claude needs a tool permission. Allow? [y/n] ", milkTag())
	}
	yn, _ := input.readLine(prompt)
	if !strings.EqualFold(yn, "y") {
		return res
	}
	var retryAgent *claude.Agent
	if tool != "" {
		retryAgent = agent.WithExtraAllowedTool(tool)
	} else {
		retryAgent = agent
	}
	fmt.Fprint(out, bold(blue(claudeLabel))+" ")
	retried, err := retryAgent.RunResume(ctx, sess.ClaudeSessionID, "Please continue with the approved permission.", out)
	if err != nil {
		return res
	}
	return retried
}

// suggestDir extracts a suggested directory from a tool input map.
// Checks common path keys ("path", "file_path"), then scans "command" for the
// first absolute path token.
func suggestDir(input map[string]any) string {
	for _, key := range []string{"path", "file_path", "filepath"} {
		if path, ok := input[key].(string); ok && path != "" {
			return filepath.Dir(path)
		}
	}
	if cmd, ok := input["command"].(string); ok {
		for _, token := range strings.Fields(cmd) {
			if strings.HasPrefix(token, "/") {
				return filepath.Dir(token)
			}
		}
	}
	return ""
}

// askDir proposes a suggested directory (if any) and asks y/n; if declined or
// none suggested, prompts for a free-form path. Returns "" to skip.
func askDir(input inputReader, out io.Writer, suggested string) string {
	if suggested != "" {
		yn, _ := input.readLine(fmt.Sprintf("    allow directory %s? [y/n] ", bold(suggested)))
		if strings.EqualFold(yn, "y") {
			return suggested
		}
	}
	dir, _ := input.readLine("    enter directory path to allow (empty to skip): ")
	return strings.TrimSpace(dir)
}

func handlePhraseDir(ctx context.Context, sess *session.Session, agent *claude.Agent, input inputReader, out io.Writer) claude.ParseResult {
	dir := askDir(input, out, "")
	if dir == "" {
		return claude.ParseResult{}
	}
	retryAgent := agent.WithExtraDir(dir)
	fmt.Fprint(out, bold(blue(claudeLabel))+" ")
	retried, err := retryAgent.RunResume(ctx, sess.ClaudeSessionID, fmt.Sprintf("Access to %q has been granted. Please continue.", dir), out)
	if err != nil {
		return claude.ParseResult{}
	}
	return retried
}

// makePermissionHandler returns a PermissionHandler that asks the user
// interactively via stdin for each control_request Claude emits.
func makePermissionHandler(input inputReader) claude.PermissionHandler {
	return func(req claude.ControlRequest, stdinW io.Writer) {
		fmt.Fprintln(os.Stdout)
		printPermissionRequest(req)
		yn, _ := input.readLine(fmt.Sprintf("%s Allow tool? [y/n] ", milkTag()))
		if strings.EqualFold(yn, "y") {
			claude.Allow(req.RequestID, stdinW)
		} else {
			claude.Deny(req.RequestID, stdinW)
		}
		// Offer directory access if a blocked path is known
		if req.Body.BlockedPath != "" {
			suggested := filepath.Dir(req.Body.BlockedPath)
			askDir(input, os.Stdout, suggested) // result unused here; handled by retry in runClaudeWith
		}
	}
}

// printPermissionRequest shows the user what Claude is asking permission for.
func printPermissionRequest(req claude.ControlRequest) {
	b := req.Body
	fmt.Fprintf(os.Stdout, "%s permission request — tool: %s", milkTag(), bold(b.ToolName))
	if b.BlockedPath != "" {
		fmt.Fprintf(os.Stdout, "  path: %s", dim(b.BlockedPath))
	}
	if b.DecisionReasonType != "" {
		fmt.Fprintf(os.Stdout, "  reason: %s", b.DecisionReasonType)
	}
	fmt.Fprintln(os.Stdout)
	if b.Description != "" {
		fmt.Fprintf(os.Stdout, "  %s\n", b.Description)
	}
}

// sessionToMessages converts local-agent session turns to the local agent's Message format.
// Claude turns are excluded: the local model should only see its own prior conversation.
func sessionToMessages(sess *session.Session) []local.Message {
	msgs := []local.Message{}
	for _, t := range sess.History {
		if t.Agent != session.AgentLocal {
			continue
		}
		switch t.Role {
		case session.RoleUser:
			msgs = append(msgs, local.Message{Role: "user", Content: t.Content})
		case session.RoleAssistant:
			if t.Content == "" {
				continue
			}
			msgs = append(msgs, local.Message{Role: "assistant", Content: t.Content})
		case session.RoleToolResult:
			msgs = append(msgs, local.Message{Role: "tool", Content: t.Content})
		}
	}
	return msgs
}

func runList(all bool) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf(errGettingCWD, err)
	}
	target := cwd
	if all {
		target = ""
	}
	entries, err := session.List(target)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no sessions found")
		return nil
	}
	for dir, list := range entries {
		fmt.Printf("%s\n", dir)
		for _, e := range list {
			name := e.Name
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Printf("  %s  %-20s  %s\n", e.ID[:8], name, e.LastUsed.Format("2006-01-02 15:04"))
		}
	}
	return nil
}

func runDrop() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf(errGettingCWD, err)
	}
	sess, err := session.Resume(cwd, flagSession)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	if err := session.Drop(sess.ID, cwd); err != nil {
		return err
	}
	fmt.Printf("dropped session %s\n", sess.ID[:8])
	return nil
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Print effective configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		fmt.Printf("llama_url:      %s\n", cfg.LlamaURL)
		fmt.Printf("llama_model:    %s\n", cfg.LlamaModel)
		fmt.Printf("claude_bin:     %s\n", cfg.ClaudeBin)
		fmt.Printf("default_route:  %s\n", cfg.DefaultRoute)
		fmt.Printf("escalate_above_tokens: %d\n", cfg.Rules.EscalateAboveTokens)
		fmt.Printf("local_below_tokens:    %d\n", cfg.Rules.LocalBelowTokens)
		fmt.Printf("escalate_keywords:     %v\n", cfg.Rules.EscalateKeywords)
		fmt.Printf("local_verbs:           %v\n", cfg.Rules.LocalVerbs)
		fmt.Printf("escalate_verbs:        %v\n", cfg.Rules.EscalateVerbs)
		fmt.Printf("escalate_threshold:    %d\n", cfg.Rules.EscalateThreshold)
		fmt.Printf("local_threshold:       %d\n", cfg.Rules.LocalThreshold)
		fmt.Printf("local_verb_weight:     %d\n", cfg.Rules.LocalVerbWeight)
		fmt.Printf("escalate_verb_weight:  %d\n", cfg.Rules.EscalateVerbWeight)
		fmt.Printf("path_ref_weight:       %d\n", cfg.Rules.PathRefWeight)
		fmt.Printf("code_block_weight:     %d\n", cfg.Rules.CodeBlockWeight)
		fmt.Printf("open_question_weight:  %d\n", cfg.Rules.OpenQuestionWeight)
		fmt.Printf("classifier_fallback:   %s\n", cfg.Rules.ClassifierFallback)
		return nil
	},
}
