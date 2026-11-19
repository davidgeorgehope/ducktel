package telemetry

import (
	"fmt"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// spanColumnSpan seeds one span whose name, kind and status are explicit, so
// filters on those columns can be told apart from attribute filters.
func spanColumnSpan(traceID, name, kind, status string, durMs float64) writer.TraceSpan {
	now := time.Now()
	return writer.TraceSpan{
		TraceID: traceID, SpanID: "span-" + traceID, ParentSpanID: "",
		ServiceName: "api", SpanName: name, SpanKind: kind,
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: durMs,
		StatusCode: status, Attributes: "{}",
		ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
		Events:             "[]", Links: "[]",
	}
}

func checkTraceSet(t *testing.T, what string, rows []map[string]any, want ...string) {
	t.Helper()
	got := rowTraceIDs(rows)
	if len(got) != len(want) {
		t.Errorf("%s returned %v, want %v", what, got, want)
		return
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("%s returned %v, want %v", what, got, want)
			return
		}
	}
}

func TestSpanSearchBySpanColumns(t *testing.T) {
	c := newWindowCore(t, []writer.TraceSpan{
		spanColumnSpan("trace-mcp", "POST /mcp", "SPAN_KIND_SERVER", "STATUS_CODE_OK", 900),
		spanColumnSpan("trace-orders", "GET /orders", "SPAN_KIND_SERVER", "STATUS_CODE_OK", 500),
		spanColumnSpan("trace-client", "GET /orders", "SPAN_KIND_CLIENT", "STATUS_CODE_ERROR", 300),
	}, nil, nil)

	for _, tc := range []struct {
		name    string
		filters []AttrFilter
		want    []string
	}{
		{"span_name !=", []AttrFilter{{Key: "span_name", Op: "!=", Value: "POST /mcp"}},
			[]string{"trace-orders", "trace-client"}},
		{"span_name =", []AttrFilter{{Key: "span_name", Value: "POST /mcp"}},
			[]string{"trace-mcp"}},
		{"status_code !=", []AttrFilter{{Key: "status_code", Op: "!=", Value: "STATUS_CODE_ERROR"}},
			[]string{"trace-mcp", "trace-orders"}},
		{"status_code =", []AttrFilter{{Key: "status_code", Value: "STATUS_CODE_ERROR"}},
			[]string{"trace-client"}},
		{"span_kind !=", []AttrFilter{{Key: "span_kind", Op: "!=", Value: "SPAN_KIND_CLIENT"}},
			[]string{"trace-mcp", "trace-orders"}},
		{"span_kind =", []AttrFilter{{Key: "span_kind", Value: "SPAN_KIND_CLIENT"}},
			[]string{"trace-client"}},
	} {
		rows, _, err := c.SearchSpans("acme", "", tc.filters, 0, 0, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		checkTraceSet(t, tc.name, rows, tc.want...)
	}
}

func TestSpanColumnKeyStaysAnAttributeFilterForLogsAndMetrics(t *testing.T) {
	now := time.Now()
	c := newWindowCore(t, nil,
		[]writer.LogRecord{
			{
				Timestamp: now.UnixMicro(), SeverityText: "INFO", Body: "request",
				Attributes:         `{"span_name":"POST /mcp"}`,
				ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
				ServiceName:        "api", TraceID: "log-mcp",
			},
			{
				Timestamp: now.UnixMicro(), SeverityText: "INFO", Body: "request",
				Attributes:         `{"span_name":"GET /orders"}`,
				ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
				ServiceName:        "api", TraceID: "log-orders",
			},
		},
		[]writer.MetricPoint{
			{
				MetricName: "span.filter.gauge", MetricType: "gauge", Timestamp: now.UnixMicro(),
				ValueDouble: 1, Attributes: `{"span_name":"POST /mcp"}`,
				ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
				ServiceName:        "api",
			},
			{
				MetricName: "span.filter.gauge", MetricType: "gauge", Timestamp: now.UnixMicro(),
				ValueDouble: 2, Attributes: `{"span_name":"GET /orders"}`,
				ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
				ServiceName:        "api",
			},
			{
				MetricName: "span.filter.gauge", MetricType: "gauge", Timestamp: now.UnixMicro(),
				ValueDouble: 3, Attributes: `{}`,
				ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
				ServiceName:        "api",
			},
		})

	rows, _, err := c.SearchLogs("acme", "", "", "", "", []AttrFilter{{Key: "span_name", Value: "POST /mcp"}}, 0, 0, 0)
	if err != nil {
		t.Fatalf("log_search span_name =: %v", err)
	}
	checkTraceSet(t, "log_search span_name =", rows, "log-mcp")

	rows, _, err = c.SearchLogs("acme", "", "", "", "", []AttrFilter{{Key: "span_name", Op: "!=", Value: "POST /mcp"}}, 0, 0, 0)
	if err != nil {
		t.Fatalf("log_search span_name !=: %v", err)
	}
	checkTraceSet(t, "log_search span_name !=", rows, "log-orders")

	rows, _, err = c.QueryMetric("acme", "span.filter.gauge", "count", 0, 0, "", []AttrFilter{{Key: "span_name", Value: "POST /mcp"}})
	if err != nil {
		t.Fatalf("metric_query span_name =: %v", err)
	}
	if len(rows) != 1 || asFloat64(t, rows[0]["value"]) != 1 {
		t.Errorf("metric_query span_name = returned %v, want count 1", rows)
	}

	rows, _, err = c.QueryMetric("acme", "span.filter.gauge", "count", 0, 0, "", []AttrFilter{{Key: "span_name", Op: "!=", Value: "POST /mcp"}})
	if err != nil {
		t.Fatalf("metric_query span_name !=: %v", err)
	}
	if len(rows) != 1 || asFloat64(t, rows[0]["value"]) != 2 {
		t.Errorf("metric_query span_name != returned %v, want count 2", rows)
	}
}

func TestCompareSpansSpanNameNotEqual(t *testing.T) {
	var spans []writer.TraceSpan
	for i := 0; i < 10; i++ {
		spans = append(spans,
			spanColumnSpan(fmt.Sprintf("mcp-%d", i), "POST /mcp", "SPAN_KIND_SERVER", "STATUS_CODE_OK", 400),
			spanColumnSpan(fmt.Sprintf("slow-%d", i), "GET /orders", "SPAN_KIND_SERVER", "STATUS_CODE_OK", 400),
			spanColumnSpan(fmt.Sprintf("fast-%d", i), "GET /orders", "SPAN_KIND_SERVER", "STATUS_CODE_OK", 20))
	}
	c := newWindowCore(t, spans, nil, nil)
	minMs, maxMs := 300.0, 500.0

	all, err := c.CompareSpans("acme", CompareOpts{
		ServiceName: "api",
		Slow:        Range{Min: &minMs, Max: &maxMs},
	})
	if err != nil {
		t.Fatalf("CompareSpans: %v", err)
	}
	if all.SlowN != 20 || all.RestN != 10 {
		t.Fatalf("unfiltered slow_n/rest_n = %d/%d, want 20/10", all.SlowN, all.RestN)
	}
	foundMCP := false
	for _, r := range all.Rows {
		if r.Key == "span_name" && r.Value == "POST /mcp" {
			foundMCP = true
		}
	}
	if !foundMCP {
		t.Errorf("unfiltered comparison should show POST /mcp: %+v", all.Rows)
	}

	cmp, err := c.CompareSpans("acme", CompareOpts{
		ServiceName: "api",
		Filters:     []AttrFilter{{Key: "span_name", Op: "!=", Value: "POST /mcp"}},
		Slow:        Range{Min: &minMs, Max: &maxMs},
	})
	if err != nil {
		t.Fatalf("CompareSpans with span_name exclusion: %v", err)
	}
	if cmp.SlowN != 10 || cmp.RestN != 10 {
		t.Errorf("excluded slow_n/rest_n = %d/%d, want 10/10", cmp.SlowN, cmp.RestN)
	}
	for _, r := range cmp.Rows {
		if r.Key == "span_name" && r.Value == "POST /mcp" {
			t.Errorf("POST /mcp survived the exclusion: %+v", r)
		}
	}
}

func TestSlowRequestsSpanNameNotEqual(t *testing.T) {
	c := newWindowCore(t, []writer.TraceSpan{
		spanColumnSpan("trace-mcp", "POST /mcp", "SPAN_KIND_SERVER", "STATUS_CODE_OK", 900),
		spanColumnSpan("trace-a", "GET /orders", "SPAN_KIND_SERVER", "STATUS_CODE_OK", 500),
		spanColumnSpan("trace-b", "GET /orders", "SPAN_KIND_SERVER", "STATUS_CODE_OK", 300),
	}, nil, nil)

	all, err := c.SlowRequests("acme", SlowOpts{})
	if err != nil {
		t.Fatalf("SlowRequests: %v", err)
	}
	if len(all) != 3 || all[0].TraceID != "trace-mcp" {
		t.Fatalf("unfiltered slow requests = %+v, want trace-mcp first", all)
	}

	rows, err := c.SlowRequests("acme", SlowOpts{
		Filters: []AttrFilter{{Key: "span_name", Op: "!=", Value: "POST /mcp"}},
	})
	if err != nil {
		t.Fatalf("SlowRequests with span_name exclusion: %v", err)
	}
	if len(rows) != 2 || rows[0].TraceID != "trace-a" || rows[1].TraceID != "trace-b" {
		t.Errorf("excluded slow requests = %+v, want trace-a then trace-b", rows)
	}
}
