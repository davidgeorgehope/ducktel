package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFlusherDisabledWhenNoURL(t *testing.T) {
	f := NewFlusher("", "")
	if f.Enabled() {
		t.Error("Flusher with empty url should not be enabled")
	}
	if err := f.Flush(context.Background()); err == nil {
		t.Error("Flush with no url configured should error")
	}
}

func TestFlusherSendsTokenAndSucceeds(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := NewFlusher(srv.URL, "secret")
	if !f.Enabled() {
		t.Fatal("Flusher with a url should be enabled")
	}
	if err := f.Flush(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization header = %q, want Bearer secret", gotAuth)
	}
}

func TestFlusherPropagatesRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	f := NewFlusher(srv.URL, "wrong")
	if err := f.Flush(context.Background()); err == nil {
		t.Error("a rejected flush should return an error")
	}
}
