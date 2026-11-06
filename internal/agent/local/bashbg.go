package local

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/scoutme/milk/internal/livebuf"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/workflow/board"
)

// bashbg.go — background shells started via bash(run_in_background), with
// bash_output to read them and kill_bash to stop them. Lifecycle rules are
// recorded in ADR-0063:
//
//   - A shell outlives the turn that started it (it runs under the registry's
//     own context, like a background agent job) but never outlives milk:
//     StopBackgroundShells is called from the process exit path.
//   - Hard cap on concurrent shells; a shell is auto-killed after
//     bgShellMaxLifetime so a forgotten dev server cannot live forever.
//   - Output accumulates in a byte-capped livebuf.Buffer (oldest dropped), so
//     a chatty process cannot exhaust memory.
//   - Finished shells stay readable (bash_output) until reaped
//     (bgShellRetainAfterExit after exit, or when the registry fills up).

const (
	maxBackgroundShells     = 16
	bgShellMaxLifetime      = time.Hour
	bgShellRetainAfterExit  = 30 * time.Minute
	bgShellOutputBufferByte = 256 * 1024
)

type bgShell struct {
	id      string
	command string // the sh -c command line — kept for panel rows and ACP titles (ADR-0064)
	dir     string
	owner   string // who started it (workflow identity such as "worker·Item-2"); "" when unattributed
	cmd     *exec.Cmd
	out     *livebuf.Buffer
	done    chan struct{}
	settled chan struct{} // closed after exit bookkeeping: notice queued or suppressed, listeners fired
	started time.Time

	mu         sync.Mutex
	exitCode   int
	exitErr    error
	finished   bool
	finishedAt time.Time
	killed     bool
}

// ShellInfo is an exported snapshot of one background shell's state (ADR-0063
// lifecycle, ADR-0064 surface): the UI layer's read model for the background
// panel rows, /bg listing and ACP tool-call rows. Values are copies — the
// live output buffer is reached separately via ShellLive.
type ShellInfo struct {
	ID         string
	Command    string
	Dir        string
	Running    bool // started and not yet finished (may also be Killed — "stopping")
	Killed     bool // kill_bash (or the 1h auto-kill, or process exit) was requested
	Finished   bool
	ExitCode   int
	Err        error
	StartedAt  time.Time
	FinishedAt time.Time
}

// StatusLabel is the compact state word for UI rows and attach headers:
// "running", "stopping", "exited N", "killed", or the exec error's text.
func (i ShellInfo) StatusLabel() string {
	switch {
	case i.Running && i.Killed:
		return "stopping"
	case i.Running:
		return "running"
	case i.Err != nil:
		return i.Err.Error()
	case i.Killed:
		return fmt.Sprintf("killed (exit %d)", i.ExitCode)
	default:
		return fmt.Sprintf("exited %d", i.ExitCode)
	}
}

// info snapshots the shell's state under its lock.
func (s *bgShell) info() ShellInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ShellInfo{
		ID:         s.id,
		Command:    s.command,
		Dir:        s.dir,
		Running:    !s.finished,
		Killed:     s.killed,
		Finished:   s.finished,
		ExitCode:   s.exitCode,
		Err:        s.exitErr,
		StartedAt:  s.started,
		FinishedAt: s.finishedAt,
	}
}

// ShellWantsNotice is the single predicate for shell-completion delivery to
// the model (ADR-0100 as amended): spawn registers the interest — a finished
// shell delivers exactly like a background agent job, whatever its status.
// The only exception is a stopped shell (deliberate kill_bash or the lifetime
// cap): a stop the user or milk itself asked for is not news. Delivery
// follows the listener, never a model-set flag.
func ShellWantsNotice(i ShellInfo) bool {
	return i.Finished && !i.Killed
}

// statusLine renders the shell's state prefix for bash_output results.
func (s *bgShell) statusLine() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.killed && !s.finished:
		return fmt.Sprintf("[shell %s] killed (stopping)", s.id)
	case s.killed:
		return fmt.Sprintf("[shell %s] killed (exit code %d)", s.id, s.exitCode)
	case s.finished:
		if s.exitErr != nil {
			return fmt.Sprintf("[shell %s] exited: %v", s.id, s.exitErr)
		}
		return fmt.Sprintf("[shell %s] exited (code %d)", s.id, s.exitCode)
	default:
		pid := 0
		if s.cmd.Process != nil {
			pid = s.cmd.Process.Pid
		}
		return fmt.Sprintf("[shell %s] running since %s (pid %d)", s.id, s.started.Format(time.RFC3339), pid)
	}
}

// finishState reports whether the shell has exited and when.
func (s *bgShell) finishState() (finished bool, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished, s.finishedAt
}

type shellRegistry struct {
	mu         sync.Mutex
	shells     map[string]*bgShell
	nextID     int
	listeners  map[int]*shellListener
	nextListen int
	doneQueue  []ShellEvent // completion notices owed to the model (ADR-0100)
}

// ShellEvent is one queued shell-completion notice: the state snapshot plus
// the output tail captured at exit (the live buffer is reaped 30 min after
// exit, but the drain may run later). Queued by the registry itself — once
// per shell regardless of how many listeners are attached — and drained by
// the next dispatched turn, mirroring how background-job results reach the
// model (ADR-0043's drain pattern).
type ShellEvent struct {
	Info ShellInfo
	Tail string // last lines of output at exit ("" when there were none)
}

const (
	shellEventQueueCap = 32
	shellEventTailMax  = 15 // lines of output captured per event
)

// DrainShellEvents returns and clears every queued completion notice. Called
// from the dispatch paths next to drainBackgroundJobs — whoever turns runs
// next delivers them.
func DrainShellEvents() []ShellEvent {
	bgShells.mu.Lock()
	q := bgShells.doneQueue
	bgShells.doneQueue = nil
	bgShells.mu.Unlock()
	return q
}

// PendingShellEvents reports how many completion notices are waiting — the
// "pending" count the follow-up decision rules consume (a wake with nothing
// to drain would burn an empty turn).
func PendingShellEvents() int {
	bgShells.mu.Lock()
	defer bgShells.mu.Unlock()
	return len(bgShells.doneQueue)
}

// pushDoneEventLocked queues one completion notice. Caller holds r.mu.
func (r *shellRegistry) pushDoneEventLocked(e ShellEvent) {
	r.doneQueue = append(r.doneQueue, e)
	if len(r.doneQueue) > shellEventQueueCap {
		r.doneQueue = r.doneQueue[len(r.doneQueue)-shellEventQueueCap:]
	}
}

// shellListener is one registration from AddShellListener; pointers so the
// registry can nil-check a removed listener under the lock without copying.
type shellListener struct {
	onStart  func(ShellInfo)
	onFinish func(ShellInfo)
}

var bgShells = &shellRegistry{shells: map[string]*bgShell{}, listeners: map[int]*shellListener{}}

// AddShellListener registers lifecycle callbacks for every background shell
// (ADR-0064) and returns a remove func. The shell registry is process-global
// (shells are visible to every agent — that is the tool contract), so unlike
// Manager.SetOnStart this fans out to multiple listeners: one per TUI and one
// per ACP session. Callbacks fire synchronously on the lifecycle event's own
// goroutine (start: the tool dispatch that started the shell; finish: the
// shell's Wait goroutine) in registration order — keep them quick and never
// call back into the registry's start/stop paths.
func AddShellListener(onStart, onFinish func(ShellInfo)) (remove func()) {
	bgShells.mu.Lock()
	bgShells.nextListen++
	id := bgShells.nextListen
	bgShells.listeners[id] = &shellListener{onStart: onStart, onFinish: onFinish}
	bgShells.mu.Unlock()
	return func() {
		bgShells.mu.Lock()
		delete(bgShells.listeners, id)
		bgShells.mu.Unlock()
	}
}

// fireStartLocked / fireFinish call the registered listeners outside the
// registry lock. fireStart snapshots under r.mu; fireFinish takes no lock.
func (r *shellRegistry) fire(info ShellInfo, start bool) {
	r.mu.Lock()
	ls := make([]*shellListener, 0, len(r.listeners))
	for _, l := range r.listeners {
		ls = append(ls, l)
	}
	r.mu.Unlock()
	for _, l := range ls {
		if start {
			if l.onStart != nil {
				l.onStart(info)
			}
		} else if l.onFinish != nil {
			l.onFinish(info)
		}
	}
}

// BackgroundShells snapshots every tracked shell, oldest-started first,
// reaping finished shells past their retention window first (ADR-0063).
func BackgroundShells() []ShellInfo {
	return bgShells.list()
}

func (r *shellRegistry) list() []ShellInfo {
	r.mu.Lock()
	r.reapLocked()
	all := make([]ShellInfo, 0, len(r.shells))
	for _, s := range r.shells {
		all = append(all, s.info())
	}
	r.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].StartedAt.Before(all[j].StartedAt) })
	return all
}

// BackgroundShell looks up one shell's state snapshot by ID (the /bg show
// path); ok is false when the shell is unknown or has been reaped.
func BackgroundShell(id string) (ShellInfo, bool) {
	s, ok := bgShells.get(id)
	if !ok {
		return ShellInfo{}, false
	}
	return s.info(), true
}

// ShellLive returns the shell's live output buffer for attach/stream views
// (ADR-0047/0064). The buffer stays valid (and keeps accumulating) after the
// shell exits, until the shell is reaped.
func ShellLive(id string) (*livebuf.Buffer, bool) {
	s, ok := bgShells.get(id)
	if !ok {
		return nil, false
	}
	return s.out, true
}

// KillBackgroundShell stops a background shell from the UI surface (/bg stop,
// issue #252's follow-up): the same action as the kill_bash tool. Returns the
// shell's post-kill state snapshot and whether the shell exists at all — a
// shell that already exited is "found" but unchanged (its output stays
// readable).
func KillBackgroundShell(id string) (ShellInfo, bool) {
	return killShell(id, "ui", "")
}

// killShell stops shell id on behalf of requester (a workflow identity, or ""
// when unattributed) for the stated reason, logging who asked and whether the
// requester was not the shell's owner — the shell registry is process-global,
// so a parallel worker can stop a teammate's shell, and the log is the only
// place that shows it happened.
func killShell(id, reason, requester string) (ShellInfo, bool) {
	s, ok := bgShells.get(id)
	if !ok {
		return ShellInfo{}, false
	}
	s.kill(reason, requester)
	return s.info(), true
}

// StartBackgroundShell starts a background shell running `sh -c command` in
// dir (empty = the process cwd) — the exported entry behind the bash tool's
// run_in_background (ADR-0063), also used by the UI layers' tests. Returns
// the shell's initial state snapshot.
func StartBackgroundShell(command, dir string) (ShellInfo, error) {
	s, err := bgShells.start(command, dir, "")
	if err != nil {
		return ShellInfo{}, err
	}
	return s.info(), nil
}

// shellOutputTail keeps the last shellEventTailMax non-empty-trailing lines
// of a finished shell's output for its completion notice — enough for the
// decisive lines without shipping the whole buffer into the next prompt.
func shellOutputTail(out string) string {
	lines := strings.Split(out, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > shellEventTailMax {
		lines = lines[len(lines)-shellEventTailMax:]
	}
	return strings.Join(lines, "\n")
}

// StopBackgroundShells kills every tracked background shell. Called from the
// milk process exit path so no shell outlives the session (ADR-0063).
func StopBackgroundShells() {
	bgShells.mu.Lock()
	all := make([]*bgShell, 0, len(bgShells.shells))
	for _, s := range bgShells.shells {
		all = append(all, s)
	}
	bgShells.shells = map[string]*bgShell{}
	bgShells.mu.Unlock()
	obs.Info("bgshell.stop_all", "shells", len(all))
	for _, s := range all {
		s.kill("process_exit", "")
	}
}

// reapLocked drops finished shells past their retention window.
// Caller holds r.mu.
func (r *shellRegistry) reapLocked() {
	now := time.Now()
	for id, s := range r.shells {
		if done, at := s.finishState(); done && now.Sub(at) > bgShellRetainAfterExit {
			delete(r.shells, id)
		}
	}
}

// start registers and starts a background shell running sh -c command in dir.
// Spawn registers the delivery interest: the shell's completion is queued for
// the model when it exits, whatever its status — except kills (ADR-0100 as
// amended).
func (r *shellRegistry) start(command, dir, owner string) (*bgShell, error) {
	r.mu.Lock()
	r.reapLocked()
	if len(r.shells) >= maxBackgroundShells {
		r.mu.Unlock()
		obs.Warn("bgshell.start_refused", "reason", "too many shells", "limit", maxBackgroundShells, "owner", owner, "command", command)
		return nil, fmt.Errorf("too many background shells (%d) — stop one with kill_bash", maxBackgroundShells)
	}
	r.nextID++
	s := &bgShell{
		id:      fmt.Sprintf("s%d", r.nextID),
		command: command,
		dir:     dir,
		owner:   owner,
		out:     livebuf.New(bgShellOutputBufferByte),
		done:    make(chan struct{}),
		settled: make(chan struct{}),
		started: time.Now(),
	}
	s.cmd = exec.Command("sh", "-c", command)
	if dir != "" {
		s.cmd.Dir = dir
	}
	setProcGroup(s.cmd)
	s.cmd.Stdout = s.out.Writer()
	s.cmd.Stderr = s.out.Writer()
	r.shells[s.id] = s
	r.mu.Unlock()

	if err := s.cmd.Start(); err != nil {
		r.mu.Lock()
		delete(r.shells, s.id)
		r.mu.Unlock()
		obs.Warn("bgshell.start_failed", "shell", s.id, "owner", owner, "dir", dir, "command", command, "err", err.Error())
		return nil, err
	}
	obs.Info("bgshell.start", "shell", s.id, "owner", owner, "pid", s.cmd.Process.Pid, "dir", dir, "command", command)

	// Fire shell-started before the Wait goroutine exists, so listeners
	// always see started→finished in order even for instantly-exiting
	// commands (ADR-0064).
	r.fire(s.info(), true)

	go func() {
		err := s.cmd.Wait()
		s.mu.Lock()
		s.finished = true
		s.finishedAt = time.Now()
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				s.exitCode = exit.ExitCode()
			} else {
				s.exitErr = err
			}
		}
		killed, exitCode, exitErr := s.killed, s.exitCode, s.exitErr
		s.mu.Unlock()
		close(s.done)
		attrs := []any{"shell", s.id, "owner", s.owner, "exit_code", exitCode, "killed", killed,
			"ran_ms", time.Since(s.started).Milliseconds(), "command", s.command}
		if exitErr != nil {
			attrs = append(attrs, "err", exitErr.Error())
		}
		obs.Info("bgshell.exit", attrs...)
		// Queue the model-facing completion notice BEFORE firing listeners, so
		// a wake decision made from a listener already sees it pending
		// (decideFollowup's pending==0 guard would otherwise skip the wake).
		info := s.info()
		if ShellWantsNotice(info) {
			r.mu.Lock()
			r.pushDoneEventLocked(ShellEvent{Info: info, Tail: shellOutputTail(s.out.Snapshot())})
			r.mu.Unlock()
		}
		r.fire(info, false) // ADR-0064: shell-finished lifecycle event
		close(s.settled)
	}()

	// Auto-kill after the hard lifetime cap.
	go func() {
		select {
		case <-s.done:
		case <-time.After(bgShellMaxLifetime):
			s.kill("lifetime_cap", "")
		}
	}()

	return s, nil
}

// kill stops the shell's process group; a shell that already exited is left
// as-is (its output stays readable).
func (s *bgShell) kill(reason, requester string) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		obs.Debug("bgshell.kill_ignored", "shell", s.id, "why", "already exited", "reason", reason, "requester", requester)
		return
	}
	s.killed = true
	proc := s.cmd.Process
	s.mu.Unlock()
	obs.Info("bgshell.kill", "shell", s.id, "reason", reason, "requester", requester, "owner", s.owner,
		"cross_owner", requester != "" && s.owner != "" && requester != s.owner)
	if proc != nil {
		killProcGroup(s.cmd)
	}
}

// WaitShellSettled blocks until the shell's exit bookkeeping has completed —
// its completion notice is queued (or deliberately suppressed) and listeners
// have fired. Tests spawning real shells wait on this so a late event cannot
// leak past their cleanup into the shared queue.
func WaitShellSettled(id string) {
	if s, ok := bgShells.get(id); ok {
		<-s.settled
	}
}

func (r *shellRegistry) get(id string) (*bgShell, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.shells[id]
	return s, ok
}

// shellActor names who a tool call acts for: the workflow identity the
// interpreter put on the turn's ctx ("worker·Item-2"), or "" outside a board
// workflow.
func shellActor(ctx context.Context) string { return board.IdentityFrom(ctx) }

// runBashOutput implements the bash_output tool.
func runBashOutput(args map[string]any) (string, bool) {
	id, _ := args["shell_id"].(string)
	s, ok := bgShells.get(id)
	if !ok {
		return toolResult{Error: fmt.Sprintf("no background shell %q — IDs are returned by bash with run_in_background; finished shells are reaped 30 min after exit", id)}.String(), false
	}
	out := s.out.Snapshot()
	if n := intArg(args, "tail_lines"); n > 0 {
		lines := strings.Split(out, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1] // trailing newline is not a line
		}
		if len(lines) > n {
			omitted := len(lines) - n
			out = fmt.Sprintf("… (%d earlier lines omitted)\n", omitted) + strings.Join(lines[len(lines)-n:], "\n")
		}
	}
	return toolResult{Output: s.statusLine() + "\n" + out}.String(), false
}

// runKillBash implements the kill_bash tool.
func runKillBash(ctx context.Context, args map[string]any) (string, bool) {
	id, _ := args["shell_id"].(string)
	if _, ok := killShell(id, "kill_bash", shellActor(ctx)); !ok {
		obs.Warn("bgshell.kill_unknown", "shell", id, "requester", shellActor(ctx))
		return toolResult{Error: fmt.Sprintf("no background shell %q", id)}.String(), false
	}
	return toolResult{Output: fmt.Sprintf("killed background shell %s", id)}.String(), false
}
