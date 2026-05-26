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
	"github.com/scoutme/milk/internal/claudesettings"
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
	Short: "Switch models, not context.",
	Long: `milk lets you move between a local LLM and Claude Code mid-workflow, without
losing context. The local agent speaks the OpenAI-compatible API — any compliant
inference server works, local or remote (llama.cpp, Ollama, LM Studio, vLLM, or
any hosted endpoint).`,
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

	ac := applyFreshAWSCreds(cfg, cfg.ActiveLocalAgent())
	localAgent := local.NewFromConfig(ac)
	if od, err := config.OtelDir(); err == nil {
		localAgent.WithOtelDir(od)
	}
	claudeAgent := claude.NewWithOpts(cfg.ClaudeBin, cfg.DangerouslySkipPermissions, cfg.AllowedTools, cfg.AddDirs, cfg.EffectivePermissionPhrases(), cfg.EffectiveDirRestrictionPhrases())
	claudeAgent = applyAWSCreds(cfg, claudeAgent)
	if dbg, err := openClaudeDebugLog(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: cannot open claude debug log: %v\n", milkTag(), err)
	} else if dbg != nil {
		defer dbg.Close()
		claudeAgent = claudeAgent.WithDebugLog(dbg)
	}

	var cs *claudesettings.Store
	if store, err := claudesettings.Open(cwd); err == nil {
		cs = store
	}

	localAvail, claudeAvail, err := checkAgentAvailabilityStrict(ctx, localAgent, claudeAgent)
	if err != nil {
		return err
	}

	// Router uses nil localAgent when the inference server is unreachable (skips classifier)
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
		return runClaude(ctx, sess, claudeAgent, prompt, cs, mem)
	default:
		return fmt.Errorf("unknown routing target: %s", target)
	}
}

// applyFreshAWSCreds refreshes AWS credentials in ac when aws_auth_refresh is
// enabled and the provider is "bedrock" without explicit credentials already set.
func applyFreshAWSCreds(cfg config.Config, ac config.LocalAgentConfig) config.LocalAgentConfig {
	if !cfg.AWSAuthRefresh {
		return ac
	}
	if strings.ToLower(strings.TrimSpace(ac.Provider)) != "bedrock" {
		return ac
	}
	if ac.AWSKeyID != "" {
		return ac // explicit config takes precedence; don't override
	}
	cmd := claudesettings.AWSAuthRefreshCommand()
	if cmd == "" {
		return ac
	}
	creds, err := claude.ResolveAWSCreds(cmd)
	if err != nil || creds == nil {
		fmt.Fprintf(os.Stderr, "%s warning: aws_auth_refresh for local bedrock agent failed: %v\n", milkTag(), err)
		return ac
	}
	ac.AWSKeyID = creds.AccessKeyID
	ac.AWSSecret = creds.SecretAccessKey
	ac.AWSToken = creds.SessionToken
	return ac
}

// applyAWSCreds injects resolved AWS credentials into the agent when
// cfg.AWSAuthRefresh is enabled. The command is read from ~/.claude/settings.json.
func applyAWSCreds(cfg config.Config, agent *claude.Agent) *claude.Agent {
	if !cfg.AWSAuthRefresh {
		return agent
	}
	cmd := claudesettings.AWSAuthRefreshCommand()
	if cmd == "" {
		fmt.Fprintf(os.Stderr, "%s warning: aws_auth_refresh enabled but awsAuthRefresh not found in ~/.claude/settings.json\n", milkTag())
		return agent
	}
	creds, err := claude.ResolveAWSCreds(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: aws_auth_refresh failed: %v\n", milkTag(), err)
		return agent
	}
	if creds != nil {
		agent = agent.WithExtraEnv(creds.Env()...)
	}
	return agent
}

// openClaudeDebugLog opens (or creates/appends) the Claude raw NDJSON debug log
// when cfg.DebugClaudeCode is true. Returns nil, nil when disabled.
// The caller is responsible for closing the returned file.
func openClaudeDebugLog(cfg config.Config) (*os.File, error) {
	if !cfg.DebugClaudeCode {
		return nil, nil
	}
	path, err := config.ClaudeDebugLogPath()
	if err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
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

	if !localAvail {
		fmt.Fprintln(os.Stderr, milkTag()+" warning: local inference server unreachable — routing all to Claude")
	}
	if !claudeAvail {
		fmt.Fprintln(os.Stderr, milkTag()+" warning: claude CLI unavailable — local only")
	}

	return localAvail, claudeAvail, nil
}

// checkAgentAvailabilityStrict is like checkAgentAvailability but returns an
// error when both agents are unavailable. Used by single-prompt mode where
// starting without any agent makes no sense.
func checkAgentAvailabilityStrict(ctx context.Context, localAgent *local.Agent, claudeAgent *claude.Agent) (bool, bool, error) {
	localAvail, claudeAvail, err := checkAgentAvailability(ctx, localAgent, claudeAgent)
	if err != nil {
		return false, false, err
	}
	if !localAvail && !claudeAvail {
		return false, false, fmt.Errorf("neither local inference server nor claude CLI is available")
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

func claudeLabelStyled(a *claude.Agent) string {
	if a.SkipPermissions() {
		return bold(red(claudeLabel))
	}
	return bold(blue(claudeLabel))
}

func localLabel(cfg config.Config) string {
	ac := cfg.ActiveLocalAgent()
	name := strings.ToLower(strings.TrimSpace(ac.Name))
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(ac.Provider))
	}
	if name == "" {
		name = "local"
	}
	return name + ":"
}

func runLocal(ctx context.Context, cfg config.Config, sess *session.Session, agent *local.Agent, mem *memory.Store, prompt string, outs ...io.Writer) error {
	out := io.Writer(os.Stdout)
	if len(outs) > 0 && outs[0] != nil {
		out = outs[0]
	}
	fmt.Fprint(out, bold(green(localLabel(cfg)))+" ")
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
			escalateAgent := claude.NewWithOpts(cfg.ClaudeBin, cfg.DangerouslySkipPermissions, cfg.AllowedTools, cfg.AddDirs, cfg.EffectivePermissionPhrases(), cfg.EffectiveDirRestrictionPhrases())
			escalateAgent = applyAWSCreds(cfg, escalateAgent)
			var localCs *claudesettings.Store
			if cwd, err := os.Getwd(); err == nil {
				localCs, _ = claudesettings.Open(cwd)
			}
			return runClaudeWith(ctx, sess, escalateAgent, prompt, newStdinInputReader(), permContext{cs: localCs}, mem, out)
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

func runClaude(ctx context.Context, sess *session.Session, agent *claude.Agent, prompt string, cs *claudesettings.Store, mem *memory.Store) error {
	return runClaudeWith(ctx, sess, agent, prompt, newStdinInputReader(), permContext{cs: cs}, mem)
}

func runClaudeWith(ctx context.Context, sess *session.Session, agent *claude.Agent, prompt string, input inputReader, pc permContext, mem *memory.Store, outs ...io.Writer) error {
	out := io.Writer(os.Stdout)
	if len(outs) > 0 && outs[0] != nil {
		out = outs[0]
	}
	fmt.Fprint(out, claudeLabelStyled(agent)+" ")
	aw := newActivityWriter(out)
	resuming := sess.State == session.StateClaudeWaiting && sess.ClaudeSessionID != ""
	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentClaude, Content: prompt})
	sess.ForceState(session.StateClaude)

	// Generate a fresh nonce for this Claude turn. The same nonce is embedded in
	// the system-prompt instruction (via MemoryInstruction/BuildContext) and in the
	// stream parser (via WithOnPercept), so only tags containing this nonce are
	// captured — explanatory text about the tag format is ignored.
	nonce := claude.GenerateNonce()

	agent = applyPersistedGrants(agent, pc)
	// In TUI mode the handler is pre-attached by dispatchAgent (toolFutures != nil).
	// In single-shot mode install an interactive handler here.
	if pc.cs != nil && pc.toolFutures == nil {
		agent = agent.WithPermissionHandler(makePermissionHandler(input, out, pc.cs))
	}
	if mem != nil {
		agent = agent.WithOnPercept(func(content, consumerHint string) {
			var consumer memory.Consumer
			switch consumerHint {
			case "local":
				consumer = memory.ConsumerLocal
			case "claude":
				consumer = memory.ConsumerClaude
			}
			_, err := mem.Record(ctx, content, memory.ProducerClaude, consumer, memory.Roles{}, false)
			// DuplicateError is expected when Claude emits a percept similar to one
			// already stored — silently drop it; any other error is also non-fatal.
			_ = err
		}, nonce)
	}

	res, err := runClaudeAgent(ctx, sess, agent, prompt, aw, resuming, nonce, perceptsForClaude(mem))
	aw.Done()
	if err != nil {
		return err
	}

	if sess.ClaudeSessionID != "" {
		res = handlePermissionDenials(ctx, sess, agent, res, input, out, pc, nonce)
	}

	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentClaude, Content: res.Text})
	if res.EndsWithQ {
		sess.ForceState(session.StateClaudeWaiting)
	} else {
		sess.ForceState(session.StateRouting)
	}
	return session.Save(sess)
}

// applyPersistedGrants loads previously-approved tools and directories from
// settings.json and wires them into the agent so grants survive across turns.
// In single-shot mode it also installs the interactive permission handler.
func applyPersistedGrants(agent *claude.Agent, pc permContext) *claude.Agent {
	if pc.cs == nil {
		return agent
	}
	if tools, err := pc.cs.AllowedTools(); err == nil {
		for _, t := range tools {
			agent = agent.WithExtraAllowedTool(t)
		}
	}
	if dirs, err := pc.cs.AllowedDirectories(); err == nil {
		for _, d := range dirs {
			agent = agent.WithExtraDir(d)
		}
	}
	return agent
}

// runClaudeAgent runs one Claude turn (first or resume) and returns the result.
// nonce is the session-specific percept nonce embedded in the system-prompt instruction.
// percepts are injected as a [Remembered facts] block in the system prompt.
func runClaudeAgent(ctx context.Context, sess *session.Session, agent *claude.Agent, prompt string, out io.Writer, resuming bool, nonce string, percepts []string) (claude.ParseResult, error) {
	if resuming {
		return agent.RunResume(ctx, sess.ClaudeSessionID, escalation.MemoryInstruction(nonce), prompt, out)
	}
	sysContext := escalation.BuildContext(sess, nonce, percepts)
	claudeSessionID, res, err := agent.RunFirst(ctx, sysContext, prompt, out)
	if err == nil {
		sess.ClaudeSessionID = claudeSessionID
	}
	return res, err
}

// handlePermissionDenials checks the result for permission issues and retries if the user approves.
func handlePermissionDenials(ctx context.Context, sess *session.Session, agent *claude.Agent, res claude.ParseResult, input inputReader, out io.Writer, pc permContext, nonce string) claude.ParseResult {
	switch {
	case len(res.PermissionDenials) > 0:
		return handleStructuredDenials(ctx, sess, agent, res, input, out, pc, nonce)
	case res.PermissionDenied:
		return handlePhrasePermission(ctx, sess, agent, res, input, out, nonce)
	case res.DirRestricted:
		return handlePhraseDir(ctx, sess, agent, input, out, nonce)
	}
	return res
}

// permContext bundles the mutable permission state threaded through a Claude turn.
type permContext struct {
	cs          *claudesettings.Store
	toolFutures map[string]chan string // tool name → buffered channel pre-filled by OnToolUse
}

// handleStructuredDenials handles permission_denials from the result event —
// language-neutral, fires regardless of Claude's response language.
func handleStructuredDenials(ctx context.Context, sess *session.Session, agent *claude.Agent, res claude.ParseResult, input inputReader, out io.Writer, pc permContext, nonce string) claude.ParseResult {
	fmt.Fprintf(out, "\n%s Claude was blocked from using:\n", milkTag())
	retryAgent, changed := applyDenials(agent, dedupDenials(res.PermissionDenials), input, out, pc)
	if !changed {
		return res
	}
	fmt.Fprint(out, claudeLabelStyled(retryAgent)+" ")
	retried, err := retryAgent.RunResume(ctx, sess.ClaudeSessionID, escalation.MemoryInstruction(nonce), "Please continue with the approved permissions.", out)
	if err != nil {
		return res
	}
	return retried
}

func dedupDenials(src []claude.PermissionDenialRecord) []claude.PermissionDenialRecord {
	seen := map[string]bool{}
	var out []claude.PermissionDenialRecord
	for _, d := range src {
		if !seen[d.ToolName] {
			seen[d.ToolName] = true
			out = append(out, d)
		}
	}
	return out
}

func applyDenials(agent *claude.Agent, denials []claude.PermissionDenialRecord, input inputReader, out io.Writer, pc permContext) (*claude.Agent, bool) {
	changed := false
	for _, d := range denials {
		printDenialHeader(d, out)
		if applyToolGrant(d, input, out, pc, &agent) {
			changed = true
		}
		if applyDirGrant(d, input, pc, &agent) {
			changed = true
		}
	}
	return agent, changed
}

func printDenialHeader(d claude.PermissionDenialRecord, out io.Writer) {
	fmt.Fprintf(out, "  • %s", bold(d.ToolName))
	if cmd, ok := d.ToolInput["command"].(string); ok {
		fmt.Fprintf(out, " → %s", dim(cmd))
	} else if path, ok := d.ToolInput["file_path"].(string); ok {
		fmt.Fprintf(out, " → %s", dim(path))
	}
	fmt.Fprintln(out)
}

func applyToolGrant(d claude.PermissionDenialRecord, input inputReader, out io.Writer, pc permContext, agent **claude.Agent) bool {
	yn := drainFuture(pc.toolFutures, d.ToolName)
	if yn == "" {
		yn, _ = input.readLine(fmt.Sprintf("    allow tool %s? [Y/n] ", bold(d.ToolName)))
		if yn == "" {
			yn = "y"
		}
	} else {
		fmt.Fprintf(out, "    allow tool %s? [Y/n] %s\n", bold(d.ToolName), yn)
	}
	if !strings.EqualFold(yn, "y") {
		return false
	}
	*agent = (*agent).WithExtraAllowedTool(d.ToolName)
	if pc.cs != nil {
		pc.cs.AllowTool(d.ToolName) //nolint:errcheck
	}
	return true
}

func applyDirGrant(d claude.PermissionDenialRecord, input inputReader, pc permContext, agent **claude.Agent) bool {
	dir := askDir(input, suggestDir(d.ToolInput))
	if dir == "" {
		return false
	}
	*agent = (*agent).WithExtraDir(dir)
	if pc.cs != nil {
		pc.cs.AllowDirectory(dir) //nolint:errcheck
	}
	return true
}

// handlePhrasePermission handles a tool permission denial detected via phrase scanning.
// Asks the user y/n and retries via --resume with the tool added to allowed list.
func handlePhrasePermission(ctx context.Context, sess *session.Session, agent *claude.Agent, res claude.ParseResult, input inputReader, out io.Writer, nonce string) claude.ParseResult {
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
	fmt.Fprint(out, claudeLabelStyled(retryAgent)+" ")
	retried, err := retryAgent.RunResume(ctx, sess.ClaudeSessionID, escalation.MemoryInstruction(nonce), "Please continue with the approved permission.", out)
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
		for token := range strings.FieldsSeq(cmd) {
			if strings.HasPrefix(token, "/") {
				return filepath.Dir(token)
			}
		}
	}
	return ""
}

// askDir proposes a directory and asks Y/n (enter = yes).
// Falls back to cwd when suggested is empty. Returns "" only if the user types "n".
func askDir(input inputReader, suggested string) string {
	if suggested == "" {
		suggested, _ = os.Getwd()
	}
	if suggested == "" {
		return ""
	}
	yn, _ := input.readLine(fmt.Sprintf("    allow directory %s? [Y/n] ", bold(suggested)))
	if strings.EqualFold(yn, "n") {
		return ""
	}
	return suggested
}

// drainFuture reads from the pre-seeded channel for toolName (if any).
// Returns "" when no future exists. Blocks only if the channel was created but
// the user hasn't answered yet (should resolve in milliseconds after stream ends).
func drainFuture(futures map[string]chan string, toolName string) string {
	if futures == nil {
		return ""
	}
	ch, ok := futures[toolName]
	if !ok {
		return ""
	}
	yn := <-ch
	if yn == "" {
		yn = "y"
	}
	return yn
}

func handlePhraseDir(ctx context.Context, sess *session.Session, agent *claude.Agent, input inputReader, out io.Writer, nonce string) claude.ParseResult {
	dir := askDir(input, "")
	if dir == "" {
		return claude.ParseResult{}
	}
	retryAgent := agent.WithExtraDir(dir)
	fmt.Fprint(out, claudeLabelStyled(retryAgent)+" ")
	retried, err := retryAgent.RunResume(ctx, sess.ClaudeSessionID, escalation.MemoryInstruction(nonce), fmt.Sprintf("Access to %q has been granted. Please continue.", dir), out)
	if err != nil {
		return claude.ParseResult{}
	}
	return retried
}

// makePermissionHandler returns a PermissionHandler for single-shot (non-TUI)
// mode. It asks y/n interactively and, on approval, persists the grant to the
// Claude project settings so the tool is auto-allowed on future runs.
// cs may be nil — persistence is best-effort.
func makePermissionHandler(input inputReader, out io.Writer, cs *claudesettings.Store) claude.PermissionHandler {
	return func(req claude.ControlRequest, stdinW io.Writer) {
		fmt.Fprintln(out)
		printPermissionRequest(req, out)
		yn, _ := input.readLine(fmt.Sprintf("%s Allow tool? [y/n] ", milkTag()))
		if strings.EqualFold(yn, "y") {
			claude.Allow(req.RequestID, stdinW)
			if cs != nil && req.Body.ToolName != "" {
				cs.AllowTool(req.Body.ToolName) //nolint:errcheck
			}
			if req.Body.BlockedPath != "" {
				dir := filepath.Dir(req.Body.BlockedPath)
				yn2, _ := input.readLine(fmt.Sprintf("    allow directory %s? [y/n] ", bold(dir)))
				if strings.EqualFold(yn2, "y") && cs != nil {
					cs.AllowDirectory(dir) //nolint:errcheck
				}
			}
		} else {
			claude.Deny(req.RequestID, stdinW)
		}
	}
}

// claudeToolArgSummary picks the most informative single argument value for display,
// mirroring the local agent's toolArgSummary.
func claudeToolArgSummary(args map[string]any) string {
	for _, key := range []string{"command", "path", "file_path", "url", "query", "pattern", "reason", "content"} {
		if v, ok := args[key].(string); ok && v != "" {
			if len(v) > 60 {
				return v[:57] + "..."
			}
			return v
		}
	}
	return ""
}

// makeTUIPermissionHandler returns a PermissionHandler for TUI mode.
// It blocks the stream goroutine by sending a permRequestMsg to the TUI and
// waiting for the user's y/n reply before forwarding allow/deny to Claude.
// cs may be nil — persistence is best-effort.
func makeTUIPermissionHandler(input inputReader, cs *claudesettings.Store) claude.PermissionHandler {
	return func(req claude.ControlRequest, stdinW io.Writer) {
		b := req.Body
		prompt := fmt.Sprintf("\n%s permission request — tool: %s", milkTag(), bold(b.ToolName))
		if b.BlockedPath != "" {
			prompt += fmt.Sprintf("  path: %s", dim(b.BlockedPath))
		}
		if b.DecisionReasonType != "" {
			prompt += fmt.Sprintf("  reason: %s", b.DecisionReasonType)
		}
		prompt += "\n"
		if b.Description != "" {
			prompt += fmt.Sprintf("  %s\n", b.Description)
		}
		prompt += fmt.Sprintf("%s Allow? [Y/n] ", milkTag())

		yn, _ := input.readLine(prompt)
		if yn == "" {
			yn = "y"
		}
		if strings.EqualFold(yn, "y") {
			claude.Allow(req.RequestID, stdinW)
			if cs != nil {
				if b.ToolName != "" {
					cs.AllowTool(b.ToolName) //nolint:errcheck
				}
				if b.BlockedPath != "" {
					cs.AllowDirectory(filepath.Dir(b.BlockedPath)) //nolint:errcheck
				}
			}
		} else {
			claude.Deny(req.RequestID, stdinW)
		}
	}
}

// printPermissionRequest shows the user what Claude is asking permission for.
func printPermissionRequest(req claude.ControlRequest, out io.Writer) {
	b := req.Body
	fmt.Fprintf(out, "%s permission request — tool: %s", milkTag(), bold(b.ToolName))
	if b.BlockedPath != "" {
		fmt.Fprintf(out, "  path: %s", dim(b.BlockedPath))
	}
	if b.DecisionReasonType != "" {
		fmt.Fprintf(out, "  reason: %s", b.DecisionReasonType)
	}
	fmt.Fprintln(out)
	if b.Description != "" {
		fmt.Fprintf(out, "  %s\n", b.Description)
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

// perceptsForClaude returns the content strings of all percepts that Claude
// should receive at session start: those not exclusively targeted at the local
// agent and not already produced by Claude (to avoid echo loops).
func perceptsForClaude(mem *memory.Store) []string {
	if mem == nil {
		return nil
	}
	all := mem.List(memory.ListOpts{})
	var out []string
	for _, p := range all {
		if p.Producer == memory.ProducerClaude {
			continue // Claude wrote it; no need to echo it back
		}
		if p.Consumer == memory.ConsumerLocal {
			continue // explicitly local-only
		}
		out = append(out, p.Content)
	}
	return out
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
