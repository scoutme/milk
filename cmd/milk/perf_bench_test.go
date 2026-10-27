package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
)

// Benchmarks for the per-keystroke Update path and the per-frame View cost
// (issue: input lag when holding arrows — every KeyPressMsg runs
// updateTA+syncLayout and every frame re-renders View). These pin the cost
// so regressions are visible and optimizations measurable.

func newBenchModel(b *testing.B, transcriptLines int) model {
	b.Helper()
	b.Setenv("HOME", b.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		b.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)

	// Size the model through the real resize path.
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 56})
	m = updated.(model)

	// Realistic transcript: colored turns with tool lines, like a real session.
	var sb strings.Builder
	for i := 0; i < transcriptLines/14; i++ {
		sb.WriteString("\x1b[1;36muser\x1b[0m: please run the tests and fix the flaky one in package cmd\n\n")
		for j := 0; j < 10; j++ {
			sb.WriteString("\x1b[2m[tool]\x1b[0m read_file internal/router/scorer.go — 248 lines, 8.2 KB read into context\n")
		}
		sb.WriteString("\x1b[1;32massistant\x1b[0m: I read the scorer and the flake traces; the weighted tie-break resets weights on empty candidate sets. Patching it now.\n\n")
	}
	m.transcript.WriteString(sb.String())
	m.setViewportContent()
	m.vp.GotoBottom()

	// Panels like a real session: memory/tasks/project/git open (the user's
	// layout) — every View frame renders all of them.
	m.panelMemory = true
	m.panelTasks = true
	m.panelProject = true
	m.panelGit = true
	m.syncLayout()
	return m
}

// BenchmarkKeystroke_Left holds the cost of one arrow key: Update's
// default path (updateTA + syncLayout + full viewport rebuild).
func BenchmarkKeystroke_Left(b *testing.B) {
	for _, lines := range []int{200, 2000, 10000} {
		b.Run(fmt.Sprintf("lines=%d", lines), func(b *testing.B) {
			m := newBenchModel(b, lines)
			// Put some text in the input so cursor movement has somewhere to go.
			m.ta.SetValue("please run the tests and fix the flaky one in package cmd/x")
			b.ResetTimer()
			var mm tea.Model = m
			for i := 0; i < b.N; i++ {
				mm, _ = mm.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
			}
			_ = mm
		})
	}
}

// BenchmarkFrame_View holds the cost of one render frame (View runs after
// every Update batch and on every renderer repaint).
func BenchmarkFrame_View(b *testing.B) {
	for _, lines := range []int{200, 2000, 10000} {
		b.Run(fmt.Sprintf("lines=%d", lines), func(b *testing.B) {
			m := newBenchModel(b, lines)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.View()
			}
		})
	}
}

// BenchmarkKeystroke_Full is the real per-key cost: one Update plus the
// render frame that follows it.
func BenchmarkKeystroke_Full(b *testing.B) {
	for _, lines := range []int{200, 2000, 10000} {
		b.Run(fmt.Sprintf("lines=%d", lines), func(b *testing.B) {
			m := newBenchModel(b, lines)
			m.ta.SetValue("please run the tests and fix the flaky one in package cmd/x")
			b.ResetTimer()
			var mm tea.Model = m
			for i := 0; i < b.N; i++ {
				mm, _ = mm.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
				_ = mm.(model).View()
			}
		})
	}
}
