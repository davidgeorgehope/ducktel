package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/davidgeorgehope/ducktel/internal/telemetry"
)

func TestTraceLookupDefaultReturnsSummaryObject(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "trace_lookup", map[string]any{"tenant": "acme", "trace_id": "trace-acme-1"})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	var summary telemetry.TraceSummary
	if err := json.Unmarshal([]byte(body), &summary); err != nil {
		t.Fatalf("default trace_lookup is not a summary object: %v\n%s", err, body)
	}
	if summary.TraceID != "trace-acme-1" || summary.SpanCount != 1 || summary.Root == nil {
		t.Errorf("summary = %+v", summary)
	}
}

func TestTraceLookupDetailSpansReturnsRows(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "trace_lookup", map[string]any{
		"tenant": "acme", "trace_id": "trace-acme-1", "detail": "spans",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	if n := countRows(t, body); n != 1 {
		t.Errorf("detail=spans returned %d rows, want 1\n%s", n, body)
	}
	if strings.Contains(body, `"span_count"`) {
		t.Errorf("detail=spans returned a summary, not rows:\n%s", body)
	}
}

func TestTraceLookupBadDetailErrors(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "trace_lookup", map[string]any{
		"tenant": "acme", "trace_id": "trace-acme-1", "detail": "everything",
	})
	if !isErr {
		t.Fatalf("bad detail should error, got: %s", body)
	}
	if !strings.Contains(body, "summary") || !strings.Contains(body, "spans") {
		t.Errorf("error does not list both detail values: %s", body)
	}
}

func TestSlowRequestsToolFromMetric(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "slow_requests", map[string]any{
		"tenant": "acme", "metric_name": "http.server.duration",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	var rows []telemetry.SlowRequest
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("slow_requests is not a JSON array: %v\n%s", err, body)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d slow requests, want 2", len(rows))
	}
	if rows[0].TraceID != "trace-acme-1" || rows[0].Value != 100 || rows[0].Unit != "ms" {
		t.Errorf("first row = %+v, want trace-acme-1 at 100 ms", rows[0])
	}
	if rows[0].Trace.SpanCount != 1 || rows[0].Trace.Root == nil {
		t.Errorf("first row has no brief trace summary: %+v", rows[0].Trace)
	}

	body, isErr = call(t, s, "slow_requests", map[string]any{
		"tenant": "acme", "metric_name": "http.server.duration", "service_name": "worker",
	})
	if isErr {
		t.Fatalf("service filter: %s", body)
	}
	if n := countRows(t, body); n != 1 {
		t.Errorf("service_name=worker returned %d rows, want 1\n%s", n, body)
	}
}

func TestSlowRequestsToolFromRootSpans(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "slow_requests", map[string]any{"tenant": "acme"})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	var rows []telemetry.SlowRequest
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("slow_requests is not a JSON array: %v\n%s", err, body)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the two root spans", len(rows))
	}
	for _, r := range rows {
		if r.Unit != "ms" || r.Trace.SpanCount != 1 || r.Trace.Root == nil {
			t.Errorf("row = %+v, want a ms measurement and a brief summary", r)
		}
	}
}

func TestSlowRequestsToolDashboardLinks(t *testing.T) {
	run := linkedServer(t)

	body := run("slow_requests", map[string]any{
		"tenant": "acme", "metric_name": "http.server.duration",
		"service_name": "api", "min_value": 50, "max_value": 150, "limit": 3,
	})
	view, q := dashboardLink(t, body)
	if view != "metrics" || q.Get("metric_name") != "http.server.duration" ||
		q.Get("aggregation") != "exemplars" || q.Get("min_value") != "50" ||
		q.Get("max_value") != "150" || q.Get("limit") != "3" ||
		q.Get("filter") != "service.name:api" {
		t.Fatalf("metric slow link %q %v", view, q)
	}

	body = run("slow_requests", map[string]any{
		"tenant": "acme", "service_name": "api", "min_value": 1, "max_value": 10,
	})
	view, q = dashboardLink(t, body)
	if view != "traces" || q.Get("service_name") != "api" ||
		q.Get("min_ms") != "1" || q.Get("max_ms") != "10" {
		t.Fatalf("span slow link %q %v", view, q)
	}
}
