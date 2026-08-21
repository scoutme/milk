package mcpauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	TokenType        string `json:"token_type,omitempty"`
	ExpiresIn        int64  `json:"expires_in,omitempty"`
	Scope            string `json:"scope,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

func postForm(ctx context.Context, tokenEndpoint string, form url.Values) (*tokenResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("token endpoint: invalid response (HTTP %d): %w", resp.StatusCode, err)
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("token endpoint: %s: %s", tr.Error, tr.ErrorDescription)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token endpoint: HTTP %d", resp.StatusCode)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint: response missing access_token")
	}
	return &tr, nil
}

func (tr *tokenResponse) toTokenSet() *TokenSet {
	ts := &TokenSet{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
	}
	if tr.ExpiresIn > 0 {
		ts.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return ts
}

// ExchangeCode performs the authorization_code grant (PKCE, no client secret
// required for public clients) against tokenEndpoint.
func ExchangeCode(ctx context.Context, tokenEndpoint, code, verifier, clientID, clientSecret, redirectURI string) (*TokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	tr, err := postForm(ctx, tokenEndpoint, form)
	if err != nil {
		return nil, err
	}
	return tr.toTokenSet(), nil
}

// RefreshAccessToken performs the refresh_token grant against tokenEndpoint.
func RefreshAccessToken(ctx context.Context, tokenEndpoint, refreshToken, clientID, clientSecret string) (*TokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	tr, err := postForm(ctx, tokenEndpoint, form)
	if err != nil {
		return nil, err
	}
	return tr.toTokenSet(), nil
}
