package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/session"
)

func TestDrainBackgroundJobs_NilManager(t *testing.T) {
	sess := &session.Session{}
	if got := drainBackgroundJobs(context.Background(), nil, sess); got != "" {
		t.Errorf("expected empty string for nil manager, got %q", got)
	}
}

func TestDrainBackgroundJobs_NothingCompleted(t *testing.T) {
	mgr := local.NewManager(context.Background(), 3)
	sess := &session.Session{}
	if got := drainBackgroundJobs(context.Background(), mgr, sess); got != "" {
		t.Errorf("expected empty string when nothing has completed, got %q", got)
	}
}

func TestDrainBackgroundJobs_FormatsCompletedAndFailed_RecordsTokens(t *testing.T) {
	mgr := local.NewManager(context.Background(), 3)
	var wg sync.WaitGroup
	wg.Add(2)
	mgr.SetOnDone(func(j *local.Job) { wg.Done() })

	mgr.Spawn("investigate X", "task1", "primary", "test-model", func(ctx context.Context) (string, session.TokenUsage, error) {
		return "found the bug in foo.go", session.TokenUsage{Prompt: 100, Completion: 20}, nil
	})
	mgr.Spawn("investigate Y", "task2", "escalation", "test-model", func(ctx context.Context) (string, session.TokenUsage, error) {
		return "", session.TokenUsage{}, errors.New("boom")
	})
	wg.Wait()

	sess := &session.Session{}
	got := drainBackgroundJobs(context.Background(), mgr, sess)

	if !strings.Contains(got, `background agent "investigate X" completed: found the bug in foo.go`) {
		t.Errorf("expected completed-job text in output, got %q", got)
	}
	if !strings.Contains(got, `background agent "investigate Y" failed: boom`) {
		t.Errorf("expected failed-job text in output, got %q", got)
	}

	usage := sess.Tokens["test-model\x00primary:subagent"]
	if usage == nil || usage.Prompt != 100 || usage.Completion != 20 {
		t.Errorf("expected primary:subagent token usage to be recorded, got %+v", usage)
	}

	// Draining again must not re-emit or re-record anything.
	second := drainBackgroundJobs(context.Background(), mgr, sess)
	if second != "" {
		t.Errorf("expected empty string on second drain, got %q", second)
	}
}
