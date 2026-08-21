package mcpauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ClientRegistration is the subset of an RFC 7591 dynamic client registration
// response that milk needs.
type ClientRegistration struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
}

type dcrRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name"`
	Scope                   string   `json:"scope,omitempty"`
}

type dcrErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// RegisterClient dynamically registers milk as an OAuth client (RFC 7591)
// against registrationEndpoint, as a public client using PKCE (no client
// secret expected for the authorization_code grant).
func RegisterClient(ctx context.Context, registrationEndpoint, redirectURI string, scopes []string) (*ClientRegistration, error) {
	reqBody := dcrRequest{
		RedirectURIs:            []string{redirectURI},
		TokenEndpointAuthMethod: "none",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		ClientName:              "milk",
	}
	if len(scopes) > 0 {
		reqBody.Scope = strings.Join(scopes, " ")
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var dcrErr dcrErrorResponse
		if json.Unmarshal(respBody, &dcrErr) == nil && dcrErr.Error != "" {
			return nil, fmt.Errorf("dynamic client registration: %s: %s", dcrErr.Error, dcrErr.ErrorDescription)
		}
		return nil, fmt.Errorf("dynamic client registration: HTTP %d", resp.StatusCode)
	}

	var reg ClientRegistration
	if err := json.Unmarshal(respBody, &reg); err != nil {
		return nil, fmt.Errorf("dynamic client registration: invalid response: %w", err)
	}
	if reg.ClientID == "" {
		return nil, fmt.Errorf("dynamic client registration: response missing client_id")
	}
	return &reg, nil
}
