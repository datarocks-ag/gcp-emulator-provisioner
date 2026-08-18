package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTokenEndpointAnswersAGrantExchange(t *testing.T) {
	srv := httptest.NewServer(newHandler("local-emulator-token", 3600))
	defer srv.Close()

	// What a Google client library actually sends: a signed JWT exchanged for
	// an access token.
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {"eyJhbGciOiJSUzI1NiJ9.fake.signature"},
	}
	resp, err := http.Post(srv.URL+"/token", "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("POST /token: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}

	var body tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.AccessToken != "local-emulator-token" || body.TokenType != "Bearer" || body.ExpiresIn != 3600 {
		t.Errorf("response = %+v", body)
	}
}

// The assertion is deliberately unverified — nothing downstream checks
// signatures either, so a garbage assertion must still be answered.
func TestTokenEndpointDoesNotVerifyTheAssertion(t *testing.T) {
	srv := httptest.NewServer(newHandler("t", 60))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/token", "application/x-www-form-urlencoded",
		strings.NewReader("assertion=not-even-a-jwt"))
	if err != nil {
		t.Fatalf("POST /token: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — the stub must not validate", resp.StatusCode)
	}
}

// The container healthcheck probes with GET rather than forging a grant.
func TestTokenEndpointAnswersGetForHealthchecks(t *testing.T) {
	srv := httptest.NewServer(newHandler("t", 60))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/token")
	if err != nil {
		t.Fatalf("GET /token: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestUnknownPathsAre404JSON(t *testing.T) {
	srv := httptest.NewServer(newHandler("t", 60))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/somewhere-else")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("404 body is not JSON: %v", err)
	}
	if body["error"] != "not_found" {
		t.Errorf("body = %v", body)
	}
}

func TestTokenEndpointRejectsOtherMethods(t *testing.T) {
	srv := httptest.NewServer(newHandler("t", 60))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/token", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /token: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestRunShutsDownOnSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, "127.0.0.1:0", "t", 60) }()

	// Cancelling stands in for SIGTERM from `docker compose down`.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v, want a clean shutdown", err)
		}
	case <-time.After(shutdownGrace + time.Second):
		t.Error("run did not shut down after the context was cancelled")
	}
}

func TestRunReportsAnUnusableAddress(t *testing.T) {
	err := run(context.Background(), "127.0.0.1:-1", "t", 60)
	if err == nil {
		t.Error("expected an invalid listen address to fail")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "fallback"); got != "fallback" {
		t.Errorf("got %q", got)
	}
	if got := firstNonEmpty("flag", "env", "default"); got != "flag" {
		t.Errorf("flag must win, got %q", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestProbeSucceedsAgainstARunningStub(t *testing.T) {
	srv := httptest.NewServer(newHandler("t", 60))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := probe(addr); err != nil {
		t.Errorf("probe against a live stub: %v", err)
	}
}

func TestProbeFailsWhenNothingIsListening(t *testing.T) {
	// Bind and immediately release, so the port is almost certainly free.
	srv := httptest.NewServer(newHandler("t", 60))
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	if err := probe(addr); err == nil {
		t.Error("probe must fail when the stub is down, or the healthcheck is useless")
	}
}

func TestProbeFailsOnANonOKStatus(t *testing.T) {
	// Any path other than /token 404s, which stands in for a broken stub.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := probe(strings.TrimPrefix(srv.URL, "http://")); err == nil {
		t.Error("probe must fail on a non-200 response")
	}
}

func TestProbeHandlesABareColonAddress(t *testing.T) {
	// ":8099" is a valid listen address but not a valid URL host, so probe has
	// to fill in a host before dialling. Nothing listens here; the point is that
	// it produces a dial error rather than a URL parse error.
	err := probe(":65534")
	if err == nil {
		t.Fatal("expected a dial failure")
	}
	if strings.Contains(err.Error(), "missing protocol scheme") ||
		strings.Contains(err.Error(), "invalid URI") {
		t.Errorf("bare :port was not normalised into a URL host: %v", err)
	}
}

func TestSetupLogging(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", "nonsense", ""} {
		t.Setenv("LOG_LEVEL", level)
		setupLogging()
	}
}
