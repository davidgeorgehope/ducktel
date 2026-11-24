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

// NewHandlers builds the REST handlers. token, when non-empty, is required on
// every route — the same read token the MCP server uses, as a bearer token or
// as HTTP Basic (password = the token). See internal/httpauth.Browser.
func NewHandlers(core *telemetry.Core, flusher *telemetry.Flusher, token string) *Handlers {
	return &Handlers{core: core, flusher: flusher, token: token}
}

// Register mounts /api/* routes onto mux, each guarded by the dashboard
// check: Bearer or HTTP Basic, with a Basic challenge on 401 so a browser
// can prompt. /mcp stays on httpauth.Bearer and is not registered here.
func (h *Handlers) Register(mux *http.ServeMux) {
	auth := func(fn http.HandlerFunc) http.Handler {
		return logRequests(httpauth.Browser(h.token, fn))
	}
	mux.Handle("GET /api/traces", auth(h.handleSearchSpans))
	mux.Handle("GET /api/traces/{trace_id}", auth(h.handleLookupTrace))
	mux.Handle("GET /api/traces/{trace_id}/summary", auth(h.handleTraceSummary))
	mux.Handle("GET /api/slow", auth(h.handleSlowRequests))
	mux.Handle("GET /api/compare", auth(h.handleCompareSpans))
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
// writeJSON writes an object or array result. It is the non-rows counterpart
// of writeRows: there are no engine floats here to sanitise, so a plain
// encoder is enough, but the Content-Type must be set the same way.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

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

// queryFilters parses repeated ?filter=key:value pairs (equal matches) and
// ?filter_not=key:value pairs (exclusions), the form used by the span search,
// log search, metric, slow-request and compare endpoints.
func queryFilters(w http.ResponseWriter, r *http.Request) ([]telemetry.AttrFilter, bool) {
	var filters []telemetry.AttrFilter
	for _, p := range []struct{ name, op string }{
		{"filter", ""},
		{"filter_not", "!="},
	} {
		for _, raw := range r.URL.Query()[p.name] {
			key, value, ok := strings.Cut(raw, ":")
			if !ok {
				http.Error(w, fmt.Sprintf("%s %q must be key:value", p.name, raw), http.StatusBadRequest)
				return nil, false
			}
			filters = append(filters, telemetry.AttrFilter{Key: key, Value: value, Op: p.op})
		}
	}
	return filters, true
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

// queryRange reads an optional numeric range from two query parameters. A
// value that is present but not a number is a 400, not silently ignored: a
// dropped bound would return a different set than the caller asked for.
func queryRange(w http.ResponseWriter, r *http.Request, minName, maxName string) (telemetry.Range, bool) {
	var rg telemetry.Range
	for _, p := range []struct {
		name string
		dst  **float64
	}{{minName, &rg.Min}, {maxName, &rg.Max}} {
		v := r.URL.Query().Get(p.name)
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("%s %q is not a number", p.name, v), http.StatusBadRequest)
			return rg, false
		}
		*p.dst = &f
	}
	return rg, true
}

// --- handlers ---

func (h *Handlers) handleSearchSpans(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filters, ok := queryFilters(w, r)
	if !ok {
		return
	}

	dur, ok := queryRange(w, r, "min_ms", "max_ms")
	if !ok {
		return
	}

	rows, cols, err := h.core.SearchSpansIn(
		q.Get("tenant"), q.Get("service_name"), filters,
		queryInt(r, "since_minutes", 0), queryInt(r, "until_minutes", 0), queryInt(r, "limit", 0), dur,
	)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRows(w, rows, cols)
}

func (h *Handlers) handleSearchLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filters, ok := queryFilters(w, r)
	if !ok {
		return
	}

	rows, cols, err := h.core.SearchLogs(
		q.Get("tenant"), q.Get("trace_id"), q.Get("service_name"), q.Get("severity"), q.Get("search"), filters,
		queryInt(r, "since_minutes", 0), queryInt(r, "until_minutes", 0), queryInt(r, "limit", 0),
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

// handleTraceSummary is the compact counterpart of handleLookupTrace: the
// same aggregated object the MCP trace_lookup returns by default.
func (h *Handlers) handleTraceSummary(w http.ResponseWriter, r *http.Request) {
	tn := r.URL.Query().Get("tenant")
	summary, err := h.core.SummarizeTrace(r.PathValue("trace_id"), tn)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, summary)
}

// handleSlowRequests is the REST form of the slow_requests MCP tool.
func (h *Handlers) handleSlowRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filters, ok := queryFilters(w, r)
	if !ok {
		return
	}

	val, ok := queryRange(w, r, "min_value", "max_value")
	if !ok {
		return
	}

	rows, err := h.core.SlowRequests(q.Get("tenant"), telemetry.SlowOpts{
		MetricName:  q.Get("metric_name"),
		ServiceName: q.Get("service_name"),
		SpanName:    q.Get("span_name"),
		Filters:     filters,
		Since:       queryInt(r, "since_minutes", 0),
		Until:       queryInt(r, "until_minutes", 0),
		Value:       val,
		Limit:       queryInt(r, "limit", 0),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, rows)
}

// handleCompareSpans is the REST form of the compare_spans MCP tool: the
// same scope and slow range, the same SpanComparison body.
func (h *Handlers) handleCompareSpans(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filters, ok := queryFilters(w, r)
	if !ok {
		return
	}

	slow, ok := queryRange(w, r, "min_ms", "max_ms")
	if !ok {
		return
	}

	cmp, err := h.core.CompareSpans(q.Get("tenant"), telemetry.CompareOpts{
		ServiceName: q.Get("service_name"),
		SpanName:    q.Get("span_name"),
		SpanKind:    q.Get("span_kind"),
		Filters:     filters,
		Since:       queryInt(r, "since_minutes", 0),
		Until:       queryInt(r, "until_minutes", 0),
		Slow:        slow,
		Limit:       queryInt(r, "limit", 0),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, cmp)
}

// metricParamsHelp is the 400 body for /api/metrics without its required
// parameters.
const metricParamsHelp = "metric_name and aggregation are required. Parameters: " +
	"metric_name (required; see /api/metric-names), " +
	"aggregation (required: avg, sum, min, max, count, p50, p95, p99, exemplars; buckets for histograms), " +
	"tenant, since_minutes (default 60), " +
	"until_minutes (optional; where the window ends, default 0 = now, must be less than since_minutes), " +
	"group_by (comma-separated columns, attr.<key> data-point attributes, or resource.<key> resource attributes), " +
	"step (optional time series: 30s, 1m, 5m, 1h or whole seconds; adds a time column), " +
	"filter=key:value (repeatable), filter_not=key:value (repeatable exclusion), " +
	"min_value, max_value, limit (exemplars only)"

func (h *Handlers) handleQueryMetric(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("metric_name") == "" || q.Get("aggregation") == "" {
		// Name every parameter at once: an agent calling the REST API
		// directly should not have to discover them one 400 at a time.
		http.Error(w, metricParamsHelp, http.StatusBadRequest)
		return
	}

	filters, ok := queryFilters(w, r)
	if !ok {
		return
	}

	val, ok := queryRange(w, r, "min_value", "max_value")
	if !ok {
		return
	}

	rows, cols, err := h.core.QueryMetricWith(
		q.Get("tenant"), q.Get("metric_name"), q.Get("aggregation"),
		queryInt(r, "since_minutes", 0), q.Get("group_by"), filters,
		telemetry.MetricOpts{Value: val, Limit: queryInt(r, "limit", 0), Step: q.Get("step"), Until: queryInt(r, "until_minutes", 0)},
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
