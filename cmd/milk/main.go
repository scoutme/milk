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

// Set via -ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
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
	Version:      fmt.Sprintf("%s (commit %s, built %s)", version, commit, date),
	RunE:         run,
}

func init() {
	rootCmd.Flags().BoolVar(&flagEscalate, "escalate", false, "Force route to escalation agent for this turn")
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

	ac := applyFreshAWSCreds(cfg, activeLocalAgentConfig(cfg))
	localAgent := local.NewFromConfig(ac)
	if od, err := config.OtelDir(); err == nil {
		localAgent.WithOtelDir(od)
	}
	// Single-prompt mode: wire permissions (no interactive ask — tools are denied
	// unless dangerously_skip_permissions is on or already granted in the store).
	if lp, err := local.OpenPermStore(cwd); err == nil {
		localAgent.WithPermissions(lp, nil)
	}
	localAgent.WithSkipPermissions(cliAgentConfig(cfg).DangerouslySkipPermissions)

	var escalationLocalAgent *local.Agent
	if !cfg.EscalationAgentConfig().IsCLI() {
		escAC := applyFreshAWSCreds(cfg, cfg.EscalationAgentConfig())
		if escAC.URL != "" {
			escalationLocalAgent = local.NewFromConfig(escAC).AsEscalationTarget(escAC.Name)
			if od, err := config.OtelDir(); err == nil {
				escalationLocalAgent.WithOtelDir(od)
			}
			escalationLocalAgent.WithSkipPermissions(cliAgentConfig(cfg).DangerouslySkipPermissions)
			if lp, err := local.OpenPermStore(cwd); err == nil {
				escalationLocalAgent.WithPermissions(lp, nil)
			}
		} else {
			fmt.Fprintf(os.Stderr, "%s warning: escalation_agent %q not found in agents — falling back to claude-cli\n", milkTag(), cfg.EscalationAgent)
		}
	}

	cliAgent := newCLIAgent(cliAgentConfig(cfg))
	cliAgent = applyAWSCreds(cfg, cliAgent)
	if dbg, err := openCLIDebugLog(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: cannot open claude debug log: %v\n", milkTag(), err)
	} else if dbg != nil {
		defer dbg.Close()
		cliAgent = cliAgent.WithDebugLog(dbg)
	}

	var cs *claudesettings.Store
	if store, err := claudesettings.Open(cwd); err == nil {
		cs = store
	}

	// When escalation is a second local provider, ping it instead of the Claude CLI.
	var localAvail, escalationAvail bool
	if escalationLocalAgent != nil {
		localAvail = localAgent.Ping(ctx) == nil
		escalationAvail = escalationLocalAgent.Ping(ctx) == nil
		if !localAvail && !escalationAvail {
			return fmt.Errorf("neither local inference server nor escalation agent is available")
		}
	} else {
		localAvail, escalationAvail, err = checkAgentAvailabilityStrict(ctx, localAgent, cliAgent)
		if err != nil {
			return err
		}
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

	target := resolveTarget(decision.Target, localAvail, escalationAvail)

	switch target {
	case router.TargetLocal:
		if mem != nil {
			defer mem.Consolidate() //nolint:errcheck
		}
		return runLocal(ctx, cfg, sess, localAgent, mem, prompt)
	case router.TargetEscalation:
		if escalationLocalAgent != nil {
			return runEscalationLocal(ctx, cfg, sess, escalationLocalAgent, mem, prompt)
		}
		return runCLIEscalation(ctx, cfg, sess, cliAgent, prompt, cs, mem)
	default:
		return fmt.Errorf("unknown routing target: %s", target)
	}
}

// cliAgentConfig returns the AgentConfig for the claude-cli backend —
// the first entry with Provider "claude-cli", or a built-in default.
func cliAgentConfig(cfg config.Config) config.AgentConfig {
	for _, a := range cfg.Agents {
		if a.IsCLI() {
			return a
		}
	}
	return config.AgentConfig{Name: "claude", Provider: "claude-cli", Bin: "claude"}
}

// newCLIAgent constructs a claude.Agent from the claude-cli AgentConfig.
func newCLIAgent(ac config.AgentConfig) *claude.Agent {
	bin := ac.Bin
	if bin == "" {
		bin = "claude"
	}
	return claude.NewWithOpts(bin, ac.DangerouslySkipPermissions, ac.AllowedTools, ac.AddDirs)
}

// activeLocalAgentConfig returns the active AgentConfig with AWSRefreshCmd
// populated from ~/.claude/settings.json when aws_auth_refresh is enabled.
// All NewFromConfig call sites should use this instead of cfg.ActiveAgent()
// directly so the transport gets the refresh command wired in.
func activeLocalAgentConfig(cfg config.Config) config.AgentConfig {
	ac := cfg.ActiveAgent()
	if cfg.AWSAuthRefresh && strings.ToLower(strings.TrimSpace(ac.Provider)) == "bedrock" {
		ac.AWSRefreshCmd = claudesettings.AWSAuthRefreshCommand()
	}
	return ac
}

// needsAWSRefresh reports whether an async background credential refresh is
// required for the active local agent.
func needsAWSRefresh(cfg config.Config) bool {
	if !cfg.AWSAuthRefresh {
		return false
	}
	ac := cfg.ActiveAgent()
	if strings.ToLower(strings.TrimSpace(ac.Provider)) != "bedrock" {
		return false
	}
	return ac.AWSKeyID == "" // explicit config takes precedence
}

// needsTokenCmdRefresh reports whether the active local agent uses token_cmd
// and should show a status bar hint while the first token is fetched.
func needsTokenCmdRefresh(cfg config.Config) bool {
	ac := cfg.ActiveAgent()
	return ac.TokenCmd != "" && strings.ToLower(strings.TrimSpace(ac.Provider)) != "bedrock"
}

// applyFreshAWSCreds refreshes AWS credentials in ac when aws_auth_refresh is
// enabled and the provider is "bedrock" without explicit credentials already set.
func applyFreshAWSCreds(cfg config.Config, ac config.AgentConfig) config.AgentConfig {
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

// openCLIDebugLog opens (or creates/appends) the Claude raw NDJSON debug log
// when cfg.DebugCLILog is true. Returns nil, nil when disabled.
// The caller is responsible for closing the returned file.
func openCLIDebugLog(cfg config.Config) (*os.File, error) {
	if !cfg.DebugCLILog {
		return nil, nil
	}
	path, err := config.CLIDebugLogPath()
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

func checkAgentAvailability(ctx context.Context, localAgent *local.Agent, cliAgent *claude.Agent) (bool, bool, error) {
	localAvail := localAgent.Ping(ctx) == nil
	escalationAvail := cliAgent.Ping() == nil

	if !localAvail {
		fmt.Fprintln(os.Stderr, milkTag()+" warning: primary agent unreachable — routing all to escalation agent")
	}
	if !escalationAvail {
		fmt.Fprintln(os.Stderr, milkTag()+" warning: escalation agent unavailable — primary only")
	}

	return localAvail, escalationAvail, nil
}

// checkAgentAvailabilityStrict is like checkAgentAvailability but returns an
// error when both agents are unavailable. Used by single-prompt mode where
// starting without any agent makes no sense.
func checkAgentAvailabilityStrict(ctx context.Context, localAgent *local.Agent, cliAgent *claude.Agent) (bool, bool, error) {
	localAvail, escalationAvail, err := checkAgentAvailability(ctx, localAgent, cliAgent)
	if err != nil {
		return false, false, err
	}
	if !localAvail && !escalationAvail {
		return false, false, fmt.Errorf("neither local inference server nor claude CLI is available")
	}
	return localAvail, escalationAvail, nil
}

func resolveTarget(target router.Target, localAvail, escalationAvail bool) router.Target {
	if target == router.TargetLocal && !localAvail {
		return router.TargetEscalation
	}
	if target == router.TargetEscalation && !escalationAvail {
		return router.TargetLocal
	}
	return target
}

const cliLabel = "claude:"

func cliLabelStyled(a *claude.Agent) string {
	if a.SkipPermissions() {
		return bold(red(cliLabel))
	}
	return bold(blue(cliLabel))
}

func localLabel(cfg config.Config) string {
	ac := cfg.ActiveAgent()
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
			fmt.Fprintf(out, "\n%s primary model requested escalation: %s\n", milkTag(), esc.Reason)
			// Dispatch to the configured escalation target (a second local provider, or Claude CLI).
			if !cfg.EscalationAgentConfig().IsCLI() {
				escAC := applyFreshAWSCreds(cfg, cfg.EscalationAgentConfig())
				if escAC.URL != "" {
					escAgent := local.NewFromConfig(escAC).AsEscalationTarget(escAC.Name)
					// runEscalationLocal adds the user turn and manages session state itself;
					// pre-adding it here would create duplicate consecutive user messages,
					// which Bedrock's Converse API rejects.
					return runEscalationLocal(ctx, cfg, sess, escAgent, mem, prompt, out)
				}
			}
			// For Claude CLI: pre-add the user turn so BuildContext includes it in the
			// system-prompt context block sent to the CLI escalation agent.
			sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentLocal, Content: prompt})
			// Set brief before rebuild so the brick includes it.
			sess.EscalationBrief = esc.Reason
			sess.RebuildSummaryBricks(cfg.ContextBudget())
			sess.ForceState(session.StateRouting)
			session.Save(sess) //nolint:errcheck
			escalateAgent := newCLIAgent(cliAgentConfig(cfg))
			escalateAgent = applyAWSCreds(cfg, escalateAgent)
			var localCs *claudesettings.Store
			if cwd, err := os.Getwd(); err == nil {
				localCs, _ = claudesettings.Open(cwd)
			}
			return runCLIEscalationWith(ctx, cfg, sess, escalateAgent, prompt, newStdinInputReader(), permContext{cs: localCs}, mem, out)
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
		sess.RebuildSummaryBricks(cfg.ContextBudget())
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

func runCLIEscalation(ctx context.Context, cfg config.Config, sess *session.Session, agent *claude.Agent, prompt string, cs *claudesettings.Store, mem *memory.Store) error {
	return runCLIEscalationWith(ctx, cfg, sess, agent, prompt, newStdinInputReader(), permContext{cs: cs}, mem)
}

// runEscalationLocal handles escalated turns routed to a second local provider
// (when cfg.EscalationAgent names an agents entry with a non-claude-cli provider).
// It injects the session context via the system prompt so the escalation agent
// sees prior history, and stores turns under AgentEscalation so session state and
// history-filtering logic work without modification.
func runEscalationLocal(ctx context.Context, cfg config.Config, sess *session.Session, agent *local.Agent, mem *memory.Store, prompt string, outs ...io.Writer) error {
	out := io.Writer(os.Stdout)
	if len(outs) > 0 && outs[0] != nil {
		out = outs[0]
	}
	escName := strings.ToLower(strings.TrimSpace(cfg.EscalationAgentConfig().Name))
	if escName == "" {
		escName = "escalation"
	}
	fmt.Fprint(out, bold(blue(escName+":"))+" ")
	aw := newActivityWriter(out)

	// Build history that the escalation agent should see: all turns, not just
	// the primary-local ones, so it has full context.
	history := escalationLocalHistory(sess, prompt)

	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentEscalation, Content: prompt})
	sess.ForceState(session.StateEscalation)

	updatedHistory, err := agent.Run(ctx, history, prompt, aw, sess, mem)
	aw.Done()
	if err != nil {
		// Escalation-local agent cannot itself escalate further.
		return err
	}

	assistantContent := ""
	if len(updatedHistory) > 0 {
		last := updatedHistory[len(updatedHistory)-1]
		if last.Role == "assistant" {
			assistantContent = last.Content
		}
	}
	if assistantContent != "" {
		sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentEscalation, Content: assistantContent})
	}

	sess.ForceState(session.StateRouting)
	return session.Save(sess)
}

// escalationLocalHistory converts session turns to local.Message format for
// the escalation-local agent. Unlike sessionToMessages (which filters to local
// turns only), this includes all prior turns so the escalation agent has full
// context across both agents.
//
// prompt is the current user prompt being escalated. Any trailing unanswered
// user turn matching prompt is stripped: it was added by runLocal's escalation
// path and is about to be sent as the live prompt — including it in history
// would trigger isRepeatedPrompt in Run and cause a spurious EscalationSignal.
func escalationLocalHistory(sess *session.Session, prompt string) []local.Message {
	var msgs []local.Message
	for _, t := range sess.History {
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
	// Strip a trailing unanswered user turn that matches the prompt being escalated.
	// Such a turn has no following assistant turn and would cause isRepeatedPrompt to
	// fire inside the escalation agent's Run, masking the real error.
	if n := len(msgs); n > 0 && msgs[n-1].Role == "user" && msgs[n-1].Content == prompt {
		msgs = msgs[:n-1]
	}
	return msgs
}

func runCLIEscalationWith(ctx context.Context, cfg config.Config, sess *session.Session, agent *claude.Agent, prompt string, input inputReader, pc permContext, mem *memory.Store, outs ...io.Writer) error {
	out := io.Writer(os.Stdout)
	if len(outs) > 0 && outs[0] != nil {
		out = outs[0]
	}
	fmt.Fprint(out, cliLabelStyled(agent)+" ")
	aw := newActivityWriter(out)
	resuming := sess.State == session.StateEscalationWaiting && sess.EscalationSessionID != ""
	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentEscalation, Content: prompt})
	sess.ForceState(session.StateEscalation)

	// Generate a fresh nonce for this escalation turn. The same nonce is embedded in
	// the system-prompt instructions (BuildContext, MemoryInstruction, NeedInstruction)
	// and in the stream parser, so only tags containing this nonce are captured.
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
				consumer = memory.ConsumerEscalation
			}
			_, err := mem.Record(ctx, content, memory.ProducerEscalation, consumer, memory.Roles{}, false)
			// DuplicateError is expected when Claude emits a percept similar to one
			// already stored — silently drop it; any other error is also non-fatal.
			_ = err
		}, nonce)
	}
	agent = agent.WithOnNeed(func(content string) {
		sess.CurrentNeed = content
	}, nonce)

	res, err := runCLIEscalationAgent(ctx, sess, agent, prompt, aw, resuming, nonce, perceptsForEscalation(mem))
	aw.Done()
	if err != nil {
		return err
	}

	if sess.EscalationSessionID != "" {
		res = handlePermissionDenials(ctx, sess, agent, res, input, out, pc, nonce)
	}

	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentEscalation, Content: res.Text})
	sess.RebuildSummaryBricks(cfg.ContextBudget())
	if res.EndsWithQ {
		sess.ForceState(session.StateEscalationWaiting)
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

// runCLIEscalationAgent runs one escalation turn (first or resume) and returns the result.
// nonce is the session-specific percept nonce embedded in the system-prompt instruction.
// percepts are injected as a [Remembered facts] block in the system prompt.
func runCLIEscalationAgent(ctx context.Context, sess *session.Session, agent *claude.Agent, prompt string, out io.Writer, resuming bool, nonce string, percepts []string) (claude.ParseResult, error) {
	sysContext := escalation.BuildContext(sess, nonce, percepts, resuming)
	if resuming {
		return agent.RunResume(ctx, sess.EscalationSessionID, sysContext, prompt, out)
	}
	claudeSessionID, res, err := agent.RunFirst(ctx, sysContext, prompt, out)
	if err == nil {
		sess.EscalationSessionID = claudeSessionID
	}
	return res, err
}

// handlePermissionDenials checks the result for permission issues and retries if the user approves.
func handlePermissionDenials(ctx context.Context, sess *session.Session, agent *claude.Agent, res claude.ParseResult, input inputReader, out io.Writer, pc permContext, nonce string) claude.ParseResult {
	if len(res.PermissionDenials) > 0 {
		return handleStructuredDenials(ctx, sess, agent, res, input, out, pc, nonce)
	}
	return res
}

// permContext bundles the mutable permission state threaded through a CLI escalation turn.
type permContext struct {
	cs          *claudesettings.Store
	toolFutures map[string]chan string // tool name → buffered channel pre-filled by OnToolUse
}

// handleStructuredDenials handles permission_denials from the result event —
// language-neutral, fires regardless of the escalation agent's response language.
func handleStructuredDenials(ctx context.Context, sess *session.Session, agent *claude.Agent, res claude.ParseResult, input inputReader, out io.Writer, pc permContext, nonce string) claude.ParseResult {
	fmt.Fprintf(out, "\n%s escalation agent was blocked from using:\n", milkTag())
	retryAgent, changed := applyDenials(agent, dedupDenials(res.PermissionDenials), input, out, pc)
	if !changed {
		return res
	}
	fmt.Fprint(out, cliLabelStyled(retryAgent)+" ")
	retried, err := retryAgent.RunResume(ctx, sess.EscalationSessionID, escalation.MemoryInstruction(nonce), "Please continue with the approved permissions.", out)
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
			if filepath.IsAbs(token) {
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

// makePermissionHandler returns a PermissionHandler for single-shot (non-TUI)
// mode. It asks y/n interactively and, on approval, persists the grant to the
// project settings so the tool is auto-allowed on future runs.
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

// cliToolArgSummary picks the most informative single argument value for display,
// mirroring the local agent's toolArgSummary.
func cliToolArgSummary(args map[string]any) string {
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
// waiting for the user's y/n reply before forwarding allow/deny to the CLI escalation agent.
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

// printPermissionRequest shows the user what the escalation agent is asking permission for.
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
// Escalation agent turns are excluded: the local model should only see its own prior conversation.
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

// perceptsForEscalation returns the content strings of all percepts that Claude
// should receive at session start: those not exclusively targeted at the local
// agent and not already produced by Claude (to avoid echo loops).
func perceptsForEscalation(mem *memory.Store) []string {
	if mem == nil {
		return nil
	}
	all := mem.List(memory.ListOpts{})
	var out []string
	for _, p := range all {
		if p.Producer == memory.ProducerEscalation {
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
		ac := cfg.ActiveAgent()
		cac := cliAgentConfig(cfg)
		fmt.Printf("agent:          %s\n", cfg.Agent)
		fmt.Printf("agent_url:      %s\n", ac.URL)
		fmt.Printf("agent_model:    %s\n", ac.Model)
		fmt.Printf("cli_bin:     %s\n", cac.Bin)
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
