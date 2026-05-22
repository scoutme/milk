package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// scriptPath writes a shell script to a tempdir and returns its path.
func scriptPath(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "creds.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveAWSCreds_Empty(t *testing.T) {
	creds, err := ResolveAWSCreds("")
	if err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
	if creds != nil {
		t.Fatalf("want nil creds, got %+v", creds)
	}
}

func TestResolveAWSCreds_Valid(t *testing.T) {
	script := scriptPath(t, `printf '{"Version":1,"AccessKeyId":"AKID","SecretAccessKey":"SECRET","SessionToken":"TOKEN"}'`)
	creds, err := ResolveAWSCreds(script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds == nil {
		t.Fatal("want non-nil creds, got nil")
	}
	if creds.AccessKeyID != "AKID" {
		t.Errorf("AccessKeyID: want AKID, got %q", creds.AccessKeyID)
	}
	if creds.SecretAccessKey != "SECRET" {
		t.Errorf("SecretAccessKey: want SECRET, got %q", creds.SecretAccessKey)
	}
	if creds.SessionToken != "TOKEN" {
		t.Errorf("SessionToken: want TOKEN, got %q", creds.SessionToken)
	}
}

func TestResolveAWSCreds_InvalidJSON(t *testing.T) {
	script := scriptPath(t, `printf 'not valid json'`)
	_, err := ResolveAWSCreds(script)
	if err == nil {
		t.Fatal("want error for invalid JSON, got nil")
	}
}

func TestResolveAWSCreds_MissingKey(t *testing.T) {
	script := scriptPath(t, `printf '{"Version":1}'`)
	_, err := ResolveAWSCreds(script)
	if err == nil {
		t.Fatal("want error when AccessKeyId is missing, got nil")
	}
}

func TestResolveAWSCreds_CommandFails(t *testing.T) {
	_, err := ResolveAWSCreds("/nonexistent/does/not/exist")
	if err == nil {
		t.Fatal("want error for nonexistent command, got nil")
	}
}

func TestAWSCreds_Env_WithToken(t *testing.T) {
	c := &AWSCreds{
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
		SessionToken:    "TOKEN",
	}
	env := c.Env()
	want := []string{
		"AWS_ACCESS_KEY_ID=AKID",
		"AWS_SECRET_ACCESS_KEY=SECRET",
		"AWS_SESSION_TOKEN=TOKEN",
	}
	if len(env) != len(want) {
		t.Fatalf("want %d vars, got %d: %v", len(want), len(env), env)
	}
	for i, v := range want {
		if env[i] != v {
			t.Errorf("env[%d]: want %q, got %q", i, v, env[i])
		}
	}
}

func TestAWSCreds_Env_NoToken(t *testing.T) {
	c := &AWSCreds{
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
		SessionToken:    "",
	}
	env := c.Env()
	want := []string{
		"AWS_ACCESS_KEY_ID=AKID",
		"AWS_SECRET_ACCESS_KEY=SECRET",
	}
	if len(env) != len(want) {
		t.Fatalf("want %d vars, got %d: %v", len(want), len(env), env)
	}
	for i, v := range want {
		if env[i] != v {
			t.Errorf("env[%d]: want %q, got %q", i, v, env[i])
		}
	}
	for _, e := range env {
		if e == "AWS_SESSION_TOKEN=" || len(e) > 18 && e[:18] == "AWS_SESSION_TOKEN=" {
			t.Errorf("AWS_SESSION_TOKEN should be absent, found %q", e)
		}
	}
}
