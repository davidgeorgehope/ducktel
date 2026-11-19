package telemetry

import (
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// notFilterSpan builds a span with a route attribute (omitted when route is
// "") and an env attribute, so exclusion tests can cover a key that is present
// and one that is missing.
func notFilterSpan(tenant, traceID, route, env string, durMs float64) writer.TraceSpan {
	attrs := `{"env":"` + env + `"}`
	if route != "" {
		attrs = `{"http.route":"` + route + `","env":"` + env + `"}`
	}
	now := time.Now()
	return writer.TraceSpan{
		TraceID: traceID, SpanID: "span-" + traceID, ParentSpanID: "",
		ServiceName: "api", SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: durMs,
		StatusCode: "STATUS_CODE_OK", Attributes: attrs,
		ResourceAttributes: `{"service.name":"api","tenant.id":"` + tenant + `"}`,
		Events:             "[]", Links: "[]",
	}
}

// notFilterCore seeds one span, one log record and one gauge point for routes
// /a and /b plus one record without the route key at all.
func notFilterCore(t *testing.T) *Core {
	t.Helper()
	now := time.Now()
	logRec := func(traceID, route string) writer.LogRecord {
		attrs := `{}`
		if route != "" {
			attrs = `{"http.route":"` + route + `"}`
		}
		return writer.LogRecord{
			Timestamp: now.UnixMicro(), SeverityText: "INFO", Body: "request",
			Attributes:         attrs,
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
	return newWindowCore(t,
		[]writer.TraceSpan{
			notFilterSpan("acme", "trace-a", "/a", "prod", 5),
			notFilterSpan("acme", "trace-b", "/b", "prod", 5),
			notFilterSpan("acme", "trace-none", "", "dev", 5),
		},
		[]writer.LogRecord{
			logRec("log-a", "/a"),
			logRec("log-b", "/b"),
			logRec("log-none", ""),
		},
		[]writer.MetricPoint{
			point("/a", 1),
			point("/b", 2),
			point("", 3),
		})
}

func rowTraceIDs(rows []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		out[strVal(r["trace_id"])] = true
	}
	return out
}

func TestSpanSearchNotEqual(t *testing.T) {
	c := notFilterCore(t)

	rows, _, err := c.SearchSpans("acme", "", []AttrFilter{{Key: "http.route", Op: "!=", Value: "/a"}}, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := rowTraceIDs(rows)
	if len(got) != 2 || !got["trace-b"] || !got["trace-none"] {
		t.Errorf("http.route != /a returned %v, want trace-b and trace-none", got)
	}

	// ANDed with an equal filter on another key: only the prod /b span.
	rows, _, err = c.SearchSpans("acme", "", []AttrFilter{
		{Key: "http.route", Op: "!=", Value: "/a"},
		{Key: "env", Value: "prod"},
	}, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got = rowTraceIDs(rows)
	if len(got) != 1 || !got["trace-b"] {
		t.Errorf("not-equal AND equal returned %v, want only trace-b", got)
	}

	// An empty exclusion value keeps records where the key is present and
	// non-empty in at least one scope.
	rows, _, err = c.SearchSpans("acme", "", []AttrFilter{{Key: "http.route", Op: "!=", Value: ""}}, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got = rowTraceIDs(rows)
	if len(got) != 2 || !got["trace-a"] || !got["trace-b"] {
		t.Errorf("http.route != \"\" returned %v, want trace-a and trace-b", got)
	}

	// The same exclusion applies to a key that only lives in resource
	// attributes: service.name is "api" on every seeded span.
	rows, _, err = c.SearchSpans("acme", "", []AttrFilter{{Key: "service.name", Op: "!=", Value: "api"}}, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("service.name != api returned %d spans, want 0", len(rows))
	}
}

func TestSearchLogsNotEqual(t *testing.T) {
	c := notFilterCore(t)

	rows, _, err := c.SearchLogs("acme", "", "", "", "", []AttrFilter{{Key: "http.route", Op: "!=", Value: "/a"}}, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := rowTraceIDs(rows)
	if len(got) != 2 || !got["log-b"] || !got["log-none"] {
		t.Errorf("log http.route != /a returned %v, want log-b and log-none", got)
	}

	rows, _, err = c.SearchLogs("acme", "", "", "", "", []AttrFilter{{Key: "http.route", Op: "!=", Value: ""}}, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := rowTraceIDs(rows); len(got) != 2 || !got["log-a"] || !got["log-b"] {
		t.Errorf("log http.route != \"\" returned %v, want log-a and log-b", got)
	}
}

func TestMetricQueryNotEqualGauge(t *testing.T) {
	c := notFilterCore(t)

	rows, _, err := c.QueryMetric("acme", "not.gauge", "count", 0, 0, "", []AttrFilter{{Key: "http.route", Op: "!=", Value: "/a"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 1 || asFloat64(t, rows[0]["value"]) != 2 {
		t.Errorf("gauge count with http.route != /a = %v, want 2", rows)
	}

	// Grouped, the excluded route must not appear as a group at all. The
	// keyless point is kept and groups under an empty value.
	rows, _, err = c.QueryMetric("acme", "not.gauge", "count", 0, 0, "attr.http.route",
		[]AttrFilter{{Key: "http.route", Op: "!=", Value: "/a"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]float64{}
	for _, r := range rows {
		got[strVal(r["attr.http.route"])] = asFloat64(t, r["value"])
	}
	if _, ok := got["/a"]; ok || got["/b"] != 1 || got[""] != 1 {
		t.Errorf("grouped gauge with exclusion = %v, want /b=1 and keyless=1, no /a", got)
	}
}

func TestHistogramNotEqual(t *testing.T) {
	c := newHistogramCore(t)

	rows, _, err := c.QueryMetric("acme", "http.server.duration", "count", 60, 0, "",
		[]AttrFilter{{Key: "http.route", Op: "!=", Value: "/a"}})
	if err != nil {
		t.Fatalf("histogram count: %v", err)
	}
	if len(rows) != 1 || asFloat64(t, rows[0]["value"]) != 4 {
		t.Errorf("histogram count with http.route != /a = %v, want /b's 4 observations", rows)
	}

	rows, _, err = c.QueryMetric("acme", "http.server.duration", "sum", 60, 0, "",
		[]AttrFilter{{Key: "http.route", Op: "!=", Value: "/a"}})
	if err != nil {
		t.Fatalf("histogram sum: %v", err)
	}
	if len(rows) != 1 || asFloat64(t, rows[0]["value"]) != 56 {
		t.Errorf("histogram sum with http.route != /a = %v, want /b's 56", rows)
	}
}

// comparePollCore seeds a fast group dominated by the polling route: 10 slow
// /orders spans, 20 fast /poll spans and 10 fast /orders spans.
func comparePollCore(t *testing.T) *Core {
	t.Helper()
	spans := make([]writer.TraceSpan, 0, 40)
	for i := 0; i < 10; i++ {
		spans = append(spans, notFilterSpan("acme", "slow-"+string(rune('a'+i)), "/orders", "prod", 400))
	}
	for i := 0; i < 20; i++ {
		spans = append(spans, notFilterSpan("acme", "poll-"+string(rune('a'+i)), "/poll", "prod", 5))
	}
	for i := 0; i < 10; i++ {
		spans = append(spans, notFilterSpan("acme", "fast-"+string(rune('a'+i)), "/orders", "prod", 20))
	}
	return newWindowCore(t, spans, nil, nil)
}

func TestCompareSpansNotEqualRemovesPollingRoute(t *testing.T) {
	c := comparePollCore(t)
	minMs, maxMs := 300.0, 500.0

	all, err := c.CompareSpans("acme", CompareOpts{
		ServiceName: "api",
		Slow:        Range{Min: &minMs, Max: &maxMs},
	})
	if err != nil {
		t.Fatalf("CompareSpans: %v", err)
	}
	if all.SlowN != 10 || all.RestN != 30 {
		t.Fatalf("unfiltered slow_n/rest_n = %d/%d, want 10/30", all.SlowN, all.RestN)
	}
	foundPoll := false
	for _, r := range all.Rows {
		if r.Key == "http.route" && r.Value == "/poll" {
			foundPoll = true
		}
	}
	if !foundPoll {
		t.Errorf("unfiltered comparison should show the polling route: %+v", all.Rows)
	}

	cmp, err := c.CompareSpans("acme", CompareOpts{
		ServiceName: "api",
		Filters:     []AttrFilter{{Key: "http.route", Op: "!=", Value: "/poll"}},
		Slow:        Range{Min: &minMs, Max: &maxMs},
	})
	if err != nil {
		t.Fatalf("CompareSpans with exclusion: %v", err)
	}
	if cmp.SlowN != 10 || cmp.RestN != 10 {
		t.Errorf("excluded slow_n/rest_n = %d/%d, want 10/10", cmp.SlowN, cmp.RestN)
	}
	for _, r := range cmp.Rows {
		if r.Key == "http.route" && r.Value == "/poll" {
			t.Errorf("polling route survived the exclusion: %+v", r)
		}
	}
}

func TestSlowRequestsNotEqual(t *testing.T) {
	c := newWindowCore(t, []writer.TraceSpan{
		notFilterSpan("acme", "trace-poll", "/poll", "prod", 900),
		notFilterSpan("acme", "trace-a", "/orders", "prod", 500),
		notFilterSpan("acme", "trace-b", "/orders", "prod", 300),
	}, nil, nil)

	all, err := c.SlowRequests("acme", SlowOpts{})
	if err != nil {
		t.Fatalf("SlowRequests: %v", err)
	}
	if len(all) != 3 || all[0].TraceID != "trace-poll" {
		t.Fatalf("unfiltered slow requests = %+v, want trace-poll first", all)
	}

	rows, err := c.SlowRequests("acme", SlowOpts{
		Filters: []AttrFilter{{Key: "http.route", Op: "!=", Value: "/poll"}},
	})
	if err != nil {
		t.Fatalf("SlowRequests with exclusion: %v", err)
	}
	if len(rows) != 2 || rows[0].TraceID != "trace-a" || rows[1].TraceID != "trace-b" {
		t.Errorf("excluded slow requests = %+v, want trace-a then trace-b", rows)
	}
}

func TestAttrFilterBadOpIsInvalidInput(t *testing.T) {
	c := notFilterCore(t)
	for _, op := range []string{"~", "<>"} {
		filters := []AttrFilter{{Key: "http.route", Value: "/a", Op: op}}
		if _, _, err := c.SearchSpans("acme", "", filters, 0, 0, 0); !IsInvalidInput(err) {
			t.Errorf("span_search op %q error = %v, want invalid input", op, err)
		}
		if _, _, err := c.SearchLogs("acme", "", "", "", "", filters, 0, 0, 0); !IsInvalidInput(err) {
			t.Errorf("log_search op %q error = %v, want invalid input", op, err)
		}
		if _, _, err := c.QueryMetric("acme", "not.gauge", "count", 0, 0, "", filters); !IsInvalidInput(err) {
			t.Errorf("metric_query op %q error = %v, want invalid input", op, err)
		}
		if _, err := c.CompareSpans("acme", CompareOpts{Filters: filters}); !IsInvalidInput(err) {
			t.Errorf("compare_spans op %q error = %v, want invalid input", op, err)
		}
		if _, err := c.SlowRequests("acme", SlowOpts{Filters: filters}); !IsInvalidInput(err) {
			t.Errorf("slow_requests op %q error = %v, want invalid input", op, err)
		}
	}
}
