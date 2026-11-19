package webapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// newNotFilterMux seeds routes /poll, /a and /b plus one record without the
// route key across all three signals, so filter_not can be checked end to end.
func newNotFilterMux(t *testing.T) *http.ServeMux {
	t.Helper()
	now := time.Now()
	span := func(traceID, route string, durMs float64) writer.TraceSpan {
		attrs := `{}`
		if route != "" {
			attrs = `{"http.route":"` + route + `"}`
		}
		return writer.TraceSpan{
			TraceID: traceID, SpanID: "span-" + traceID, ParentSpanID: "",
			ServiceName: "api", SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
			StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: durMs,
			StatusCode: "STATUS_CODE_OK", Attributes: attrs,
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
			Events:             "[]", Links: "[]",
		}
	}
	logRec := func(traceID, route string) writer.LogRecord {
		attrs := `{}`
		if route != "" {
			attrs = `{"http.route":"` + route + `"}`
		}
		return writer.LogRecord{
			Timestamp: now.UnixMicro(), SeverityText: "INFO", Body: "request", Attributes: attrs,
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
			ServiceName:        "api", TraceID: traceID,
		}
	}
	point := func(route string, value float64) writer.MetricPoint {
		attrs := `{}`
		if route != "" {
			attrs = `{"http.route":"` + route + `"}`
		}
		return writer.MetricPoint{
			MetricName: "not.gauge", MetricType: "gauge", Timestamp: now.UnixMicro(),
			ValueDouble: value, Attributes: attrs,
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
			ServiceName:        "api",
		}
	}

	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		span("trace-poll", "/poll", 900),
		span("trace-a", "/a", 500),
		span("trace-b", "/b", 300),
		span("trace-none", "", 100),
	})
	w.AddLogs([]writer.LogRecord{
		logRec("log-poll", "/poll"),
		logRec("log-a", "/a"),
		logRec("log-b", "/b"),
		logRec("log-none", ""),
	})
	w.AddMetrics([]writer.MetricPoint{
		point("/a", 1),
		point("/b", 2),
		point("", 3),
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

func TestAPIFilterNot(t *testing.T) {
	mux := newNotFilterMux(t)

	for _, tc := range []struct {
		target string
		rows   int
	}{
		{"/api/traces?tenant=acme&filter_not=http.route:/a", 3},
		{"/api/logs?tenant=acme&filter_not=http.route:/a", 3},
	} {
		rec := doReq(t, mux, "GET", tc.target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", tc.target, rec.Code, rec.Body.String())
		}
		var resp rowsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: bad JSON: %v", tc.target, err)
		}
		if len(resp.Rows) != tc.rows {
			t.Errorf("%s returned %d rows, want %d", tc.target, len(resp.Rows), tc.rows)
		}
		for _, r := range resp.Rows {
			if attrs, ok := r["attributes"].(map[string]any); ok && attrs["http.route"] == "/a" {
				t.Errorf("%s kept the excluded /a row: %v", tc.target, r)
			}
		}
	}

	rec := doReq(t, mux, "GET", "/api/metrics?tenant=acme&metric_name=not.gauge&aggregation=count&filter_not=http.route:/a", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics: status %d: %s", rec.Code, rec.Body.String())
	}
	var resp rowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("metrics: bad JSON: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0]["value"] != float64(2) {
		t.Errorf("metrics count with exclusion = %v, want 2", resp.Rows)
	}

	rec = doReq(t, mux, "GET", "/api/slow?tenant=acme&filter_not=http.route:/poll", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("slow: status %d: %s", rec.Code, rec.Body.String())
	}
	var slow []telemetry.SlowRequest
	if err := json.Unmarshal(rec.Body.Bytes(), &slow); err != nil {
		t.Fatalf("slow: bad JSON: %v", err)
	}
	if len(slow) != 3 || slow[0].TraceID != "trace-a" {
		t.Errorf("slow with exclusion = %+v, want trace-a first and no poll", slow)
	}

	rec = doReq(t, mux, "GET", "/api/compare?tenant=acme&min_ms=400&max_ms=600&filter_not=http.route:/poll", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("compare: status %d: %s", rec.Code, rec.Body.String())
	}
	var cmp telemetry.SpanComparison
	if err := json.Unmarshal(rec.Body.Bytes(), &cmp); err != nil {
		t.Fatalf("compare: bad JSON: %v", err)
	}
	if cmp.SlowN != 1 || cmp.RestN != 2 {
		t.Errorf("compare slow_n/rest_n = %d/%d, want 1/2 with poll excluded", cmp.SlowN, cmp.RestN)
	}
	for _, r := range cmp.Rows {
		if r.Key == "http.route" && r.Value == "/poll" {
			t.Errorf("compare kept the excluded polling route: %+v", r)
		}
	}
}

func TestAPIBadFilterNotIs400(t *testing.T) {
	mux := newNotFilterMux(t)

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme&filter_not=noColon", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad filter_not: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
