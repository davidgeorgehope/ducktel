// Package webapi serves ducktel's stored telemetry as a plain JSON REST API,
// for a browser-based dashboard that cannot speak MCP's Streamable HTTP
// framing the way an agent client can. It wraps the exact same
// internal/telemetry.Core the MCP tools use, so tenant isolation and query
// construction are not reimplemented here — this package only translates HTTP
// query parameters into Core calls and Core results into JSON.
package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/davidgeorgehope/ducktel/internal/httpauth"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
)

// Handlers holds the shared query core and flush wiring behind the REST API.
type Handlers struct {
	core    *telemetry.Core
	flusher *telemetry.Flusher
	token   string
}

// NewHandlers builds the REST handlers. token, when non-empty, is required as
// a bearer token on every route — the same read token the MCP server uses;
// see internal/httpauth for the check itself.
func NewHandlers(core *telemetry.Core, flusher *telemetry.Flusher, token string) *Handlers {
	return &Handlers{core: core, flusher: flusher, token: token}
}

// Register mounts /api/* routes onto mux, each guarded by the same bearer
// check as every other authenticated surface in ducktel.
func (h *Handlers) Register(mux *http.ServeMux) {
	auth := func(fn http.HandlerFunc) http.Handler {
		return logRequests(httpauth.Bearer(h.token, fn))
	}
	mux.Handle("GET /api/traces", auth(h.handleSearchSpans))
	mux.Handle("GET /api/traces/{trace_id}", auth(h.handleLookupTrace))
	mux.Handle("GET /api/logs", auth(h.handleSearchLogs))
	mux.Handle("GET /api/metrics", auth(h.handleQueryMetric))
	mux.Handle("GET /api/tenants", auth(h.handleListTenants))
	mux.Handle("GET /api/services", auth(h.handleListServices))
	mux.Handle("GET /api/metric-names", auth(h.handleListMetricNames))
	mux.Handle("POST /api/flush", auth(h.handleFlush))
	mux.Handle("GET /api/config", auth(h.handleConfig))
}

// --- response helpers ---

type rowsResponse struct {
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
}

// writeRows sanitizes non-finite float values (NaN/Inf from a metric
// aggregate) before marshaling — encoding/json fails outright on those,
// unlike MCP's MarshalRows, which degrades them to null per-cell. It also
// normalises a nil slice to an empty one, so a query that matched nothing
// serialises as "rows":[] rather than "rows":null — a client iterating the
// result should never have to special-case an empty set.
func writeRows(w http.ResponseWriter, rows []map[string]any, cols []string) {
	rows = telemetry.SanitizeRows(rows)
	if rows == nil {
		rows = []map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rowsResponse{Columns: cols, Rows: rows})
}

// writeErr maps a Core error to a status code: a caller input problem (missing
// tenant, bad aggregation, ...) is 400; anything else is a query engine
// failure, 500.
func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if telemetry.IsInvalidInput(err) {
		status = http.StatusBadRequest
	}
	http.Error(w, err.Error(), status)
}

func queryInt(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// --- handlers ---

func (h *Handlers) handleSearchSpans(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Filters arrive as repeated ?filter=key:value pairs.
	var filters []telemetry.AttrFilter
	for _, raw := range q["filter"] {
		key, value, ok := strings.Cut(raw, ":")
		if !ok {
			http.Error(w, fmt.Sprintf("filter %q must be key:value", raw), http.StatusBadRequest)
			return
		}
		filters = append(filters, telemetry.AttrFilter{Key: key, Value: value})
	}

	rows, cols, err := h.core.SearchSpans(
		q.Get("tenant"), q.Get("service_name"), filters,
		queryInt(r, "since_minutes", 0), queryInt(r, "limit", 0),
	)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRows(w, rows, cols)
}

func (h *Handlers) handleSearchLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Same repeated ?filter=key:value pairs as the span search endpoint.
	var filters []telemetry.AttrFilter
	for _, raw := range q["filter"] {
		key, value, ok := strings.Cut(raw, ":")
		if !ok {
			http.Error(w, fmt.Sprintf("filter %q must be key:value", raw), http.StatusBadRequest)
			return
		}
		filters = append(filters, telemetry.AttrFilter{Key: key, Value: value})
	}

	rows, cols, err := h.core.SearchLogs(
		q.Get("tenant"), q.Get("trace_id"), q.Get("service_name"), q.Get("severity"), q.Get("search"), filters,
		queryInt(r, "since_minutes", 0), queryInt(r, "limit", 0),
	)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRows(w, rows, cols)
}

func (h *Handlers) handleLookupTrace(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("trace_id")
	q := r.URL.Query()

	rows, cols, err := h.core.LookupTrace(traceID, q.Get("tenant"), queryInt(r, "limit", 0))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRows(w, rows, cols)
}

func (h *Handlers) handleQueryMetric(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Filters arrive as repeated ?filter=key:value pairs, same as the span
	// search endpoint.
	var filters []telemetry.AttrFilter
	for _, raw := range q["filter"] {
		key, value, ok := strings.Cut(raw, ":")
		if !ok {
			http.Error(w, fmt.Sprintf("filter %q must be key:value", raw), http.StatusBadRequest)
			return
		}
		filters = append(filters, telemetry.AttrFilter{Key: key, Value: value})
	}

	rows, cols, err := h.core.QueryMetric(
		q.Get("tenant"), q.Get("metric_name"), q.Get("aggregation"),
		queryInt(r, "since_minutes", 0), q.Get("group_by"), filters,
	)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRows(w, rows, cols)
}

func (h *Handlers) handleListTenants(w http.ResponseWriter, r *http.Request) {
	rows, cols, err := h.core.ListTenants()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRows(w, rows, cols)
}

func (h *Handlers) handleListServices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// ?source=logs lists services seen in the log signal instead of traces;
	// the two sets can differ, and the dashboard's log filter needs the
	// logs-derived one.
	var (
		rows []map[string]any
		cols []string
		err  error
	)
	if q.Get("source") == "logs" {
		rows, cols, err = h.core.ListLogServices(q.Get("tenant"))
	} else {
		rows, cols, err = h.core.ListServices(q.Get("tenant"))
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRows(w, rows, cols)
}

func (h *Handlers) handleListMetricNames(w http.ResponseWriter, r *http.Request) {
	rows, cols, err := h.core.ListMetricNames(r.URL.Query().Get("tenant"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRows(w, rows, cols)
}

// handleFlush proxies to the shared Flusher — the same request the
// flush_buffer MCP tool issues, authorised with this server's own token, never
// the ingest token.
func (h *Handlers) handleFlush(w http.ResponseWriter, r *http.Request) {
	if !h.flusher.Enabled() {
		http.Error(w, "flush is not configured", http.StatusNotFound)
		return
	}
	if err := h.flusher.Flush(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"flushed"}`))
}

// handleConfig lets the dashboard decide whether to render the flush button
// without guessing — mirrors the MCP server only advertising flush_buffer
// when a flush endpoint is configured.
func (h *Handlers) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"flush_enabled": h.flusher.Enabled()})
}
