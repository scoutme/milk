// Package mcpauth implements native OAuth 2.0 support for MCP servers:
// RFC 9728 protected-resource discovery, RFC 8414 authorization-server
// discovery, RFC 7591 dynamic client registration, and Authorization Code +
// PKCE (RFC 7636). It has no bubbletea/TUI dependency — cmd/milk supplies
// UI glue (status bar, transcript, browser-open trigger) via Options hooks.
package mcpauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// GenerateVerifier returns a PKCE code verifier: 32 random bytes, base64url
// (no padding) encoded, giving a 43-character string per RFC 7636 (43-128
// chars, unreserved charset).
func GenerateVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Challenge computes the S256 PKCE code challenge for a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// RandomState returns a random CSRF state token for the authorization request.
func RandomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
