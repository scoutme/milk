// Package testutil gives test binaries a systemic state-root sandbox (issue
// #404): per-test t.Setenv("HOME") guards are convention, not a property of
// the suite — the session-file pollution in ~/.milk/sessions proved they
// leak. TestMain-level redirection fixes every test in a package at once,
// and the canary turns any future real-root write into a loud failure
// instead of silent pollution.
package testutil

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// realConfigPath resolves the config file the way config.Dir does — from the
// CURRENT environment — for the caller to capture BEFORE SandboxStateRoot
// redirects it.
func realConfigPath() string {
	if r := strings.TrimSpace(os.Getenv("MILK_CONFIG_ROOT")); r != "" {
		return filepath.Join(r, "config.json")
	}
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		home = strings.TrimSpace(os.Getenv("USERPROFILE"))
	}
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".milk", "config.json")
}

// SandboxStateRoot redirects the milk state root (HOME, USERPROFILE and
// MILK_CONFIG_ROOT) to a fresh temp dir and returns a canary that verifies
// the REAL config.json from before the redirect is byte-identical (or still
// absent). Call it at the top of TestMain and run the canary after m.Run(),
// exiting non-zero when it reports a violation:
//
//	func TestMain(m *testing.M) {
//		canary := testutil.SandboxStateRoot()
//		code := m.Run()
//		if err := canary(); err != nil {
//			fmt.Fprintln(os.Stderr, "FATAL:", err)
//			os.Exit(98)
//		}
//		os.Exit(code)
//	}
//
// The redirect is process-wide and permanent for the test binary — tests
// that t.Setenv their own HOME still get their per-test sandbox (it restores
// to this one), and child processes (the milk binary, tmux launches) inherit
// the sandbox root too.
func SandboxStateRoot() (canary func() error) {
	canaryPath := realConfigPath()
	before, err := os.ReadFile(canaryPath)
	if err != nil {
		before = nil // absent (or unreadable) counts as "no config"
	}
	tmp, err := os.MkdirTemp("", "milk-test-state-")
	if err != nil {
		panic("testutil: cannot create sandbox state root: " + err.Error())
	}
	_ = os.Setenv("HOME", tmp)
	_ = os.Setenv("USERPROFILE", tmp)
	// Empty = config.Dir falls through to HOME (its env check trims first).
	_ = os.Setenv("MILK_CONFIG_ROOT", "")

	return func() error {
		if canaryPath == "" {
			return nil
		}
		after, err := os.ReadFile(canaryPath)
		switch {
		case os.IsNotExist(err):
			if before == nil {
				return nil
			}
			return fmt.Errorf("test canary: real config %s was DELETED during the test run", canaryPath)
		case err != nil:
			return fmt.Errorf("test canary: reading %s: %w", canaryPath, err)
		}
		if before == nil {
			return fmt.Errorf("test canary: real config %s was CREATED during the test run (%d bytes) — a test wrote the real state root", canaryPath, len(after))
		}
		if !bytes.Equal(before, after) {
			return fmt.Errorf("test canary: real config %s CHANGED during the test run (%d -> %d bytes) — a test wrote the real state root", canaryPath, len(before), len(after))
		}
		return nil
	}
}
