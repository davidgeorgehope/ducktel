package webapi

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// logRequests writes one line per /api request: method, path, the query
// parameter NAMES plus the cost-shaping values (since_minutes, limit), status,
// response bytes and duration. Without it there is no way to tell which
// request drove the query process into its memory limit. Filter values and
// tokens are never logged.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		q := r.URL.Query()
		names := make([]string, 0, len(q))
		for k := range q {
			names = append(names, k)
		}
		sort.Strings(names)
		log.Printf("api: %s %s params=[%s] since_minutes=%s limit=%s status=%d bytes=%d dur=%s",
			r.Method, r.URL.Path, strings.Join(names, ","),
			orDash(q.Get("since_minutes")), orDash(q.Get("limit")),
			rec.status, rec.bytes, time.Since(start).Round(time.Millisecond))
	})
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}
