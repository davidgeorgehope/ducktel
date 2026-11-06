package ducktel_test

import (
	"bytes"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// --- OTLP bearer auth ---

const testToken = "s3cr3t-token"

// authBody is the smallest valid OTLP/JSON trace payload.
func authBody() []byte {
	nano := strconv.FormatInt(time.Now().UnixNano(), 10)
	return []byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"auth-svc"}}]},` +
		`"scopeSpans":[{"scope":{"name":"t"},"spans":[{"traceId":"0102030405060708090a0b0c0d0e0f10",` +
		`"spanId":"0102030405060708","name":"op","kind":2,"startTimeUnixNano":"` + nano +
		`","endTimeUnixNano":"` + nano + `"}]}]}]}`)
}

// postRawAuth sends the Authorization header verbatim, so tests can construct
// malformed credentials (bare tokens, wrong schemes) rather than only
// well-formed ones.
func postRawAuth(addr, path, headerValue string) (*http.Response, error) {
	req, err := http.NewRequest("POST", addr+path, bytes.NewReader(authBody()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if headerValue != "" {
		req.Header.Set("Authorization", headerValue)
	}
	return http.DefaultClient.Do(req)
}

// postWithAuth sends a well-formed `Authorization: Bearer <token>` header.
func postWithAuth(addr, path, token string) (*http.Response, error) {
	if token == "" {
		return postRawAuth(addr, path, "")
	}
	return postRawAuth(addr, path, "Bearer "+token)
}

// TestAuthRequiredWhenTokenConfigured verifies the receiver fails closed: with a
// token configured, a request without it must be rejected, not silently served.
func TestAuthRequiredWhenTokenConfigured(t *testing.T) {
	addr, _, cleanup := startTestServerWithToken(t, testToken)
	defer cleanup()

	for _, path := range []string{"/v1/traces", "/v1/logs", "/v1/metrics"} {
		resp, err := postWithAuth(addr, path, "") // no Authorization header
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", path, resp.StatusCode)
		}
	}
}

// TestAuthRejectsWrongToken covers near-misses and empty values, so the check
// cannot be satisfied by presentation rather than the secret itself.
func TestAuthRejectsWrongToken(t *testing.T) {
	addr, _, cleanup := startTestServerWithToken(t, testToken)
	defer cleanup()

	// Well-formed scheme but wrong secret: near-misses must not pass.
	wrong := []string{
		"not-the-token",
		testToken + "x",              // prefix + extra
		testToken[:len(testToken)-1], // truncation
		" " + testToken,              // leading space
	}
	for _, tok := range wrong {
		resp, err := postWithAuth(addr, "/v1/traces", tok)
		if err != nil {
			t.Fatalf("%q: %v", tok, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q = %d, want 401", tok, resp.StatusCode)
		}
	}

	// Malformed credentials: sent verbatim, so these exercise the scheme check.
	// NB: trailing whitespace is not included — RFC 7230 makes it
	// insignificant and Go's header parser trims it before we see it, so
	// asserting rejection there would be testing net/http, not this code.
	raw := []string{
		testToken,              // bare token, no scheme
		"Basic " + testToken,   // wrong scheme
		"Bearer",               // scheme with no credential
		"Bearer  " + testToken, // two spaces: empty credential, not the token
	}
	for _, h := range raw {
		resp, err := postRawAuth(addr, "/v1/traces", h)
		if err != nil {
			t.Fatalf("%q: %v", h, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("header %q = %d, want 401", h, resp.StatusCode)
		}
	}
}

// TestAuthSchemeIsCaseInsensitive verifies RFC 6750 tolerance for the scheme
// name, so `bearer` works as well as `Bearer`.
func TestAuthSchemeIsCaseInsensitive(t *testing.T) {
	addr, _, cleanup := startTestServerWithToken(t, testToken)
	defer cleanup()

	req, err := http.NewRequest("POST", addr+"/v1/traces", bytes.NewReader(authBody()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("lowercase scheme = %d, want 200", resp.StatusCode)
	}
}

// TestAuthAcceptsCorrectToken verifies the token does not break the happy path.
func TestAuthAcceptsCorrectToken(t *testing.T) {
	addr, w, cleanup := startTestServerWithToken(t, testToken)
	defer cleanup()

	resp, err := postWithAuth(addr, "/v1/traces", testToken)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token = %d, want 200", resp.StatusCode)
	}

	// And the data must actually land.
	w.Flush()
	if got := spanCount(t, w.DataDir()); got != 1 {
		t.Errorf("authenticated request stored %d spans, want 1", got)
	}
}

// TestAuthDisabledWhenNoToken verifies the default local workflow is unchanged:
// no token configured means no check, so dev usage is not broken.
func TestAuthDisabledWhenNoToken(t *testing.T) {
	addr, _, cleanup := startTestServer(t) // no token
	defer cleanup()

	resp, err := postWithAuth(addr, "/v1/traces", "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("no-token server rejected an unauthenticated request: %d", resp.StatusCode)
	}
}

// TestHealthStaysUnauthenticated verifies probes keep working under auth, since
// k8s liveness checks cannot send a bearer token.
func TestHealthStaysUnauthenticated(t *testing.T) {
	addr, _, cleanup := startTestServerWithToken(t, testToken)
	defer cleanup()

	resp, err := http.Get(addr + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health with auth enabled = %d, want 200", resp.StatusCode)
	}
}
