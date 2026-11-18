package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func TestTraceSummaryEndpoint(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/traces/trace-acme-1/summary?tenant=acme", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	var s telemetry.TraceSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("response is not a summary object: %v: %s", err, rec.Body.String())
	}
	if s.TraceID != "trace-acme-1" || s.SpanCount != 1 || s.Root == nil {
		t.Errorf("summary = %+v", s)
	}
}

func TestTraceSummaryEndpointTenantScoped(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/traces/trace-globex-1/summary?tenant=acme", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var s telemetry.TraceSummary
	json.Unmarshal(rec.Body.Bytes(), &s)
	if s.SpanCount != 0 || s.Root != nil {
		t.Errorf("acme saw globex's trace summary: %+v", s)
	}

	if rec := doReq(t, mux, "GET", "/api/traces/trace-acme-1/summary", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("missing tenant: status %d, want 400", rec.Code)
	}
}

func TestSlowEndpointRootSpans(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/slow?tenant=acme", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	var rows []telemetry.SlowRequest
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("response is not a JSON array: %v: %s", err, rec.Body.String())
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the two acme root spans", len(rows))
	}
	for _, r := range rows {
		if r.Unit != "ms" || r.Trace.SpanCount != 1 || r.Trace.Root == nil {
			t.Errorf("row = %+v, want a ms measurement with a brief summary", r)
		}
		if r.TraceID == "trace-globex-1" {
			t.Errorf("globex trace leaked: %+v", r)
		}
	}

	rec = doReq(t, mux, "GET", "/api/slow?tenant=acme&service_name=api", "")
	json.Unmarshal(rec.Body.Bytes(), &rows)
	if len(rows) != 1 || rows[0].TraceID != "trace-acme-1" {
		t.Errorf("service_name=api: %+v", rows)
	}
}

func TestSlowEndpointMetric(t *testing.T) {
	mux := newSlowMetricMux(t)

	rec := doReq(t, mux, "GET", "/api/slow?tenant=acme&metric_name=http.server.duration", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var rows []telemetry.SlowRequest
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("bad JSON: %v: %s", err, rec.Body.String())
	}
	if len(rows) != 2 || rows[0].TraceID != "trace-slow" || rows[0].Value != 420 || rows[0].Unit != "ms" {
		t.Fatalf("rows = %+v, want trace-slow at 420 ms first", rows)
	}
	if rows[0].Trace.SpanCount != 1 || rows[0].Trace.Root == nil {
		t.Errorf("metric slow row has no brief summary: %+v", rows[0].Trace)
	}
}

func TestSlowEndpointBadInput(t *testing.T) {
	mux := newTestMux(t, "")

	for target, want := range map[string]int{
		"/api/slow":                                              http.StatusBadRequest,
		"/api/slow?tenant=acme&min_value=abc":                    http.StatusBadRequest,
		"/api/slow?tenant=acme&min_value=10&max_value=1":         http.StatusBadRequest,
		"/api/slow?tenant=acme&filter=noColon":                   http.StatusBadRequest,
		"/api/slow?tenant=acme&min_value=1&max_value=10&limit=2": http.StatusOK,
	} {
		if rec := doReq(t, mux, "GET", target, ""); rec.Code != want {
			t.Errorf("%s: status = %d, want %d (%s)", target, rec.Code, want, rec.Body.String())
		}
	}
}

// newSlowMetricMux seeds matching spans and metric exemplars for the metric
// source of /api/slow.
func newSlowMetricMux(t *testing.T) *http.ServeMux {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	now := time.Now()

	w.Add([]writer.TraceSpan{
		seedSpan("api", "acme", "trace-slow"),
		seedSpan("api", "acme", "trace-fast"),
	})
	w.AddMetrics([]writer.MetricPoint{{
		MetricName: "http.server.duration", MetricType: "histogram", MetricUnit: "ms",
		Timestamp: now.UnixMicro(), Count: 2, Sum: 424, Min: 4, Max: 420,
		BucketCounts: "[1,1]", ExplicitBounds: "[50]",
		AggregationTemporality: "AGGREGATION_TEMPORALITY_CUMULATIVE",
		Attributes:             "{}",
		ResourceAttributes:     `{"service.name":"api","tenant.id":"acme"}`,
		ServiceName:            "api",
		Exemplars: fmt.Sprintf(`[{"time_unix_nano":%d,"trace_id":"trace-slow","span_id":"s-slow","value":420},`+
			`{"time_unix_nano":%d,"trace_id":"trace-fast","span_id":"s-fast","value":4}]`,
			now.Add(-time.Minute).UnixNano(), now.Add(-2*time.Minute).UnixNano()),
	}})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	mux := http.NewServeMux()
	h := NewHandlers(telemetry.NewCore(engine), telemetry.NewFlusher("", ""), "")
	h.Register(mux)
	return mux
}
