package webapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func windowSpan(traceID string, ts time.Time, durationMs float64) writer.TraceSpan {
	return writer.TraceSpan{
		TraceID: traceID, SpanID: "s-" + traceID, ParentSpanID: "",
		ServiceName: "api", SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
		StartTime: ts.UnixMicro(), EndTime: ts.UnixMicro(), DurationMs: durationMs,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
		Events:             "[]", Links: "[]",
	}
}

// newWindowMux seeds the oldest and newest of three 10-minute slices.
func newWindowMux(t *testing.T) *http.ServeMux {
	t.Helper()
	now := time.Now()
	oldest, newest := now.Add(-25*time.Minute), now.Add(-5*time.Minute)

	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		windowSpan("trace-oldest", oldest, 50),
		windowSpan("trace-newest", newest, 5),
	})
	w.AddLogs([]writer.LogRecord{
		{Timestamp: oldest.UnixMicro(), ServiceName: "api", SeverityText: "INFO",
			Body: "oldest", Attributes: "{}",
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`},
		{Timestamp: newest.UnixMicro(), ServiceName: "api", SeverityText: "INFO",
			Body: "newest", Attributes: "{}",
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`},
	})
	w.AddMetrics([]writer.MetricPoint{
		{MetricName: "window.gauge", MetricType: "gauge", Timestamp: oldest.UnixMicro(),
			ValueDouble: 1, ResourceAttributes: `{"tenant.id":"acme"}`},
		{MetricName: "window.gauge", MetricType: "gauge", Timestamp: newest.UnixMicro(),
			ValueDouble: 4, ResourceAttributes: `{"tenant.id":"acme"}`},
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
	NewHandlers(telemetry.NewCore(engine), telemetry.NewFlusher("", ""), "").Register(mux)
	return mux
}

func TestAPIUntilWindowOnEveryEndpoint(t *testing.T) {
	mux := newWindowMux(t)

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme&since_minutes=30&until_minutes=20", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("traces status = %d: %s", rec.Code, rec.Body.String())
	}
	var spans rowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &spans); err != nil {
		t.Fatalf("traces JSON: %v", err)
	}
	if len(spans.Rows) != 1 || spans.Rows[0]["trace_id"] != "trace-oldest" {
		t.Errorf("/api/traces = %v, want only trace-oldest", spans.Rows)
	}

	rec = doReq(t, mux, "GET", "/api/logs?tenant=acme&since_minutes=30&until_minutes=20", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status = %d: %s", rec.Code, rec.Body.String())
	}
	var logs rowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &logs); err != nil {
		t.Fatalf("logs JSON: %v", err)
	}
	if len(logs.Rows) != 1 || logs.Rows[0]["body"] != "oldest" {
		t.Errorf("/api/logs = %v, want only the oldest record", logs.Rows)
	}

	rec = doReq(t, mux, "GET", "/api/metrics?tenant=acme&metric_name=window.gauge&aggregation=sum&since_minutes=30&until_minutes=20", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", rec.Code, rec.Body.String())
	}
	var metrics rowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &metrics); err != nil {
		t.Fatalf("metrics JSON: %v", err)
	}
	if len(metrics.Rows) != 1 || metrics.Rows[0]["value"] != float64(1) {
		t.Errorf("/api/metrics = %v, want one row with value 1", metrics.Rows)
	}

	rec = doReq(t, mux, "GET", "/api/slow?tenant=acme&since_minutes=30&until_minutes=20", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("slow status = %d: %s", rec.Code, rec.Body.String())
	}
	var slow []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &slow); err != nil {
		t.Fatalf("slow JSON: %v", err)
	}
	if len(slow) != 1 || slow[0]["trace_id"] != "trace-oldest" {
		t.Errorf("/api/slow = %v, want only trace-oldest", slow)
	}
}

func TestAPIUntilValidationIs400(t *testing.T) {
	mux := newWindowMux(t)

	targets := []string{
		"/api/traces?tenant=acme&since_minutes=10&until_minutes=10",
		"/api/logs?tenant=acme&since_minutes=10&until_minutes=10",
		"/api/metrics?tenant=acme&metric_name=window.gauge&aggregation=count&since_minutes=10&until_minutes=10",
		"/api/slow?tenant=acme&since_minutes=10&until_minutes=10",
		"/api/traces?tenant=acme&since_minutes=30&until_minutes=-1",
	}
	for _, target := range targets {
		rec := doReq(t, mux, "GET", target, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", target, rec.Code, rec.Body.String())
		}
	}
}

func TestMetricsParamsHelpMentionsUntil(t *testing.T) {
	mux := newWindowMux(t)

	rec := doReq(t, mux, "GET", "/api/metrics?tenant=acme", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "until_minutes") {
		t.Errorf("help does not mention until_minutes: %s", rec.Body.String())
	}
}
