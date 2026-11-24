package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davidgeorgehope/ducktel/internal/httpauth"
)

func TestDashboardStaticRequiresToken(t *testing.T) {
	mux := http.NewServeMux()
	mountDashboard(mux, "secret")

	for _, path := range []string{"/dashboard", "/dashboard/", "/dashboard/app.js", "/dashboard/style.css"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", path, rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != httpauth.DashboardChallenge {
			t.Errorf("%s WWW-Authenticate = %q, want %q", path, got, httpauth.DashboardChallenge)
		}
	}
}

func TestDashboardStaticAcceptsBearerAndBasic(t *testing.T) {
	mux := http.NewServeMux()
	mountDashboard(mux, "secret")

	cases := []struct {
		name string
		auth func(*http.Request)
		path string
		want int
	}{
		{"bearer slash", func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret") }, "/dashboard/", http.StatusOK},
		{"basic slash", func(r *http.Request) { r.SetBasicAuth("ducktel", "secret") }, "/dashboard/", http.StatusOK},
		{"basic app.js", func(r *http.Request) { r.SetBasicAuth("", "secret") }, "/dashboard/app.js", http.StatusOK},
		{"bearer redirect", func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret") }, "/dashboard", http.StatusMovedPermanently},
		{"wrong basic", func(r *http.Request) { r.SetBasicAuth("ducktel", "nope") }, "/dashboard/", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			tc.auth(req)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestHealthUnaffectedByDashboardMount(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mountDashboard(mux, "secret")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/health = %d, want 200", rec.Code)
	}
}
