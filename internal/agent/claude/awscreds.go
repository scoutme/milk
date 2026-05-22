package claude

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// AWSCreds holds temporary AWS credentials returned by a credential_process command.
type AWSCreds struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
}

// ResolveAWSCreds runs the given credential_process command and parses its JSON output.
// Returns nil if cmd is empty.
func ResolveAWSCreds(cmd string) (*AWSCreds, error) {
	if cmd == "" {
		return nil, nil
	}
	parts := strings.Fields(cmd)
	out, err := exec.Command(parts[0], parts[1:]...).Output()
	if err != nil {
		return nil, fmt.Errorf("aws_auth_refresh command failed: %w", err)
	}
	var creds AWSCreds
	if err := json.Unmarshal(out, &creds); err != nil {
		return nil, fmt.Errorf("aws_auth_refresh: invalid JSON output: %w", err)
	}
	if creds.AccessKeyID == "" {
		return nil, fmt.Errorf("aws_auth_refresh: AccessKeyId missing from output")
	}
	return &creds, nil
}

// Env returns the credentials as AWS_* environment variable strings.
func (c *AWSCreds) Env() []string {
	env := []string{
		"AWS_ACCESS_KEY_ID=" + c.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + c.SecretAccessKey,
	}
	if c.SessionToken != "" {
		env = append(env, "AWS_SESSION_TOKEN="+c.SessionToken)
	}
	return env
}
