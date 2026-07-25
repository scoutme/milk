package claude

import (
	"testing"
)

func TestParseOAuthChallenge_NotNeeded(t *testing.T) {
	cases := []string{
		"",
		"some unrelated warning message",
		"no stdin data received",
		"connection refused",
	}
	for _, tc := range cases {
		url, needed := parseOAuthChallenge(tc)
		if needed {
			t.Errorf("parseOAuthChallenge(%q): wanted needed=false, got url=%q", tc, url)
		}
	}
}

func TestParseOAuthChallenge_NeededNoURL(t *testing.T) {
	cases := []string{
		"Authorization required for MCP server",
		"Please re-authenticate with the server",
		"Authentication required before proceeding",
		"login required to access this resource",
		"Not authorized",
	}
	for _, tc := range cases {
		url, needed := parseOAuthChallenge(tc)
		if !needed {
			t.Errorf("parseOAuthChallenge(%q): wanted needed=true, got false", tc)
		}
		if url != "" {
			t.Errorf("parseOAuthChallenge(%q): wanted empty url, got %q", tc, url)
		}
	}
}

func TestParseOAuthChallenge_NeededWithURL(t *testing.T) {
	cases := []struct {
		stderr  string
		wantURL string
	}{
		{
			stderr:  "Authorization required. Visit https://auth.example.com/oauth to authenticate.",
			wantURL: "https://auth.example.com/oauth",
		},
		{
			stderr:  "OAuth required\nhttps://provider.example.com/auth?client_id=abc",
			wantURL: "https://provider.example.com/auth?client_id=abc",
		},
		{
			stderr:  "re-authenticate at https://login.example.com",
			wantURL: "https://login.example.com",
		},
	}
	for _, tc := range cases {
		url, needed := parseOAuthChallenge(tc.stderr)
		if !needed {
			t.Errorf("parseOAuthChallenge(%q): wanted needed=true, got false", tc.stderr)
		}
		if url != tc.wantURL {
			t.Errorf("parseOAuthChallenge(%q): wanted url=%q, got %q", tc.stderr, tc.wantURL, url)
		}
	}
}

func TestParseOAuthChallenge_CaseInsensitive(t *testing.T) {
	cases := []string{
		"AUTHORIZATION REQUIRED",
		"OAuth Challenge Detected",
		"RE-AUTHENTICATE please",
	}
	for _, tc := range cases {
		_, needed := parseOAuthChallenge(tc)
		if !needed {
			t.Errorf("parseOAuthChallenge(%q): wanted needed=true (case-insensitive), got false", tc)
		}
	}
}
