package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func seedSpan(svc, tenant, traceID string) writer.TraceSpan {
	now := time.Now()
	return writer.TraceSpan{
		TraceID: traceID, SpanID: "s-" + svc, ParentSpanID: "",
		ServiceName: svc, SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 5,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: `{"service.name":"` + svc + `","tenant.id":"` + tenant + `"}`,
		Events:             "[]", Links: "[]",
	}
}

// newTestMux seeds a store and returns a mux with /api/* registered, guarded
// by the given token (empty disables auth, matching httpauth.Bearer).
func newTestMux(t *testing.T, token string) *http.ServeMux {
	t.Helper()
	dir := t.TempDir()

	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		seedSpan("api", "acme", "trace-acme-1"),
		seedSpan("worker", "acme", "trace-acme-2"),
		seedSpan("api", "globex", "trace-globex-1"),
	})
	w.AddMetrics([]writer.MetricPoint{
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: time.Now().UnixMicro(),
			ValueDouble: 0.5, ResourceAttributes: `{"tenant.id":"acme"}`},
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: time.Now().UnixMicro(),
			ValueDouble: 0.9, ResourceAttributes: `{"tenant.id":"globex"}`},
	})
	w.AddLogs([]writer.LogRecord{
		{Timestamp: time.Now().UnixMicro(), ServiceName: "api", SeverityText: "ERROR",
			Body: "connection refused", Attributes: "{}", TraceID: "trace-acme-1",
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`},
		// A logs-only service: absent from traces, so it distinguishes the
		// logs-sourced services list from the traces-sourced one. No trace id
		// either — the record-outside-a-span case.
		{Timestamp: time.Now().UnixMicro(), ServiceName: "logsonly", SeverityText: "INFO",
			Body: "handled request", Attributes: "{}",
			ResourceAttributes: `{"service.name":"logsonly","tenant.id":"acme"}`},
		{Timestamp: time.Now().UnixMicro(), ServiceName: "api", SeverityText: "ERROR",
			Body: "globex-only failure", Attributes: "{}", TraceID: "trace-globex-1",
			ResourceAttributes: `{"service.name":"api","tenant.id":"globex"}`},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	mux := http.NewServeMux()
	NewHandlers(telemetry.NewCore(engine), token).Register(mux)
	return mux
}

func doReq(t *testing.T, mux *http.ServeMux, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// --- auth ---

func TestAuthRejectsMissingToken(t *testing.T) {
	mux := newTestMux(t, "secret")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuthRejectsWrongToken(t *testing.T) {
	mux := newTestMux(t, "secret")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuthAcceptsCorrectToken(t *testing.T) {
	mux := newTestMux(t, "secret")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme", "secret")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthDisabledWhenNoToken(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme", "")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with auth disabled: %s", rec.Code, rec.Body.String())
	}
}

// --- input validation -> 400 ---

func TestMissingTenantIs400(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/traces", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestBadFilterFormatIs400(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme&filter=noColon", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestUnknownAggregationIs400(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/metrics?tenant=acme&metric_name=cpu&aggregation=median", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// --- tenant isolation through the HTTP layer ---

func TestTenantIsolationThroughHTTP(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp rowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Errorf("acme got %d rows, want 2 (globex must not leak)", len(resp.Rows))
	}

	rec = doReq(t, mux, "GET", "/api/traces/trace-globex-1?tenant=acme", "")
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Rows) != 0 {
		t.Errorf("acme could read globex's trace via /api/traces/{id}: %d rows", len(resp.Rows))
	}
}

// --- /api/logs ---

func TestSearchLogsEndpoint(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/logs?tenant=acme&severity=error", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp rowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Errorf("severity=error returned %d rows, want 1 (the acme ERROR)", len(resp.Rows))
	}
	if resp.Rows[0]["service_name"] != "api" {
		t.Errorf("row has service %v", resp.Rows[0]["service_name"])
	}
}

func TestSearchLogsEndpointTenantScoped(t *testing.T) {
	mux := newTestMux(t, "")

	// acme must not see globex's ERROR, whatever other filters are absent.
	rec := doReq(t, mux, "GET", "/api/logs?tenant=acme", "")
	var resp rowsResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Rows) != 2 {
		t.Errorf("acme logs = %d rows, want 2 (globex leaked?)", len(resp.Rows))
	}
}

func TestSearchLogsEndpointMissingTenantIs400(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/logs", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestSearchLogsEndpointByTraceID covers the waterfall's query: all records
// of one trace, not windowed by the default 60 minutes.
func TestSearchLogsEndpointByTraceID(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/logs?tenant=acme&trace_id=trace-acme-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp rowsResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Rows) != 1 {
		t.Fatalf("trace-acme-1 returned %d records, want 1", len(resp.Rows))
	}
	if resp.Rows[0]["trace_id"] != "trace-acme-1" {
		t.Errorf("row has trace %v", resp.Rows[0]["trace_id"])
	}

	// globex's trace must not resolve under acme.
	rec = doReq(t, mux, "GET", "/api/logs?tenant=acme&trace_id=trace-globex-1", "")
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Rows) != 0 {
		t.Errorf("acme read globex's trace logs: %d rows", len(resp.Rows))
	}
}

func TestListServicesSourceLogs(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/services?tenant=acme&source=logs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp rowsResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	got := map[string]bool{}
	for _, r := range resp.Rows {
		got[r["service_name"].(string)] = true
	}
	if !got["api"] || !got["logsonly"] {
		t.Errorf("logs-sourced services = %v, want api and logsonly", resp.Rows)
	}
	if got["worker"] {
		t.Errorf("traces-only service leaked into the logs-sourced list: %v", resp.Rows)
	}

	// Default source stays traces (backwards compatible).
	rec = doReq(t, mux, "GET", "/api/services?tenant=acme", "")
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Rows) != 2 {
		t.Errorf("traces-sourced services = %d, want 2 (api, worker)", len(resp.Rows))
	}
}

func TestListServicesScopedToTenant(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/services?tenant=acme", "")
	var resp rowsResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Rows) != 2 {
		t.Errorf("acme services = %d, want 2 (api, worker)", len(resp.Rows))
	}
}

func TestListMetricNamesScopedToTenant(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/metric-names?tenant=acme", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp rowsResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Rows) != 1 || resp.Rows[0]["metric_name"] != "cpu.util" {
		t.Errorf("acme metric names = %v, want exactly [cpu.util]", resp.Rows)
	}
}

func TestListMetricNamesMissingTenantIs400(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/metric-names", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestEmptyResultSerializesAsArray pins the fix for a real dashboard
// regression: a query matching nothing used to serialize as "rows":null
// (Go's nil slice), and any client iterating the result crashed on it — the
// dashboard's first load silently died when the auto-selected tenant had no
// spans in the default window. An empty result must be [], never null.
func TestEmptyResultSerializesAsArray(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=no-such-tenant", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp rowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if resp.Rows == nil {
		t.Error(`empty result serialized as null; must be [] so clients can iterate without special-casing`)
	}
	if len(resp.Rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(resp.Rows))
	}
}

func TestListTenantsReturnsBoth(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/tenants", "")
	var resp rowsResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	got := map[string]bool{}
	for _, r := range resp.Rows {
		got[r["tenant"].(string)] = true
	}
	if !got["acme"] || !got["globex"] {
		t.Errorf("tenants = %v, want acme and globex", resp.Rows)
	}
}
