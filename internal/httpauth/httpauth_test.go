package httpauth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testToken = "s3cr3t-token"

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

func get(t *testing.T, h http.Handler, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBearerRejectsMissingAndWrong(t *testing.T) {
	h := Bearer(testToken, okHandler())
	cases := []string{
		"",
		testToken, // bare token, no scheme
		"Basic " + base64.StdEncoding.EncodeToString([]byte(":"+testToken)),
		"Bearer",
		"Bearer  " + testToken,
		"Bearer not-the-token",
		"Bearer " + testToken + "x",
	}
	for _, auth := range cases {
		rec := get(t, h, auth)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q = %d, want 401", auth, rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("auth %q set WWW-Authenticate on Bearer surface", auth)
		}
	}
}

func TestBearerAcceptsCorrectToken(t *testing.T) {
	h := Bearer(testToken, okHandler())
	for _, auth := range []string{"Bearer " + testToken, "bearer " + testToken} {
		rec := get(t, h, auth)
		if rec.Code != http.StatusOK {
			t.Errorf("auth %q = %d, want 200", auth, rec.Code)
		}
	}
}

func TestBearerDisabledWhenNoToken(t *testing.T) {
	h := Bearer("", okHandler())
	rec := get(t, h, "")
	if rec.Code != http.StatusOK {
		t.Errorf("empty token = %d, want 200", rec.Code)
	}
}

func TestBrowserAcceptsBearerAndBasic(t *testing.T) {
	h := Browser(testToken, okHandler())
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("ducktel:"+testToken))
	emptyUser := "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+testToken))
	for _, auth := range []string{"Bearer " + testToken, "bearer " + testToken, basic, emptyUser} {
		rec := get(t, h, auth)
		if rec.Code != http.StatusOK {
			t.Errorf("auth %q = %d, want 200", auth, rec.Code)
		}
	}
}

func TestBrowserRejectsWrongAndChallenges(t *testing.T) {
	h := Browser(testToken, okHandler())
	wrongBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("ducktel:wrong"))
	cases := []string{"", "Bearer wrong", wrongBasic, testToken}
	for _, auth := range cases {
		rec := get(t, h, auth)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q = %d, want 401", auth, rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != DashboardChallenge {
			t.Errorf("auth %q WWW-Authenticate = %q, want %q", auth, got, DashboardChallenge)
		}
	}
}
