package local

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// tokenCmdTransport runs a shell command to obtain a Bearer token, caches it,
// and re-runs the command when the server returns a 401/403. This supports
// providers that use short-lived tokens refreshed via a CLI command
// (e.g. "gh auth token --hostname org.ghe.com").
type tokenCmdTransport struct {
	inner http.RoundTripper
	cmd   string // shell command whose stdout is the token

	mu    sync.Mutex
	token string
}

func (t *tokenCmdTransport) fetchToken() (string, error) {
	out, err := exec.Command("sh", "-c", t.cmd).Output()
	if err != nil {
		return "", fmt.Errorf("token_cmd: %w", err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("token_cmd: empty output")
	}
	return tok, nil
}

func (t *tokenCmdTransport) getToken() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" {
		return t.token, nil
	}
	tok, err := t.fetchToken()
	if err != nil {
		return "", err
	}
	t.token = tok
	return tok, nil
}

func (t *tokenCmdTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.getToken()
	if err != nil {
		return nil, err
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := t.inner.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Token may have expired — refresh and retry once.
		t.mu.Lock()
		t.token = ""
		t.mu.Unlock()
		newTok, ferr := t.getToken()
		if ferr != nil {
			return resp, nil // return original response; caller sees the 401/403
		}
		resp.Body.Close()
		r2 := req.Clone(req.Context())
		r2.Header.Set("Authorization", "Bearer "+newTok)
		return t.inner.RoundTrip(r2)
	}
	return resp, nil
}

// headerTransport injects static HTTP headers on every request.
// Wraps an inner RoundTripper (usually http.DefaultTransport).
type headerTransport struct {
	inner   http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone to avoid mutating the original
	r := req.Clone(req.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return t.inner.RoundTrip(r)
}

// sigv4Transport signs each request with AWS Signature Version 4 before sending.
type sigv4Transport struct {
	inner   http.RoundTripper
	region  string
	service string
	keyID   string
	secret  string
	token   string // optional session token
}

func (t *sigv4Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())

	// Buffer the body so we can hash it
	var bodyBytes []byte
	if r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	payloadHash := hashSHA256(bodyBytes)

	now := time.Now().UTC()
	dateTime := now.Format("20060102T150405Z")
	date := now.Format("20060102")

	r.Header.Set("x-amz-date", dateTime)
	r.Header.Set("x-amz-content-sha256", payloadHash)
	if t.token != "" {
		r.Header.Set("x-amz-security-token", t.token)
	}
	r.Header.Set("host", r.URL.Host)

	// Canonical request
	// AWS SigV4 requires each path segment to be URI-encoded independently.
	// Use r.URL.EscapedPath() to get the single-encoded path (as sent on the wire),
	// then re-encode each segment so special chars like %3A become %253A.
	signedHeaders, canonicalHeaders := buildCanonicalHeaders(r)
	canonicalURI := buildCanonicalURI(r.URL.EscapedPath())
	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURI,
		r.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	// String to sign
	credScope := date + "/" + t.region + "/" + t.service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + dateTime + "\n" + credScope + "\n" + hexSHA256([]byte(canonicalRequest))

	// Signing key
	signingKey := deriveSigningKey(t.secret, date, t.region, t.service)
	signature := hmacHex(signingKey, stringToSign)

	r.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		t.keyID, credScope, signedHeaders, signature,
	))

	return t.inner.RoundTrip(r)
}

// buildCanonicalURI constructs the canonical URI for SigV4 by re-encoding each
// path segment. The input is the already-escaped path from url.URL.EscapedPath().
// Re-encoding makes any percent-encoded characters (e.g. %3A) become %253A, which
// matches what AWS expects for services like Bedrock when the model ID is an ARN.
func buildCanonicalURI(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	segments := strings.Split(escapedPath, "/")
	for i, seg := range segments {
		segments[i] = awsURIEncodeSegment(seg)
	}
	return strings.Join(segments, "/")
}

// awsURIEncodeSegment percent-encodes a single path segment, encoding all
// characters except unreserved ones (A-Z a-z 0-9 - _ . ~).
func awsURIEncodeSegment(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// awsURIEncodeModel encodes a model ID (plain ID or full ARN) for use as a single
// path segment in a Bedrock URL. All characters except unreserved ones are encoded,
// including forward slashes (/ → %2F) and colons (: → %3A), so that an ARN like
// "arn:aws:bedrock:...:application-inference-profile/id" becomes a single opaque
// path segment rather than splitting into multiple segments. The SigV4 transport
// then double-encodes this segment in the canonical URI (e.g. %2F → %252F).
func awsURIEncodeModel(s string) string {
	return awsURIEncodeSegment(s)
}

func buildCanonicalHeaders(req *http.Request) (signedHeaders, canonicalHeaders string) {
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		pairs = append(pairs, kv{lk, strings.Join(vs, ",")})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].k < pairs[j].k })

	var hdrBuf strings.Builder
	var keyBuf strings.Builder
	for i, p := range pairs {
		hdrBuf.WriteString(p.k + ":" + strings.TrimSpace(p.v) + "\n")
		if i > 0 {
			keyBuf.WriteByte(';')
		}
		keyBuf.WriteString(p.k)
	}
	return keyBuf.String(), hdrBuf.String()
}

func hashSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}

func hexSHA256(data []byte) string {
	return hashSHA256(data)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hmacHex(key []byte, data string) string {
	return fmt.Sprintf("%x", hmacSHA256(key, data))
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}
