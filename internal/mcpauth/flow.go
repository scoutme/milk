package mcpauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/config"
)

// defaultAuthTimeout bounds how long Authorize waits for the browser
// callback when cfg.AuthTimeout is unset.
const defaultAuthTimeout = 5 * time.Minute

// Options configures an Authorize call. All fields are optional.
type Options struct {
	// OnAuthURL is called once the authorization URL is built, before the
	// caller blocks waiting for the browser redirect. Callers use this to
	// print the URL (for headless/SSH use) and update UI state.
	OnAuthURL func(authURL string)

	// Opener opens authURL in a browser. Defaults to OpenURL. Errors are
	// non-fatal — Authorize still waits for the callback, since the URL was
	// also handed to OnAuthURL for manual use.
	Opener func(authURL string) error

	// Timeout bounds how long Authorize waits for the browser callback.
	// Defaults to defaultAuthTimeout.
	Timeout time.Duration
}

// Authorize runs the full RFC 9728 -> RFC 8414 -> RFC 7591 -> PKCE
// Authorization Code flow for one MCP server and persists the resulting
// tokens via SaveToken. It blocks until the flow completes, fails, times
// out, or ctx is cancelled.
func Authorize(ctx context.Context, cfg config.MCPServerConfig, opts Options) error {
	if opts.Opener == nil {
		opts.Opener = OpenURL
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = parseAuthTimeout(cfg.AuthTimeout)
	}

	asMeta, err := DiscoverAuthServer(ctx, cfg.URL)
	if err != nil {
		return fmt.Errorf("discover authorization server: %w", err)
	}
	if asMeta.AuthorizationEndpoint == "" || asMeta.TokenEndpoint == "" {
		return fmt.Errorf("authorization server metadata for %q is missing required endpoints", cfg.Name)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start local callback listener: %w", err)
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	clientID, clientSecret := cfg.ClientID, cfg.ClientSecret
	regEndpointUsed := ""
	if clientID == "" {
		if asMeta.RegistrationEndpoint == "" {
			ln.Close()
			return fmt.Errorf("server %q has no dynamic client registration support and no oauth_client_id is configured", cfg.Name)
		}
		reg, err := RegisterClient(ctx, asMeta.RegistrationEndpoint, redirectURI, cfg.Scopes)
		if err != nil {
			ln.Close()
			return err
		}
		clientID, clientSecret = reg.ClientID, reg.ClientSecret
		regEndpointUsed = asMeta.RegistrationEndpoint
	}

	verifier, err := GenerateVerifier()
	if err != nil {
		ln.Close()
		return err
	}
	state, err := RandomState()
	if err != nil {
		ln.Close()
		return err
	}

	authURL := buildAuthURL(asMeta.AuthorizationEndpoint, clientID, redirectURI, Challenge(verifier), state, cfg.Scopes)
	if opts.OnAuthURL != nil {
		opts.OnAuthURL(authURL)
	}
	go func() { _ = opts.Opener(authURL) }()

	code, err := waitForCallback(ctx, ln, state, timeout)
	if err != nil {
		return err
	}

	ts, err := ExchangeCode(ctx, asMeta.TokenEndpoint, code, verifier, clientID, clientSecret, redirectURI)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	ts.Issuer = asMeta.Issuer
	ts.AuthorizationEndpoint = asMeta.AuthorizationEndpoint
	ts.TokenEndpoint = asMeta.TokenEndpoint
	ts.RegistrationEndpoint = regEndpointUsed
	ts.ClientID = clientID
	ts.ClientSecret = clientSecret

	return SaveToken(cfg.Name, ts)
}

func parseAuthTimeout(s string) time.Duration {
	if s == "" {
		return defaultAuthTimeout
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return defaultAuthTimeout
}

func buildAuthURL(authEndpoint, clientID, redirectURI, challenge, state string, scopes []string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	sep := "?"
	if hasQuery(authEndpoint) {
		sep = "&"
	}
	return authEndpoint + sep + q.Encode()
}

func hasQuery(u string) bool {
	parsed, err := url.Parse(u)
	return err == nil && parsed.RawQuery != ""
}

// callbackResult carries the outcome of the single /callback request.
type callbackResult struct {
	code string
	err  error
}

// waitForCallback serves a single-shot HTTP handler on ln's /callback path,
// validates state, and returns the authorization code. It always closes ln
// (via the server) before returning.
func waitForCallback(ctx context.Context, ln net.Listener, state string, timeout time.Duration) (string, error) {
	resultCh := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}

	send := func(r callbackResult) {
		select {
		case resultCh <- r:
		default:
		}
	}

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errParam := q.Get("error"); errParam != "" {
			writeCallbackPage(w, false)
			send(callbackResult{err: fmt.Errorf("authorization denied: %s", errParam)})
			return
		}
		if q.Get("state") != state {
			writeCallbackPage(w, false)
			send(callbackResult{err: fmt.Errorf("state mismatch in OAuth callback (possible CSRF)")})
			return
		}
		code := q.Get("code")
		if code == "" {
			writeCallbackPage(w, false)
			send(callbackResult{err: fmt.Errorf("no authorization code in OAuth callback")})
			return
		}
		writeCallbackPage(w, true)
		send(callbackResult{code: code})
	})

	go func() { _ = srv.Serve(ln) }()
	defer srv.Close() //nolint:errcheck

	select {
	case res := <-resultCh:
		if res.err != nil {
			return "", res.err
		}
		return res.code, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out after %s waiting for browser authorization", timeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func writeCallbackPage(w http.ResponseWriter, ok bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if ok {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "<html><body><p>Authorization complete — you can close this tab and return to milk.</p></body></html>")
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprint(w, "<html><body><p>Authorization failed — return to milk for details.</p></body></html>")
}
