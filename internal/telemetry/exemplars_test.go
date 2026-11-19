package telemetry

import (
	"strconv"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func ptr(f float64) *float64 { return &f }

// newCorrelationCore seeds http.server.duration points carrying exemplars and
// spans of several durations, for tenant acme, plus one globex point.
func newCorrelationCore(t *testing.T) *Core {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	now := time.Now()
	ns := func(d time.Duration) int64 { return now.Add(-d).UnixNano() }

	p1 := histPoint("/a", temporalityCumulative, now.Add(-time.Hour), now.Add(-20*time.Minute), 10, 100, "[5,4,1]")
	p1.Exemplars = `[{"time_unix_nano":` + itoa(ns(21*time.Minute)) + `,"trace_id":"t-slow","span_id":"s1","value":420},` +
		`{"time_unix_nano":` + itoa(ns(22*time.Minute)) + `,"trace_id":"t-fast","span_id":"s2","value":4}]`
	// The next export repeats t-slow (a cumulative reservoir may) and adds
	// one exemplar without a trace id, which cannot be followed.
	p2 := histPoint("/a", temporalityCumulative, now.Add(-time.Hour), now.Add(-10*time.Minute), 12, 130, "[6,5,1]")
	p2.Exemplars = `[{"time_unix_nano":` + itoa(ns(21*time.Minute)) + `,"trace_id":"t-slow","span_id":"s1","value":420},` +
		`{"time_unix_nano":` + itoa(ns(11*time.Minute)) + `,"trace_id":"","span_id":"","value":900},` +
		`{"time_unix_nano":` + itoa(ns(12*time.Minute)) + `,"trace_id":"t-mid","span_id":"s3","value":60}]`
	p3 := histPoint("/b", deltaTemporality, now.Add(-6*time.Minute), now.Add(-5*time.Minute), 1, 70, "[0,1,0]")
	p3.Exemplars = `[{"time_unix_nano":` + itoa(ns(5*time.Minute)) + `,"trace_id":"t-b","span_id":"s4","value":70}]`
	other := histPoint("/a", deltaTemporality, now.Add(-6*time.Minute), now.Add(-5*time.Minute), 1, 999, "[0,0,1]")
	other.ResourceAttributes = `{"service.name":"web","tenant.id":"globex"}`
	other.Exemplars = `[{"time_unix_nano":` + itoa(ns(5*time.Minute)) + `,"trace_id":"t-globex","span_id":"s9","value":999}]`
	noEx := histPoint("/c", deltaTemporality, now.Add(-6*time.Minute), now.Add(-5*time.Minute), 1, 1, "[1,0,0]")
	w.AddMetrics([]writer.MetricPoint{p1, p2, p3, other, noEx})

	span := func(id string, ms float64) writer.TraceSpan {
		s := seedSpan("web", "acme", id, "{}")
		s.SpanID, s.DurationMs = "sp-"+id, ms
		return s
	}
	w.Add([]writer.TraceSpan{span("d5", 5), span("d250", 250), span("d300", 300), span("d500", 500), span("d900", 900)})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	return NewCore(engine)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func traceIDs(rows []map[string]any) []string {
	var out []string
	for _, r := range rows {
		s, _ := r["trace_id"].(string)
		out = append(out, s)
	}
	return out
}

func TestExemplarsSlowestFirstDeduplicated(t *testing.T) {
	c := newCorrelationCore(t)
	rows, cols, err := c.QueryMetricWith("acme", "http.server.duration", "exemplars", 60, "", nil, MetricOpts{})
	if err != nil {
		t.Fatal(err)
	}
	got := traceIDs(rows)
	want := []string{"t-slow", "t-b", "t-mid", "t-fast"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v (repeat deduplicated, untraced skipped, other tenant excluded)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if len(cols) != 4 || cols[0] != "timestamp" || cols[3] != "span_id" {
		t.Fatalf("columns %v", cols)
	}
	if v := toFloat(rows[0]["value"]); v != 420 {
		t.Fatalf("value %v, want 420", v)
	}
}

func TestExemplarsValueRangeAndLimit(t *testing.T) {
	c := newCorrelationCore(t)
	rows, _, err := c.QueryMetricWith("acme", "http.server.duration", "exemplars", 60, "", nil,
		MetricOpts{Value: Range{Min: ptr(50), Max: ptr(100)}})
	if err != nil {
		t.Fatal(err)
	}
	if got := traceIDs(rows); len(got) != 2 || got[0] != "t-b" || got[1] != "t-mid" {
		t.Fatalf("50..100: got %v, want [t-b t-mid]", got)
	}
	rows, _, _ = c.QueryMetricWith("acme", "http.server.duration", "exemplars", 60, "", nil, MetricOpts{Limit: 1})
	if got := traceIDs(rows); len(got) != 1 || got[0] != "t-slow" {
		t.Fatalf("limit 1: got %v", got)
	}
	if _, _, err := c.QueryMetricWith("acme", "http.server.duration", "exemplars", 60, "", nil,
		MetricOpts{Value: Range{Min: ptr(5), Max: ptr(1)}}); !IsInvalidInput(err) {
		t.Fatalf("min above max: want invalid input, got %v", err)
	}
}

func TestExemplarsGroupedAndFiltered(t *testing.T) {
	c := newCorrelationCore(t)
	rows, cols, err := c.QueryMetricWith("acme", "http.server.duration", "exemplars", 60, "attr.http.route", nil, MetricOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if cols[0] != "attr.http.route" {
		t.Fatalf("columns %v", cols)
	}
	if rows[1]["attr.http.route"] != "/b" || rows[1]["trace_id"] != "t-b" {
		t.Fatalf("row 1 %v", rows[1])
	}
	rows, _, _ = c.QueryMetricWith("acme", "http.server.duration", "exemplars", 60, "",
		[]AttrFilter{{Key: "http.route", Value: "/b"}}, MetricOpts{})
	if got := traceIDs(rows); len(got) != 1 || got[0] != "t-b" {
		t.Fatalf("filtered: got %v", got)
	}
}

func TestSpansByDuration(t *testing.T) {
	c := newCorrelationCore(t)
	count := func(r Range) []string {
		rows, _, err := c.SearchSpansIn("acme", "", nil, 0, 0, 0, r)
		if err != nil {
			t.Fatal(err)
		}
		return traceIDs(rows)
	}
	if got := count(Range{Min: ptr(250), Max: ptr(500)}); len(got) != 3 {
		t.Fatalf("250..500 inclusive: got %v, want d250 d300 d500", got)
	}
	if got := count(Range{Min: ptr(600)}); len(got) != 1 || got[0] != "d900" {
		t.Fatalf(">=600: got %v", got)
	}
	if got := count(Range{}); len(got) != 5 {
		t.Fatalf("no range: got %v", got)
	}
	if _, _, err := c.SearchSpansIn("acme", "", nil, 0, 0, 0, Range{Min: ptr(2), Max: ptr(1)}); !IsInvalidInput(err) {
		t.Fatalf("min above max: want invalid input, got %v", err)
	}
}

// A filter that matches nothing leaves no point to decide the type by:
// buckets is then an empty result, not "not a histogram"; count stays 0 and
// an unknown aggregation is still an error.
func TestNoMatchingPointsIsEmpty(t *testing.T) {
	c := newCorrelationCore(t)
	none := []AttrFilter{{Key: "http.route", Value: "/nowhere"}}
	for _, agg := range []string{"buckets", "exemplars"} {
		rows, _, err := c.QueryMetric("acme", "http.server.duration", agg, 60, 0, "", none)
		if err != nil || len(rows) != 0 {
			t.Fatalf("%s: rows %v err %v", agg, rows, err)
		}
	}
	// count keeps its one-row answer for an empty window.
	rows, _, err := c.QueryMetric("acme", "http.server.duration", "count", 60, 0, "", none)
	if err != nil || len(rows) != 1 || toFloat(rows[0]["value"]) != 0 {
		t.Fatalf("count: rows %v err %v", rows, err)
	}
	if _, _, err := c.QueryMetric("acme", "http.server.duration", "nope", 60, 0, "", none); !IsInvalidInput(err) {
		t.Fatalf("unknown aggregation: %v", err)
	}
}
