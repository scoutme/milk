package mcpauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/config"
)

// TokenSet is the persisted OAuth state for one MCP server.
type TokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	Scope        string    `json:"scope,omitempty"`

	// Cached discovery + registration results so re-auth/refresh doesn't need
	// to re-run discovery/DCR every time.
	Issuer                string `json:"issuer,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `json:"token_endpoint,omitempty"`
	RegistrationEndpoint  string `json:"registration_endpoint,omitempty"`
	ClientID              string `json:"client_id,omitempty"`
	ClientSecret          string `json:"client_secret,omitempty"`
}

// storeDir returns ~/.milk/mcp_oauth, creating it if necessary.
func storeDir() (string, error) {
	dir, err := config.MCPOAuthDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// tokenFileName sanitizes a server name for safe use as a filename.
func tokenFileName(serverName string) string {
	var sb strings.Builder
	for _, r := range serverName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String() + ".json"
}

// LoadToken returns the stored TokenSet for serverName, or (nil, nil) if none
// has been persisted yet.
func LoadToken(serverName string) (*TokenSet, error) {
	dir, err := storeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, tokenFileName(serverName))
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ts TokenSet
	if err := json.Unmarshal(data, &ts); err != nil {
		return nil, err
	}
	return &ts, nil
}

// SaveToken atomically persists ts for serverName (tmp file + rename, 0600).
func SaveToken(serverName string, ts *TokenSet) error {
	dir, err := storeDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, tokenFileName(serverName))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DeleteToken removes the stored TokenSet for serverName, if any.
func DeleteToken(serverName string) error {
	dir, err := storeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, tokenFileName(serverName))
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
