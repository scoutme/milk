package local

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

// --- buildCanonicalURI ---

func TestBuildCanonicalURI_Empty(t *testing.T) {
	if got := buildCanonicalURI(""); got != "/" {
		t.Errorf("want /, got %q", got)
	}
}

func TestBuildCanonicalURI_SimplePath(t *testing.T) {
	if got := buildCanonicalURI("/v1/models"); got != "/v1/models" {
		t.Errorf("want /v1/models, got %q", got)
	}
}

func TestBuildCanonicalURI_ARNSegment(t *testing.T) {
	// Model ID encoded as a single segment (colons already encoded as %3A by awsURIEncodeModel,
	// slashes as %2F). buildCanonicalURI must re-encode % → %25 so the canonical URI
	// has %253A / %252F as AWS SigV4 requires.
	encoded := awsURIEncodeModel("arn:aws:bedrock:us-east-1::application-inference-profile/abcdef")
	path := "/model/" + encoded + "/converse-stream"
	canonical := buildCanonicalURI(path)

	if canonical == path {
		t.Error("buildCanonicalURI must double-encode percent signs, but got identical path")
	}
	// % in the segment must become %25
	if !containsString(canonical, "%253A") {
		t.Errorf("expected %%253A in canonical URI, got: %s", canonical)
	}
	if !containsString(canonical, "%252F") {
		t.Errorf("expected %%252F in canonical URI, got: %s", canonical)
	}
	// Literal slashes that separate segments must be preserved
	if canonical[:7] != "/model/" {
		t.Errorf("segment separators must not be encoded, got: %s", canonical)
	}
}

// --- awsURIEncodeSegment / awsURIEncodeModel ---

func TestAWSURIEncodeSegment_UnreservedPassthrough(t *testing.T) {
	s := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.~"
	if got := awsURIEncodeSegment(s); got != s {
		t.Errorf("unreserved chars must pass through unchanged, got %q", got)
	}
}

func TestAWSURIEncodeSegment_SpecialChars(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"a:b", "a%3Ab"},
		{"a/b", "a%2Fb"},
		{"a b", "a%20b"},
	}
	for _, c := range cases {
		if got := awsURIEncodeSegment(c.in); got != c.want {
			t.Errorf("awsURIEncodeSegment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAWSURIEncodeModel_ARN(t *testing.T) {
	arn := "arn:aws:bedrock:eu-central-1:123456789012:application-inference-profile/qljnat3suz4n"
	got := awsURIEncodeModel(arn)
	// must not contain literal colons or slashes
	for _, c := range []byte(got) {
		if c == ':' || c == '/' {
			t.Errorf("encoded ARN must not contain literal : or /, got: %s", got)
		}
	}
	if got == arn {
		t.Error("ARN must be encoded, got unchanged value")
	}
}

// --- headerTransport ---

func TestHeaderTransport_InjectsHeaders(t *testing.T) {
	var gotReq *http.Request
	mock := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReq = r
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	tr := &headerTransport{
		inner:   mock,
		headers: map[string]string{"X-Custom": "value", "Authorization": "Bearer tok"},
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	tr.RoundTrip(req) //nolint:errcheck
	if gotReq.Header.Get("X-Custom") != "value" {
		t.Error("X-Custom header not injected")
	}
	if gotReq.Header.Get("Authorization") != "Bearer tok" {
		t.Error("Authorization header not injected")
	}
}

func TestHeaderTransport_DoesNotMutateOriginal(t *testing.T) {
	mock := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	tr := &headerTransport{inner: mock, headers: map[string]string{"X-Injected": "yes"}}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	tr.RoundTrip(req) //nolint:errcheck
	if req.Header.Get("X-Injected") != "" {
		t.Error("headerTransport must not mutate the original request")
	}
}

// --- sigv4Transport: smoke test that Authorization header is added ---

func TestSigV4Transport_AddsAuthorizationHeader(t *testing.T) {
	var gotReq *http.Request
	mock := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReq = r
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	tr := &sigv4Transport{
		inner:   mock,
		region:  "us-east-1",
		service: "bedrock",
		keyID:   "AKIAIOSFODNN7EXAMPLE",
		secret:  "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	req, _ := http.NewRequest(http.MethodPost, "http://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	tr.RoundTrip(req) //nolint:errcheck
	auth := gotReq.Header.Get("Authorization")
	if !containsString(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("expected AWS4-HMAC-SHA256 Authorization, got: %s", auth)
	}
	if !containsString(auth, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("Authorization must include keyID, got: %s", auth)
	}
}

func TestSigV4Transport_AddsDateAndContentSHA256(t *testing.T) {
	var gotReq *http.Request
	mock := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReq = r
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	tr := &sigv4Transport{inner: mock, region: "us-east-1", service: "bedrock", keyID: "K", secret: "S"}
	req, _ := http.NewRequest(http.MethodPost, "http://example.amazonaws.com/model/x/converse", bytes.NewReader([]byte(`{}`)))
	tr.RoundTrip(req) //nolint:errcheck
	if gotReq.Header.Get("x-amz-date") == "" {
		t.Error("x-amz-date header missing")
	}
	if gotReq.Header.Get("x-amz-content-sha256") == "" {
		t.Error("x-amz-content-sha256 header missing")
	}
}

func TestSigV4Transport_SessionTokenHeader(t *testing.T) {
	var gotReq *http.Request
	mock := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReq = r
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	tr := &sigv4Transport{inner: mock, region: "us-east-1", service: "bedrock", keyID: "K", secret: "S", token: "MYTOKEN"}
	req, _ := http.NewRequest(http.MethodPost, "http://example.amazonaws.com/model/x/converse", nil)
	tr.RoundTrip(req) //nolint:errcheck
	if gotReq.Header.Get("x-amz-security-token") != "MYTOKEN" {
		t.Error("x-amz-security-token header missing or wrong")
	}
}

// --- regionFromBedrockURL ---

func TestRegionFromBedrockURL(t *testing.T) {
	cases := []struct {
		url    string
		region string
	}{
		{"https://bedrock-runtime.eu-central-1.amazonaws.com", "eu-central-1"},
		{"https://bedrock-runtime.us-east-1.amazonaws.com/model/x", "us-east-1"},
		{"http://localhost:8080", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := regionFromBedrockURL(c.url); got != c.region {
			t.Errorf("regionFromBedrockURL(%q) = %q, want %q", c.url, got, c.region)
		}
	}
}

// --- helpers ---

func containsString(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
