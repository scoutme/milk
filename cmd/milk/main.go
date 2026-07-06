package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"

	"github.com/scoutme/milk/internal/agent/aider"
	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/agent/smolagent"
	"github.com/scoutme/milk/internal/agent/subprocess"
	"github.com/scoutme/milk/internal/claudesettings"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/diff"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/mcp"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
)

const milkScope = "github.com/scoutme/milk"

var (
	flagEscalate bool
	flagPrimary  bool
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
	rootCmd.Flags().BoolVar(&flagPrimary, "primary", false, "Force route to primary agent for this turn")
	rootCmd.Flags().BoolVar(&flagNew, "new", false, "Start a new session")
	rootCmd.Flags().StringVar(&flagSession, "session", "", "Target session by name")
	rootCmd.Flags().BoolVarP(&flagContinue, "continue", "c", false, "Resume current session (default behavior, explicit alias)")
	rootCmd.Flags().BoolVar(&flagList, "list", false, "List sessions for current cwd")
	rootCmd.Flags().BoolVar(&flagListAll, "all", false, "With --list: show all sessions across all directories")
	rootCmd.Flags().BoolVar(&flagDrop, "drop", false, "Delete the current session")

	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(otelCmd)
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

	for _, w := range config.Validate(cfg) {
		fmt.Fprintf(os.Stderr, "%s config warning: %s\n", milkTag(), w)
	}

	sess, err := loadSessionForRun(cwd)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}

	sessionStart := time.Now()
	obsShutdown := initObs(cfg)
	defer func() {
		obs.SetGauge(context.Background(), milkScope, "milk.session.duration_ms",
			time.Since(sessionStart).Milliseconds(),
		)
		obsShutdown(context.Background()) //nolint:errcheck
	}()

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

	primaryRunner, localAgent, err := buildPrimaryRunner(ctx, cfg, cwd, sess)
	if err != nil {
		return err
	}
	escalationRunner, err := buildEscalationRunner(ctx, cfg, cwd, sess)
	if err != nil {
		return err
	}

	localAvail := primaryRunner != nil && primaryRunner.Ping() == nil
	escalationAvail := escalationRunner != nil && escalationRunner.Ping() == nil

	if !localAvail {
		fmt.Fprintf(os.Stderr, "%s warning: %s primary agent unreachable\n", milkTag(), cfg.ActiveAgent().Name)
	}
	if !escalationAvail {
		fmt.Fprintf(os.Stderr, "%s warning: escalation agent unavailable\n", milkTag())
	}
	if !localAvail && !escalationAvail {
		return fmt.Errorf("neither primary nor escalation agent is available")
	}

	// Router uses the local HTTP agent for classification; nil when primary is subprocess.
	var routeLocalAgent *local.Agent
	if localAvail && localAgent != nil {
		routeLocalAgent = localAgent
	}
	rtr := router.New(cfg, routeLocalAgent)

	decision, err := rtr.Route(ctx, sess, prompt, flagEscalate, flagPrimary)
	if err != nil {
		return fmt.Errorf("routing: %w", err)
	}

	target := resolveTarget(decision.Target, localAvail, escalationAvail)

	targetLabel := string(target)
	sourceLabel := turnSourceLabel(flagEscalate, flagPrimary)

	turnStart := time.Now()
	var turnErr error
	switch target {
	case router.TargetLocal:
		if mem != nil {
			defer func() {
				_ = mem.Consolidate()
				_ = mem.PruneGlobal(cfg.PerceptStoreSizeLimit())
			}()
		}
		turnErr = runPrimary(ctx, cfg, sess, primaryRunner, escalationRunner, mem, prompt, os.Stdout, nil)
	case router.TargetEscalation:
		turnErr = runEscalation(ctx, cfg, sess, escalationRunner, "", mem, prompt, os.Stdout)
	default:
		return fmt.Errorf("unknown routing target: %s", target)
	}

	obs.Inc(ctx, milkScope, "milk.turns.total",
		attribute.String("target", targetLabel),
		attribute.String("source", sourceLabel),
	)
	obs.RecordDuration(ctx, milkScope, "milk.turns.latency_ms", time.Since(turnStart),
		attribute.String("target", targetLabel),
	)
	if turnErr != nil {
		obs.Inc(ctx, milkScope, "milk.turns.errors",
			attribute.String("target", targetLabel),
			attribute.String("kind", "inference"),
		)
	}
	return turnErr
}

// buildPrimaryRunner constructs the TurnRunner for the primary agent role.
// Also returns the underlying *local.Agent when it exists (needed for the router classifier).
func buildPrimaryRunner(_ context.Context, cfg config.Config, cwd string, sess *session.Session) (TurnRunner, *local.Agent, error) {
	primaryAC := cfg.ActiveAgent()
	if primaryAC.IsExternalProcess() && !primaryAC.IsCLI() {
		var sp *subprocess.Agent
		switch {
		case primaryAC.IsSubprocess():
			if primaryAC.Bin == "" {
				if scriptPath, scriptErr := ensureSmolagentScript(); scriptErr != nil {
					fmt.Fprintf(os.Stderr, "%s warning: could not deploy milk-smolagent: %v\n", milkTag(), scriptErr)
				} else {
					primaryAC.Bin = scriptPath
				}
			}
			sp = smolagent.New(primaryAC)
		case primaryAC.IsAiderCLI():
			sp = aider.New(primaryAC)
		}
		if sp == nil {
			return nil, nil, fmt.Errorf("unsupported subprocess provider: %s", primaryAC.Provider)
		}
		sp = sp.WithLogContext(cfg.Otel.LogContext)
		r := newSubprocessRunner(sp, primaryAC.Name)
		if servers, ts := buildMCPToolSet(context.Background(), cfg, primaryAC.Name); ts != nil {
			r = r.withMCPToolSet(servers, ts)
		}
		return r, nil, nil
	}

	ac := applyFreshAWSCreds(cfg, primaryAC)
	la := local.NewFromConfig(ac)
	if od, err := config.OtelDir(); err == nil {
		la.WithOtelDir(od)
	}
	la.WithLogContext(cfg.Otel.LogContext)
	la.WithOnTokens(func(model, role string, prompt, completion int64) {
		sess.AddTokens(model, role, prompt, completion)
	})
	if lp, err := local.OpenPermStore(cwd); err == nil {
		la.WithPermissions(lp, nil)
	}
	la = la.WithSkipPermissions(cliAgentConfig(cfg).DangerouslySkipPermissions)
	if dbg, err := openLocalDebugLog(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: cannot open local debug log: %v\n", milkTag(), err)
	} else if dbg != nil {
		la = la.WithDebugLog(dbg)
	}
	name := primaryAC.Name
	if name == "" {
		name = "primary"
	}
	la = attachMCPToolSet(context.Background(), cfg, primaryAC.Name, la)
	return newLocalRunner(la, name), la, nil
}

// buildEscalationRunner constructs the TurnRunner for the escalation agent role.
func buildEscalationRunner(_ context.Context, cfg config.Config, cwd string, sess *session.Session) (TurnRunner, error) {
	escAC := cfg.EscalationAgentConfig()

	if escAC.IsExternalProcess() && !escAC.IsCLI() {
		var sp *subprocess.Agent
		switch {
		case escAC.IsSubprocess():
			if escAC.Bin == "" {
				if scriptPath, scriptErr := ensureSmolagentScript(); scriptErr != nil {
					fmt.Fprintf(os.Stderr, "%s warning: could not deploy milk-smolagent: %v\n", milkTag(), scriptErr)
				} else {
					escAC.Bin = scriptPath
				}
			}
			sp = smolagent.New(escAC)
		case escAC.IsAiderCLI():
			sp = aider.New(escAC)
		}
		if sp != nil {
			sp = sp.WithLogContext(cfg.Otel.LogContext)
			r := newSubprocessRunner(sp, escAC.Name)
			if servers, ts := buildMCPToolSet(context.Background(), cfg, escAC.Name); ts != nil {
				r = r.withMCPToolSet(servers, ts)
			}
			return r, nil
		}
	}

	if !escAC.IsCLI() {
		freshEscAC := applyFreshAWSCreds(cfg, escAC)
		if freshEscAC.URL != "" {
			la := local.NewFromConfig(freshEscAC).AsEscalationTarget(freshEscAC.Name)
			if od, err := config.OtelDir(); err == nil {
				la.WithOtelDir(od)
			}
			la.WithLogContext(cfg.Otel.LogContext)
			la.WithOnTokens(func(model, role string, prompt, completion int64) {
				sess.AddTokens(model, role, prompt, completion)
			})
			la = la.WithSkipPermissions(cliAgentConfig(cfg).DangerouslySkipPermissions)
			if lp, err := local.OpenPermStore(cwd); err == nil {
				la.WithPermissions(lp, nil)
			}
			name := escAC.Name
			if name == "" {
				name = "escalation"
			}
			la = attachMCPToolSet(context.Background(), cfg, escAC.Name, la)
			return newLocalRunner(la, name), nil
		}
		fmt.Fprintf(os.Stderr, "%s warning: escalation_agent %q not found in agents — falling back to claude-cli\n", milkTag(), cfg.EscalationAgent)
	}

	// Default: Claude CLI escalation agent.
	cliAC := cliAgentConfig(cfg)
	cliAgt := newCLIAgent(cliAC)
	cliAgt = applyAWSCreds(cfg, cliAgt)
	cliAgt = cliAgt.WithLogContext(cfg.Otel.LogContext)
	if dbg, err := openCLIDebugLog(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: cannot open claude debug log: %v\n", milkTag(), err)
	} else if dbg != nil {
		cliAgt = cliAgt.WithDebugLog(dbg)
	}
	var cs *claudesettings.Store
	if store, err := claudesettings.Open(cwd); err == nil {
		cs = store
	}
	name := cliAC.Name
	if name == "" {
		name = "claude"
	}
	r := newCLIRunner(cliAgt, name, permContext{cs: cs, cwd: cwd}, func() inputReader { return newStdinInputReader() })
	if servers := cfg.EffectiveMCPServers(cliAC.Name); len(servers) > 0 {
		r = r.withMCPServers(servers)
	}
	return r, nil
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

// attachMCPToolSet builds an mcp.ToolSet from the MCP servers configured for
// agentName, connects all clients concurrently, and wires it into la via
// WithMCPToolSet. Errors during connect are logged as warnings — partial
// connectivity is preferred over a hard startup failure. When no MCP servers
// are configured for the agent, la is returned unchanged.
func attachMCPToolSet(ctx context.Context, cfg config.Config, agentName string, la *local.Agent) *local.Agent {
	servers := cfg.EffectiveMCPServers(agentName)
	if len(servers) == 0 {
		return la
	}
	clients := make([]*mcp.Client, 0, len(servers))
	for _, s := range servers {
		clients = append(clients, mcp.New(s))
	}
	ts := mcp.NewToolSet(clients)
	connectCtx, connectCancel := context.WithTimeout(ctx, 5*time.Second)
	defer connectCancel()
	if err := ts.ConnectAll(connectCtx); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: MCP connect error for agent %q: %v\n", milkTag(), agentName, err)
		obs.Info("mcp.attach.failed", "agent", agentName, "error", err.Error())
	}
	// Always wire the ToolSet even if no clients connected at startup.
	// Lazy reconnect inside Schemas() / Dispatch() will retry on first use.
	la = la.WithMCPToolSet(ts)
	return la
}

// buildMCPToolSet builds a connected mcp.ToolSet for agentName using the servers
// from cfg, or returns (nil, nil) when no servers are configured. Errors are
// logged as warnings; partial connectivity is acceptable — lazy reconnect retries on use.
func buildMCPToolSet(ctx context.Context, cfg config.Config, agentName string) ([]config.MCPServerConfig, *mcp.ToolSet) {
	servers := cfg.EffectiveMCPServers(agentName)
	if len(servers) == 0 {
		return nil, nil
	}
	clients := make([]*mcp.Client, 0, len(servers))
	for _, s := range servers {
		clients = append(clients, mcp.New(s))
	}
	ts := mcp.NewToolSet(clients)
	connectCtx, connectCancel := context.WithTimeout(ctx, 5*time.Second)
	defer connectCancel()
	if err := ts.ConnectAll(connectCtx); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: MCP connect error for agent %q: %v\n", milkTag(), agentName, err)
		obs.Info("mcp.attach.failed", "agent", agentName, "error", err.Error())
	}
	return servers, ts
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

func openLocalDebugLog(cfg config.Config) (*os.File, error) {
	if !cfg.DebugLocalLog {
		return nil, nil
	}
	path, err := config.LocalDebugLogPath()
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

func resolveTarget(target router.Target, localAvail, escalationAvail bool) router.Target {
	if target == router.TargetLocal && !localAvail {
		return router.TargetEscalation
	}
	if target == router.TargetEscalation && !escalationAvail {
		return router.TargetLocal
	}
	return target
}

// turnSourceLabel returns the "source" label for milk.turns.total based on
// which flag or routing mode triggered the turn.
func turnSourceLabel(explicitEscalate, explicitPrimary bool) string {
	if explicitEscalate || explicitPrimary {
		return "user"
	}
	return "auto"
}

// logStateTransition emits a debug log entry and metric for a session state change.
func logStateTransition(sess *session.Session, next session.State, trigger string) {
	obs.Debug("state transition", "from", string(sess.State), "to", string(next), "trigger", trigger)
	obs.Inc(context.Background(), milkScope, "milk.session.state_transitions",
		attribute.String("from", string(sess.State)),
		attribute.String("to", string(next)),
	)
}

const cliLabel = "claude:"

func cliLabelStyled(a *claude.Agent) string {
	if a.SkipPermissions() {
		return bold(red(cliLabel))
	}
	return bold(blue(cliLabel))
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

// escalationLocalHistoryFresh is like escalationLocalHistory but only includes
// turns since the last escalation boundary (i.e. turns after the most recent
// escalation assistant turn). Used on stale-returning turns to avoid passing
// the full prior escalation history through the messages array.
func escalationLocalHistoryFresh(sess *session.Session, prompt string) []local.Message {
	var msgs []local.Message
	for _, t := range sess.History[session.LastEscalationBoundary(sess):] {
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
	if n := len(msgs); n > 0 && msgs[n-1].Role == "user" && msgs[n-1].Content == prompt {
		msgs = msgs[:n-1]
	}
	return msgs
}

// applyPersistedGrants loads previously-approved tools and directories from
// settings.json and wires them into the agent so grants survive across turns.
// In single-shot mode it also installs the interactive permission handler.
func applyPersistedGrants(agent *claude.Agent, pc permContext) *claude.Agent {
	// Always trust the working directory so Claude's directory-trust check never
	// fires as a silent "Stream closed" error before the permission handler is active.
	if pc.cwd != "" {
		agent = agent.WithExtraDir(pc.cwd)
	}
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

// shouldInjectMemoryInstructions returns true when the memory/need instruction
// block must be included in this escalation turn's system-prompt context.
// Always injects on first escalation (not resuming). On subsequent resume turns
// skips injection unless the turn-count or byte-volume threshold has been crossed
// since the last injection.
func shouldInjectMemoryInstructions(cfg config.Config, sess *session.Session, resuming bool) bool {
	if !resuming {
		return true
	}
	escAC := cfg.EscalationAgentConfig()
	turnThreshold := cfg.AgentMemoryReinjectionTurnThreshold(escAC, false)
	byteThreshold := cfg.AgentMemoryReinjectionByteThreshold(escAC, false)
	if turnThreshold == 0 && byteThreshold == 0 {
		return false
	}
	turnsSince := sess.EscalationTurnCount() - sess.MemoryInstructionInjectedAt
	if turnThreshold > 0 && turnsSince >= turnThreshold {
		return true
	}
	bytesSince := sess.EscalationOutputBytesSince(sess.MemoryInstructionInjectedAt)
	if byteThreshold > 0 && bytesSince >= byteThreshold {
		return true
	}
	return false
}

// handlePermissionDenials checks the result for permission issues and retries if the user approves.
func handlePermissionDenials(ctx context.Context, sess *session.Session, agent *claude.Agent, res claude.ParseResult, input inputReader, out io.Writer, pc permContext, nonce string, primaryName, escalationName string) claude.ParseResult {
	if len(res.PermissionDenials) > 0 {
		return handleStructuredDenials(ctx, sess, agent, res, input, out, pc, nonce, primaryName, escalationName)
	}
	return res
}

// permContext bundles the mutable permission state threaded through a CLI escalation turn.
type permContext struct {
	cs          *claudesettings.Store
	cwd         string                 // working directory; always passed as --add-dir so trust checks don't silently fail
	toolFutures map[string]chan string // tool name → buffered channel pre-filled by OnToolUse
	// contextHash, when non-nil, holds the hash of the last --append-system-prompt-file
	// sent to the escalation agent. runCLIEscalationAgent skips re-sending the file when
	// the hash is unchanged, preserving Claude's prompt cache prefix.
	contextHash *string
}

// handleStructuredDenials handles permission_denials from the result event —
// language-neutral, fires regardless of the escalation agent's response language.
func handleStructuredDenials(ctx context.Context, sess *session.Session, agent *claude.Agent, res claude.ParseResult, input inputReader, out io.Writer, pc permContext, nonce string, primaryName, escalationName string) claude.ParseResult {
	denials := dedupDenials(res.PermissionDenials)

	// Partition AskUserQuestion denials from regular tool-permission denials.
	var askDenials, regularDenials []claude.PermissionDenialRecord
	for _, d := range denials {
		if d.ToolName == "AskUserQuestion" {
			askDenials = append(askDenials, d)
		} else {
			regularDenials = append(regularDenials, d)
		}
	}

	// For AskUserQuestion, collect answers and resume with them injected as text.
	if len(askDenials) > 0 {
		resumePrompt := buildAskUserQuestionAnswers(askDenials, input, out)
		fmt.Fprint(out, cliLabelStyled(agent)+" ")
		retried, err := agent.RunResume(ctx, sess.EscalationSessionID, escalation.MemoryInstruction(nonce, primaryName, escalationName), "", resumePrompt, out)
		if err != nil {
			return res
		}
		// If there were also regular denials, handle them on the retried result.
		if len(regularDenials) > 0 && len(retried.PermissionDenials) > 0 {
			return handleStructuredDenials(ctx, sess, agent, retried, input, out, pc, nonce, primaryName, escalationName)
		}
		return retried
	}

	fmt.Fprintf(out, "\n%s escalation agent was blocked from using:\n", milkTag())
	retryAgent, changed := applyDenials(agent, regularDenials, input, out, pc)
	if !changed {
		return res
	}
	fmt.Fprint(out, cliLabelStyled(retryAgent)+" ")
	retried, err := retryAgent.RunResume(ctx, sess.EscalationSessionID, escalation.MemoryInstruction(nonce, primaryName, escalationName), "", "Please continue with the approved permissions.", out)
	if err != nil {
		return res
	}
	return retried
}

// buildAskUserQuestionAnswers presents each AskUserQuestion denial to the user,
// collects their selections, and returns a prompt string that tells Claude the answers.
// input.readLine handles display: in single-shot mode it prints to stdout; in TUI mode
// it sends a permRequestMsg that the TUI renders in the transcript.
func buildAskUserQuestionAnswers(denials []claude.PermissionDenialRecord, input inputReader, _ io.Writer) string {
	var answers []string
	for _, d := range denials {
		questions := claude.ParseAskUserQuestionInput(d.ToolInput)
		for _, q := range questions {
			var promptBuf strings.Builder
			fmt.Fprintf(&promptBuf, "\n%s %s\n", milkTag(), bold(q.Question))
			for i, opt := range q.Options {
				if opt.Description != "" {
					fmt.Fprintf(&promptBuf, "  %d. %s — %s\n", i+1, bold(opt.Label), opt.Description)
				} else {
					fmt.Fprintf(&promptBuf, "  %d. %s\n", i+1, bold(opt.Label))
				}
			}
			fmt.Fprintf(&promptBuf, "%s Select [1-%d]: ", milkTag(), len(q.Options))

			type labeledReader interface {
				readLineLabeled(prompt, label string) (string, error)
			}
			var line string
			if lr, ok := input.(labeledReader); ok {
				line, _ = lr.readLineLabeled(promptBuf.String(), "[select]")
			} else {
				line, _ = input.readLine(promptBuf.String())
			}
			line = strings.TrimSpace(line)
			chosen := ""
			for i, opt := range q.Options {
				if line == fmt.Sprintf("%d", i+1) || strings.EqualFold(line, opt.Label) {
					chosen = opt.Label
					break
				}
			}
			if chosen == "" {
				chosen = line // pass free-text through verbatim
			}
			if chosen != "" {
				answers = append(answers, fmt.Sprintf("%s: %s", q.Question, chosen))
			}
		}
	}
	if len(answers) == 0 {
		return "Please continue."
	}
	return "My answers to your questions:\n" + strings.Join(answers, "\n")
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
// mirroring the local agent's toolArgSummary. Returns the full value — truncation
// is done at the call site using terminal width.
func cliToolArgSummary(args map[string]any) string {
	for _, key := range []string{"command", "path", "file_path", "url", "query", "pattern", "reason", "content"} {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// truncateToolSummary truncates a tool summary string to fit within termWidth,
// accounting for the prefix "⚙ <name>: ". Pass termWidth=0 to skip truncation.
func truncateToolSummary(name, summary string, termWidth int) string {
	if termWidth <= 0 || summary == "" {
		return summary
	}
	prefix := "⚙ " + name + ": "
	maxSummary := termWidth - len(prefix) - 4 // 4 = margin
	if maxSummary < 10 {
		maxSummary = 10
	}
	runes := []rune(summary)
	if len(runes) > maxSummary {
		return string(runes[:maxSummary-1]) + "…"
	}
	return summary
}

// cliToolDiff returns a colored inline diff for Claude CLI file-edit tool calls.
// Handles the Edit tool (old_string/new_string) and Write tool (content).
func cliToolDiff(name string, input map[string]any) string {
	switch name {
	case "Edit":
		path, _ := input["file_path"].(string)
		oldStr, _ := input["old_string"].(string)
		newStr, _ := input["new_string"].(string)
		if path == "" || oldStr == "" {
			return ""
		}
		return diff.ForEdit(path, oldStr, newStr, 3)
	case "Write":
		path, _ := input["file_path"].(string)
		content, _ := input["content"].(string)
		if path == "" {
			return ""
		}
		return diff.ForWrite(path, content, 3)
	}
	return ""
}

// makeTUIPermissionHandler returns a PermissionHandler for TUI mode.
// It races the TUI ask against the remote notifier: whichever responds first
// (TUI y/n or remote allow/deny) wins. cs may be nil — persistence is best-effort.
// notifier may be nil — treated as Noop.
func makeTUIPermissionHandler(input inputReader, cs *claudesettings.Store, notifier oversight.Notifier) claude.PermissionHandler {
	if notifier == nil {
		notifier = oversight.Noop{}
	}
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
		if summary := cliToolArgSummary(b.Input); summary != "" {
			prompt += fmt.Sprintf("  %s\n", dim(summary))
		}
		if b.Description != "" {
			prompt += fmt.Sprintf("  %s\n", b.Description)
		}
		prompt += fmt.Sprintf("%s Allow? [Y/n] ", milkTag())

		// Race TUI input against remote notifier. Cancel the losing goroutine as
		// soon as the first result arrives so neither leaks.
		type result struct{ allow bool }
		ch := make(chan result, 2)
		raceCtx, cancelRace := context.WithCancel(context.Background())
		defer cancelRace()

		go func() {
			// readLine blocks on a channel internally; we must also watch raceCtx
			// so this goroutine exits when the Telegram side wins.
			type lineResult struct {
				yn  string
				err error
			}
			lineCh := make(chan lineResult, 1)
			go func() {
				yn, err := input.readLine(prompt)
				lineCh <- lineResult{yn: yn, err: err}
			}()
			select {
			case lr := <-lineCh:
				yn := lr.yn
				if yn == "" {
					yn = "y"
				}
				ch <- result{allow: strings.EqualFold(yn, "y")}
			case <-raceCtx.Done():
			}
		}()

		go func() {
			dec := notifier.AskPermission(raceCtx, oversight.PermRequest{
				ToolName:    b.ToolName,
				Input:       cliToolArgSummary(b.Input),
				Description: b.Description,
				BlockedPath: b.BlockedPath,
			})
			select {
			case ch <- result{allow: dec == oversight.PermAllow}:
			case <-raceCtx.Done():
			}
		}()

		res := <-ch
		cancelRace()
		if res.allow {
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
	if summary := cliToolArgSummary(b.Input); summary != "" {
		fmt.Fprintf(out, "  %s\n", dim(summary))
	}
	if b.Description != "" {
		fmt.Fprintf(out, "  %s\n", b.Description)
	}
}

// messagesCharCount returns the total character count across all message contents.
func messagesCharCount(msgs []local.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n
}

// trimLocalMessages drops the oldest user+assistant pairs from msgs until the
// total character count is within budgetChars. Tool-result messages that follow
// a dropped assistant turn are also dropped. Returns the trimmed slice and true
// when any trimming occurred. budgetChars == 0 means no limit.
func trimLocalMessages(msgs []local.Message, budgetChars int) ([]local.Message, bool) {
	if budgetChars <= 0 || messagesCharCount(msgs) <= budgetChars {
		return msgs, false
	}
	for messagesCharCount(msgs) > budgetChars && len(msgs) > 0 {
		// Always drop the first message. If it was a user turn, also drop the
		// immediately following assistant+tool-result run so we don't leave an
		// orphaned assistant turn at the head.
		msgs = msgs[1:]
		for len(msgs) > 0 && msgs[0].Role != "user" {
			msgs = msgs[1:]
		}
	}
	return msgs, true
}

// sessionToMessages converts local-agent session turns to the local agent's Message format.
// Escalation agent turns are excluded: the local model should only see its own prior conversation.
// When the escalation agent was the most recent active agent (i.e. there are escalation turns
// after the last local turn), LastEscalationSummary is prepended so the local model knows what
// Claude just did. It is not injected if local was already the last agent, to avoid re-showing
// stale escalation context on every subsequent local turn.
func sessionToMessages(sess *session.Session) []local.Message {
	var msgs []local.Message
	if sess.LastEscalationSummary != "" && session.EscalationMostRecent(sess) {
		msgs = append(msgs, local.Message{
			Role:    "assistant",
			Content: "[Escalation agent summary]\n" + sess.LastEscalationSummary,
		})
	}
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
	// runPrimary adds the current user turn to session history before calling
	// Execute, but local.Agent.Run appends userPrompt separately — drop the
	// trailing user message here to avoid sending the same prompt twice.
	if n := len(msgs); n > 0 && msgs[n-1].Role == "user" {
		msgs = msgs[:n-1]
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
	// Pre-create a fresh empty session so the next plain `milk` invocation
	// starts clean rather than resuming the next oldest session in the index.
	if _, err := session.New(cwd, flagSession); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: could not create fresh session: %v\n", milkTag(), err)
	}
	return nil
}

// perceptsForEscalation returns the content strings of percepts that Claude
// should receive: those not exclusively targeted at the local agent and not
// already produced by Claude (to avoid echo loops). Results are relevance-gated
// against the prompt and size-capped per config before returning.
func perceptsForEscalation(cfg config.Config, mem *memory.Store, prompt string) []string {
	if mem == nil {
		return nil
	}
	// List returns percepts sorted by weight descending — required for LimitInjection.
	all := mem.List(memory.ListOpts{})
	var candidates []memory.Percept
	for _, p := range all {
		if p.Producer == memory.ProducerEscalation {
			continue // Claude wrote it; no need to echo it back
		}
		if p.Consumer == memory.ConsumerLocal {
			continue // explicitly local-only
		}
		candidates = append(candidates, p)
	}

	escAC := cfg.EscalationAgentConfig()
	if cfg.AgentPerceptRelevanceGateEnabled(escAC) {
		candidates = memory.FilterByRelevance(candidates, prompt)
	}

	candidates = memory.LimitInjection(candidates, cfg.AgentPerceptInjectMaxCount(escAC), cfg.AgentPerceptInjectMaxByteCount(escAC))

	out := make([]string, len(candidates))
	for i, p := range candidates {
		out[i] = p.Content
	}
	return out
}

// runInitWizard runs the CLI-mode init wizard, writes ~/.milk/config.json,
// and prints next-step guidance on success.
func runInitWizard() error {
	sc := bufio.NewScanner(os.Stdin)
	ask := func(prompt string) string {
		fmt.Print(prompt)
		if !sc.Scan() {
			return ""
		}
		return strings.TrimSpace(sc.Text())
	}
	askDefault := func(prompt, def string) string {
		v := ask(prompt)
		if v == "" {
			return def
		}
		return v
	}

	fmt.Println("milk config init — interactive setup")
	fmt.Println()

	name := askDefault("Primary agent name [local]: ", "local")

	fmt.Println()
	fmt.Println("Select primary agent provider:")
	fmt.Println("  1) local        — llama.cpp, Ollama, vLLM, LM Studio (plain HTTP)")
	fmt.Println("  2) bedrock      — AWS Bedrock Converse API")
	fmt.Println("  3) bearer       — OpenRouter, Together.ai, Groq, GitHub Copilot, any Bearer-token API")
	fmt.Println("  4) claude-cli   — Claude Code CLI subprocess (no HTTP server needed)")
	fmt.Println("  5) aider-cli    — aider subprocess")
	fmt.Println("  6) subprocess   — generic NDJSON subprocess agent")
	choice := askDefault("Choice [1]: ", "1")
	providerMap := map[string]string{
		"1": "local", "2": "bedrock", "3": "bearer",
		"4": "claude-cli", "5": "aider-cli", "6": "subprocess",
	}
	provider, ok := providerMap[choice]
	if !ok {
		provider = "local"
	}

	primary := config.AgentConfig{Name: name, Provider: provider}
	fmt.Println()
	switch provider {
	case "local":
		primary.URL = ask("Server URL (e.g. http://localhost:8080): ")
		primary.Model = ask("Model name: ")
	case "bedrock":
		primary.URL = ask("Bedrock endpoint URL (e.g. https://bedrock-runtime.<region>.amazonaws.com): ")
		primary.Model = ask("Model ARN: ")
		primary.AWSRegion = ask("AWS region (e.g. us-east-1): ")
	case "bearer":
		primary.URL = ask("Server URL (e.g. https://openrouter.ai/api/v1  ·  https://copilot-api.<org>.ghe.com  ·  https://<res>.cognitiveservices.azure.com/openai): ")
		switch {
		case isCopilotURL(primary.URL):
			primary.Headers = map[string]string{
				"Copilot-Integration-Id": "vscode-chat",
				"Editor-Plugin-Version":  "copilot-chat/0.49.0",
				"Editor-Version":         "vscode/1.121.0",
				"X-GitHub-Api-Version":   "2026-01-09",
			}
			fmt.Println("  (GitHub Copilot detected — headers preset automatically)")
			chatPath := askDefault("Chat path [/chat/completions]: ", "/chat/completions")
			if chatPath != "/v1/chat/completions" {
				primary.ChatPath = chatPath
			}
			primary.Model = ask("Model name (e.g. claude-sonnet-4.6 or gpt-4o): ")
			hint := "gh auth token"
			if h := copilotHostname(primary.URL); h != "" {
				hint = "gh auth token --hostname " + h
			}
			apiKey := ask("API key (leave blank to use token_cmd instead): ")
			if apiKey != "" {
				primary.APIKey = apiKey
			} else {
				primary.TokenCmd = askDefault(fmt.Sprintf("Token command [%s]: ", hint), hint)
			}
		case isAzureURL(primary.URL):
			fmt.Println("  (Azure OpenAI detected — api-key header will be used)")
			dep := azureDeployment(primary.URL)
			primary.Model = askDefault("Deployment/model name (e.g. gpt-4.1): ", dep)
			defPath := "/deployments/" + primary.Model + "/chat/completions"
			chatPath := askDefault(fmt.Sprintf("Chat path [%s]: ", defPath), defPath)
			if chatPath != "/v1/chat/completions" {
				primary.ChatPath = chatPath
			}
			apiKey := ask("Azure API key: ")
			if apiKey != "" {
				primary.Headers = map[string]string{"api-key": apiKey}
			}
		default:
			chatPath := askDefault("Chat path [/v1/chat/completions]: ", "/v1/chat/completions")
			if chatPath != "/v1/chat/completions" {
				primary.ChatPath = chatPath
			}
			primary.Model = ask("Model name: ")
			apiKey := ask("API key (leave blank to use token_cmd instead): ")
			if apiKey != "" {
				primary.APIKey = apiKey
			} else {
				primary.TokenCmd = ask("Token command (e.g. 'gh auth token' or 'op read op://vault/item/field'): ")
			}
		}
	case "aider-cli", "subprocess":
		primary.URL = ask("Server URL: ")
		primary.Model = ask("Model name: ")
	case "claude-cli":
		// nothing required
	}

	fmt.Println()
	escChoice := askDefault("Escalation agent — use Claude Code CLI? [Y/n]: ", "y")
	var escalation *config.AgentConfig
	if strings.ToLower(escChoice) != "n" {
		e := config.AgentConfig{Name: "claude", Provider: "claude-cli"}
		escalation = &e
	}

	cfg := config.InitConfig(primary, escalation)
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	fmt.Println()
	fmt.Println("config written to ~/.milk/config.json")
	fmt.Println()
	fmt.Println("next steps:")
	fmt.Println("  milk               — start the TUI")
	fmt.Println("  milk /config init  — re-run this wizard inside the TUI")
	if escalation != nil {
		fmt.Println("  /escalate          — pin a turn to Claude Code for complex work")
	}
	if primary.Provider == "bedrock" {
		fmt.Println()
		fmt.Println("tip: if you use short-lived STS credentials, add aws_refresh_cmd to your agent config to auto-renew on 403")
	}
	if isCopilotURL(primary.URL) || isAzureURL(primary.URL) {
		fmt.Println()
		fmt.Println("tip: set limits.message_budget_chars in your agent config to cap context size (e.g. 800000 for Copilot/Azure)")
	}
	return nil
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Print config as JSON (milk config open | init for more)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConfigPrint()
	},
}

func init() {
	configCmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Interactive setup wizard — configure primary and escalation agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInitWizard()
		},
	})
	configCmd.AddCommand(&cobra.Command{
		Use:   "open",
		Short: "Open config in $EDITOR or system default editor",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigOpen()
		},
	})
}

func runConfigPrint() error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func runConfigOpen() error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(dir, "config.json")

	// Load config to check for config_editors override.
	cfg, _ := config.Load()

	// Build candidate list from config_editors (with env expansion) or built-in defaults.
	// $EDITOR and $VISUAL are regular entries, expanded at runtime.
	defaultEditors := []string{"$EDITOR", "$VISUAL", "nano", "vim", "vi"}
	list := cfg.ConfigEditors
	if len(list) == 0 {
		list = defaultEditors
	}
	var candidates []string
	for _, e := range list {
		expanded := os.ExpandEnv(e)
		if expanded != "" {
			candidates = append(candidates, expanded)
		}
	}

	var editorCmd string
	var editorArgs []string
	for _, c := range candidates {
		parts := strings.Fields(c)
		if len(parts) == 0 {
			continue
		}
		if _, lerr := exec.LookPath(parts[0]); lerr == nil {
			editorCmd = parts[0]
			editorArgs = parts[1:]
			break
		}
	}
	if editorCmd == "" {
		return fmt.Errorf("no editor found — set $EDITOR or configure config_editors in config")
	}
	cmd := exec.Command(editorCmd, append(editorArgs, cfgPath)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// ── otel command ──────────────────────────────────────────────────────────────

var otelCmd = &cobra.Command{
	Use:   "otel",
	Short: "Manage observability settings",
}

func init() {
	debugCmd := &cobra.Command{
		Use:   "debug",
		Short: "Enable or disable full debug logging",
	}
	debugCmd.AddCommand(&cobra.Command{
		Use:   "enable",
		Short: "Enable debug logging (log_context, debug_claude_code, debug_local, log_level=DEBUG)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runOtelDebug(true, ""); err != nil {
				return err
			}
			cliPath, _ := config.CLIDebugLogPath()
			localPath, _ := config.LocalDebugLogPath()
			otelDir, _ := config.OtelDir()
			fmt.Printf("debug logging enabled\n  claude NDJSON → %s\n  local SSE     → %s\n  payloads      → %s/logs.jsonl\n", cliPath, localPath, otelDir)
			return nil
		},
	})
	debugCmd.AddCommand(&cobra.Command{
		Use:   "disable",
		Short: "Disable debug logging (restores log_level to pre-debug value; default INFO)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Read current config to preserve the pre-debug log level.
			cur, _ := config.Load()
			if err := runOtelDebug(false, cur.Otel.LogLevel); err != nil {
				return err
			}
			fmt.Println("debug logging disabled")
			return nil
		},
	})
	otelCmd.AddCommand(debugCmd)
}

// runOtelDebug enables or disables the full debug logging bundle.
// prevLevel is only used when enable=false: if the current persisted level is
// "DEBUG" (case-insensitive) it is restored to prevLevel (falling back to "INFO"
// when prevLevel is also "DEBUG" or empty), preserving any user-configured level.
func runOtelDebug(enable bool, prevLevel string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	cfg.Otel.LogContext = enable
	if enable {
		cfg.Otel.LogLevel = "DEBUG"
	} else if strings.EqualFold(cfg.Otel.LogLevel, "DEBUG") {
		// Restore to the caller's pre-debug level; fall back to INFO.
		if prevLevel != "" && !strings.EqualFold(prevLevel, "DEBUG") {
			cfg.Otel.LogLevel = prevLevel
		} else {
			cfg.Otel.LogLevel = "INFO"
		}
	}
	cfg.DebugCLILog = enable
	cfg.DebugLocalLog = enable
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}
