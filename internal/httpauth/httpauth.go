// Package httpauth provides token middleware shared by ducktel's HTTP
// surfaces.
//
// The OTLP receiver and the MCP server are separate trust domains — one grants
// write access to senders, the other read access to consumers — so each has its
// own token, but both check a bearer token identically. The dashboard, which
// shares the MCP read token, also accepts HTTP Basic so a browser can prompt
// for the password (the token) without a separate cookie session.
package httpauth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// DashboardChallenge is the WWW-Authenticate value Browser sends on 401, so a
// browser will show a login prompt. The realm is informational only.
const DashboardChallenge = `Basic realm="ducktel dashboard"`

// Bearer wraps a handler with an RFC 6750 bearer-token check.
//
// It is a no-op when token is empty, and fails closed (401) when a token is
// configured but the request does not present it. The `Bearer ` prefix is
// required; the scheme name is case-insensitive. A bare token is rejected.
// HTTP Basic is rejected here: the OTLP receiver and the MCP endpoint stay
// bearer-only so existing senders and MCP clients do not change.
func Bearer(token string, next http.Handler) http.Handler {
	return check(token, false, "", next)
}

// Browser is the check for the dashboard SPA and its /api routes. It accepts
// the same token as Bearer, or as the password in HTTP Basic (the username is
// ignored). A 401 includes a Basic WWW-Authenticate challenge so a browser
// will prompt. MCP clients should keep using Bearer on /mcp, which is wrapped
// with Bearer instead.
func Browser(token string, next http.Handler) http.Handler {
	return check(token, true, DashboardChallenge, next)
}

func check(token string, allowBasic bool, challenge string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if token == "" {
			next.ServeHTTP(w, req)
			return
		}
		if authorized(token, allowBasic, req) {
			next.ServeHTTP(w, req)
			return
		}
		if challenge != "" {
			w.Header().Set("WWW-Authenticate", challenge)
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func authorized(token string, allowBasic bool, req *http.Request) bool {
	scheme, cred, ok := strings.Cut(req.Header.Get("Authorization"), " ")
	if !ok {
		return false
	}
	if strings.EqualFold(scheme, "Bearer") {
		return subtle.ConstantTimeCompare([]byte(cred), []byte(token)) == 1
	}
	if allowBasic && strings.EqualFold(scheme, "Basic") {
		_, pass, ok := req.BasicAuth()
		if !ok {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(pass), []byte(token)) == 1
	}
	return false
}
