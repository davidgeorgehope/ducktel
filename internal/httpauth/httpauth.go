// Package httpauth provides bearer-token middleware shared by ducktel's HTTP
// surfaces.
//
// The OTLP receiver and the MCP server are separate trust domains — one grants
// write access to senders, the other read access to consumers — so each has its
// own token, but both check it identically.
package httpauth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Bearer wraps a handler with an RFC 6750 bearer-token check.
//
// It is a no-op when token is empty, and fails closed (401) when a token is
// configured but the request does not present it. The `Bearer ` prefix is
// required; the scheme name is case-insensitive. A bare token is rejected.
func Bearer(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if token == "" {
			next.ServeHTTP(w, req)
			return
		}
		scheme, cred, ok := strings.Cut(req.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Constant-time compare so the token cannot be recovered by timing.
		if subtle.ConstantTimeCompare([]byte(cred), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
}
