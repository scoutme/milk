package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/livebuf"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/tasks"
	"github.com/scoutme/milk/internal/transport/acp"
)

// notifyPlan publishes one plan (tasks, a workflow run, …) to the client.
// v2 clients get a plan_update per planId. v1 has a single plan per session
// and replaces it wholesale, so a v1 client gets every plan's entries
// concatenated, in first-seen order, so a workflow run doesn't erase the task
// list (or vice versa).
func (as *acpSession) notifyPlan(id acp.PlanID, entries []acp.PlanEntry) {
	as.planMu.Lock()
	defer as.planMu.Unlock()
	if as.plans == nil {
		as.plans = map[acp.PlanID][]acp.PlanEntry{}
	}
	if _, seen := as.plans[id]; !seen {
		as.planOrder = append(as.planOrder, id)
	}
	as.plans[id] = entries
	if !as.v1Client {
		as.notify(acp.NewPlanUpdate(id, entries))
		return
	}
	var merged []acp.PlanEntry
	for _, pid := range as.planOrder {
		merged = append(merged, as.plans[pid]...)
	}
	as.notify(acp.NewPlanUpdate(id, merged).AsV1())
}

// liveStreamInterval throttles streamLive. v1 clients get the full content on
// every send, so this also bounds how much they re-receive.
const liveStreamInterval = 400 * time.Millisecond

// streamLive forwards buf's growth into the tool-call row callID: an append
// (tool_call_content_chunk) for v2 clients, a full-content replace for v1.
// The returned stop function does a last flush and must be called before the
// row's final status update, so the output always precedes "completed".
func (as *acpSession) streamLive(callID acp.ToolCallID, buf *livebuf.Buffer) (stop func()) {
	var cursor acp.BufCursor
	poll := func() {
		var updates []acp.SessionUpdate
		if as.v1Client {
			updates = acp.ToolCallContentReplace(callID, buf, &cursor)
		} else {
			updates = acp.ToolCallContentPoll(callID, buf, &cursor)
		}
		for _, u := range updates {
			as.notify(u)
		}
	}
	quit, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		tick := time.NewTicker(liveStreamInterval)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				poll()
			case <-quit:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(quit)
			<-exited
			poll()
		})
	}
}

// --- tasks -------------------------------------------------------------

func (as *acpSession) setupTasks() {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	ts, err := tasks.New(filepath.Join(dir, "tasks"), as.sess.ID)
	if err != nil {
		return
	}
	as.taskStore = ts
	ts.SetOnChange(as.emitTasksPlan)
}

func (as *acpSession) emitTasksPlan() {
	list, err := as.taskStore.List(tasks.ListOpts{})
	if err != nil {
		return
	}
	as.notifyPlan(acp.PlanIDForTasks(as.sess.ID), acp.PlanEntriesForTasks(list))
}

// --- background agents -------------------------------------------------

// setupBackground creates the session's background-job manager (ADR-0043).
// Its base context is the process's, not any turn's, so jobs outlive the
// turn that spawned them; results reach the model on the next prompt via
// drainBackgroundJobs, and the client is told as each job starts and ends.
func (as *acpSession) setupBackground(hasLocalAgent bool) {
	if !hasLocalAgent {
		return
	}
	cfg := as.cfg
	mgr := local.NewManager(context.Background(), cfg.EffectiveMaxBackgroundAgents())
	mgr.SetJobTimeout(cfg.EffectiveBackgroundAgentTimeout())
	if dir, err := config.Dir(); err == nil && as.sess.ID != "" {
		mgr.SetStateFile(dir + "/jobs/" + as.sess.ID + ".json")
	}
	var stopsMu sync.Mutex
	stops := map[string]func(){}
	mgr.SetOnStart(func(j *local.Job) {
		as.notify(acp.BackgroundJobCall(*j))
		stop := as.streamLive(acp.JobToolCallID(j.ID), j.Live)
		stopsMu.Lock()
		stops[j.ID] = stop
		stopsMu.Unlock()
	})
	mgr.SetOnDone(func(j *local.Job) {
		job := *j
		stopsMu.Lock()
		stop := stops[job.ID]
		delete(stops, job.ID)
		stopsMu.Unlock()
		if stop != nil {
			stop() // flush output before the terminal status row
		}
		as.notify(acp.BackgroundJobCall(job))
		// User-started jobs have no "wave" to consolidate: follow up as soon
		// as the session is idle. Agent-started waves follow up from
		// SetOnBatchDone, once every outstanding job has finished.
		if job.Role == "user" {
			as.requestFollowup(false)
		}
	})
	mgr.SetOnBatchDone(func() { as.requestFollowup(true) })
	as.mgr = mgr
}

// spawnUserJob starts a background job on the user's behalf (/bg start),
// preferring the escalation agent's local backend like the TUI does.
func (as *acpSession) spawnUserJob(task string) string {
	agent := as.escLocalAgent
	ac := as.cfg.EscalationAgentConfig()
	if agent == nil {
		agent = as.localAgent
		ac = as.cfg.ActiveAgent()
	}
	if as.mgr == nil || agent == nil {
		return "background agents unavailable — no inference-server-backed agent configured"
	}
	modelName := ac.Model
	if modelName == "" {
		modelName = ac.Name
	}
	label := task
	if len(label) > 60 {
		label = label[:57] + "..."
	}
	cwd, mem := as.st.cwd, as.mem
	job := as.mgr.Spawn(label, task, "user", modelName, func(ctx context.Context, jobID string, out io.Writer) (string, session.TokenUsage, error) {
		return agent.RunBackgroundTask(ctx, jobID, cwd, task, "", out, mem)
	})
	return fmt.Sprintf("background agent %s started: %s", job.ID, label)
}

func acpBg(as *acpSession, rest string) (string, string) {
	if as.mgr == nil {
		return "background agents unavailable — no inference-server-backed agent configured", ""
	}
	arg := strings.TrimSpace(rest)
	parts := strings.Fields(arg)
	switch {
	case len(parts) == 0 || parts[0] == "list":
		return renderBgList(as.mgr.Jobs()), ""
	case parts[0] == "start":
		task := strings.TrimSpace(strings.TrimPrefix(arg, "start"))
		if task == "" {
			return "usage: /bg start <task>", ""
		}
		return as.spawnUserJob(task), ""
	case parts[0] == "show":
		id := strings.TrimSpace(strings.TrimPrefix(arg, "show"))
		if id == "" {
			return "usage: /bg show <id>", ""
		}
		for _, j := range as.mgr.Jobs() {
			if j.ID == id {
				return renderBgShow(j), ""
			}
		}
		return fmt.Sprintf("background agent %s not found", id), ""
	case parts[0] == "stop":
		id := strings.TrimSpace(strings.TrimPrefix(arg, "stop"))
		if id == "" {
			return "usage: /bg stop <id>", ""
		}
		if as.mgr.Cancel(id) {
			return fmt.Sprintf("background agent %s — cancelling", id), ""
		}
		return fmt.Sprintf("background agent %s not found or already finished", id), ""
	}
	return "usage: /bg [list | show <id> | start <task> | stop <id>]", ""
}

func acpTasks(as *acpSession, _ string) (string, string) { return execTasks(as.taskStore), "" }

func acpTask(as *acpSession, rest string) (string, string) {
	return execTask(strings.TrimSpace(rest), as.taskStore), ""
}
