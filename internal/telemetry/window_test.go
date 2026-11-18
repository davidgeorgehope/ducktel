package telemetry

import (
	"fmt"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// windowSliceTimes returns one timestamp in the middle of each of the three
// consecutive 10-minute slices ending now, so a test window boundary never
// falls on a seeded point.
func windowSliceTimes() (now, oldest, middle, newest time.Time) {
	now = time.Now()
	return now, now.Add(-25 * time.Minute), now.Add(-15 * time.Minute), now.Add(-5 * time.Minute)
}

func windowSpan(traceID string, ts time.Time) writer.TraceSpan {
	return writer.TraceSpan{
		TraceID: traceID, SpanID: "s-" + traceID, ParentSpanID: "",
		ServiceName: "api", SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
		StartTime: ts.UnixMicro(), EndTime: ts.UnixMicro(), DurationMs: 5,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
		Events:             "[]", Links: "[]",
	}
}

func windowLog(body string, ts time.Time) writer.LogRecord {
	return writer.LogRecord{
		Timestamp: ts.UnixMicro(), ServiceName: "api", SeverityText: "INFO",
		Body: body, Attributes: "{}",
		ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
	}
}

func windowMetric(name, typ string, ts time.Time, value float64) writer.MetricPoint {
	return writer.MetricPoint{
		MetricName: name, MetricType: typ, Timestamp: ts.UnixMicro(),
		ValueDouble: value, ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
	}
}

// windowHistPoint is one explicit-bucket histogram point. start may be the
// point's own timestamp, which marks it as the first point of a new series:
// the cumulative path then counts that first point whole.
func windowHistPoint(name, temp string, start, ts time.Time, count uint64, sum float64, buckets string) writer.MetricPoint {
	return writer.MetricPoint{
		MetricName: name, MetricType: "histogram", MetricUnit: "ms",
		Timestamp: ts.UnixMicro(), StartTimestamp: start.UnixMicro(),
		Count: count, Sum: sum, Min: 1, Max: 500,
		BucketCounts: buckets, ExplicitBounds: "[10,100]",
		AggregationTemporality: temp,
		Attributes:             "{}",
		ResourceAttributes:     `{"service.name":"api","tenant.id":"acme"}`,
		ServiceName:            "api",
	}
}

func newWindowCore(t *testing.T, spans []writer.TraceSpan, logs []writer.LogRecord, points []writer.MetricPoint) *Core {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	if len(spans) > 0 {
		w.Add(spans)
	}
	if len(logs) > 0 {
		w.AddLogs(logs)
	}
	if len(points) > 0 {
		w.AddMetrics(points)
	}
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

func TestSpanSearchUntilWindow(t *testing.T) {
	_, oldest, middle, newest := windowSliceTimes()
	c := newWindowCore(t, []writer.TraceSpan{
		windowSpan("oldest", oldest),
		windowSpan("middle", middle),
		windowSpan("newest", newest),
	}, nil, nil)

	search := func(since, until int) []string {
		t.Helper()
		rows, _, err := c.SearchSpans("acme", "", nil, since, until, 0)
		if err != nil {
			t.Fatalf("since=%d until=%d: %v", since, until, err)
		}
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, strVal(r["trace_id"]))
		}
		return ids
	}

	if got := search(30, 20); len(got) != 1 || got[0] != "oldest" {
		t.Errorf("since=30 until=20 = %v, want only oldest", got)
	}
	if got := search(30, 0); len(got) != 3 {
		t.Errorf("since=30 until=0 = %v, want all three slices", got)
	}
	middleSlice := search(20, 10)
	newestSlice := search(10, 0)
	if len(middleSlice) != 1 || middleSlice[0] != "middle" {
		t.Errorf("since=20 until=10 = %v, want only middle", middleSlice)
	}
	if len(newestSlice) != 1 || newestSlice[0] != "newest" {
		t.Errorf("since=10 until=0 = %v, want only newest", newestSlice)
	}
	whole := search(20, 0)
	if len(whole) != len(middleSlice)+len(newestSlice) {
		t.Errorf("since=20 = %v, want the middle and newest slices %v + %v", whole, middleSlice, newestSlice)
	}
}

func TestLogSearchUntilWindow(t *testing.T) {
	_, oldest, middle, newest := windowSliceTimes()
	c := newWindowCore(t, nil, []writer.LogRecord{
		windowLog("oldest", oldest),
		windowLog("middle", middle),
		windowLog("newest", newest),
	}, nil)

	search := func(since, until int) []string {
		t.Helper()
		rows, _, err := c.SearchLogs("acme", "", "", "", "", nil, since, until, 0)
		if err != nil {
			t.Fatalf("since=%d until=%d: %v", since, until, err)
		}
		bodies := make([]string, 0, len(rows))
		for _, r := range rows {
			bodies = append(bodies, strVal(r["body"]))
		}
		return bodies
	}

	if got := search(30, 20); len(got) != 1 || got[0] != "oldest" {
		t.Errorf("since=30 until=20 = %v, want only oldest", got)
	}
	if got := search(30, 0); len(got) != 3 {
		t.Errorf("since=30 until=0 = %v, want all three slices", got)
	}
	middleSlice := search(20, 10)
	newestSlice := search(10, 0)
	if len(middleSlice) != 1 || middleSlice[0] != "middle" {
		t.Errorf("since=20 until=10 = %v, want only middle", middleSlice)
	}
	if len(newestSlice) != 1 || newestSlice[0] != "newest" {
		t.Errorf("since=10 until=0 = %v, want only newest", newestSlice)
	}
	if got := search(20, 0); len(got) != len(middleSlice)+len(newestSlice) {
		t.Errorf("since=20 = %v, want %v + %v", got, middleSlice, newestSlice)
	}
}

func TestLogSearchTraceIDUntilWithoutSince(t *testing.T) {
	now := time.Now()
	old := windowLog("old", now.Add(-2*time.Hour))
	old.TraceID = "trace-old"
	recent := windowLog("recent", now.Add(-10*time.Minute))
	recent.TraceID = "trace-old"
	c := newWindowCore(t, nil, []writer.LogRecord{old, recent}, nil)

	// A trace-scoped lookup has no default start, but an until still ends the
	// window: only the record older than 30 minutes remains.
	rows, _, err := c.SearchLogs("acme", "trace-old", "", "", "", nil, 0, 30, 0)
	if err != nil {
		t.Fatalf("trace-scoped until: %v", err)
	}
	if len(rows) != 1 || strVal(rows[0]["body"]) != "old" {
		t.Errorf("rows = %v, want only the old record", rows)
	}

	// Without until the whole history is still searched.
	rows, _, err = c.SearchLogs("acme", "trace-old", "", "", "", nil, 0, 0, 0)
	if err != nil {
		t.Fatalf("trace-scoped whole history: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("rows = %d, want both records", len(rows))
	}
}

func TestMetricSearchUntilWindow(t *testing.T) {
	_, oldest, middle, newest := windowSliceTimes()
	var points []writer.MetricPoint
	for _, metric := range []string{"window.gauge", "window.sum"} {
		typ := "gauge"
		if metric == "window.sum" {
			typ = "sum"
		}
		points = append(points,
			windowMetric(metric, typ, oldest, 1),
			windowMetric(metric, typ, middle, 2),
			windowMetric(metric, typ, newest, 4),
		)
	}
	c := newWindowCore(t, nil, nil, points)

	sum := func(metric string, since, until int) float64 {
		t.Helper()
		rows, _, err := c.QueryMetric("acme", metric, "sum", since, until, "", nil)
		if err != nil {
			t.Fatalf("%s since=%d until=%d: %v", metric, since, until, err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s returned %d rows, want 1", metric, len(rows))
		}
		return asFloat64(t, rows[0]["value"])
	}

	for _, metric := range []string{"window.gauge", "window.sum"} {
		if got := sum(metric, 30, 20); got != 1 {
			t.Errorf("%s since=30 until=20 = %v, want the oldest slice 1", metric, got)
		}
		if got := sum(metric, 30, 0); got != 7 {
			t.Errorf("%s since=30 until=0 = %v, want all three slices 7", metric, got)
		}
		if got := sum(metric, 20, 10); got != 2 {
			t.Errorf("%s since=20 until=10 = %v, want the middle slice 2", metric, got)
		}
		if got := sum(metric, 10, 0); got != 4 {
			t.Errorf("%s since=10 until=0 = %v, want the newest slice 4", metric, got)
		}
		if got := sum(metric, 20, 0); got != 6 {
			t.Errorf("%s since=20 = %v, want the middle and newest slices 6", metric, got)
		}
	}
}

// windowHistogramPoints seeds three slices. Each cumulative slice is its own
// series (its start timestamp is inside the slice), so a slice query counts
// that series' increment whole. The delta series is one point per slice.
func windowHistogramPoints() (cumulative, delta []writer.MetricPoint, oldest, middle, newest time.Time) {
	_, oldest, middle, newest = windowSliceTimes()
	cumulative = []writer.MetricPoint{
		windowHistPoint("window.cumulative", temporalityCumulative, oldest.Add(-4*time.Minute), oldest.Add(-4*time.Minute), 0, 0, "[0,0,0]"),
		windowHistPoint("window.cumulative", temporalityCumulative, oldest.Add(-4*time.Minute), oldest, 10, 100, "[4,3,3]"),
		windowHistPoint("window.cumulative", temporalityCumulative, middle.Add(-4*time.Minute), middle.Add(-4*time.Minute), 0, 0, "[0,0,0]"),
		windowHistPoint("window.cumulative", temporalityCumulative, middle.Add(-4*time.Minute), middle, 10, 150, "[4,4,2]"),
		windowHistPoint("window.cumulative", temporalityCumulative, newest.Add(-4*time.Minute), newest.Add(-4*time.Minute), 0, 0, "[0,0,0]"),
		windowHistPoint("window.cumulative", temporalityCumulative, newest.Add(-4*time.Minute), newest, 20, 300, "[8,7,5]"),
	}
	delta = []writer.MetricPoint{
		windowHistPoint("window.delta", deltaTemporality, oldest, oldest, 10, 100, "[4,3,3]"),
		windowHistPoint("window.delta", deltaTemporality, middle, middle, 10, 150, "[4,4,2]"),
		windowHistPoint("window.delta", deltaTemporality, newest, newest, 20, 300, "[8,7,5]"),
	}
	return cumulative, delta, oldest, middle, newest
}

func TestHistogramUntilWindow(t *testing.T) {
	cumulative, delta, _, _, _ := windowHistogramPoints()
	c := newWindowCore(t, nil, nil, append(cumulative, delta...))

	value := func(metric, agg string, since, until int) float64 {
		t.Helper()
		rows, _, err := c.QueryMetric("acme", metric, agg, since, until, "", nil)
		if err != nil {
			t.Fatalf("%s %s since=%d until=%d: %v", metric, agg, since, until, err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s %s returned %d rows, want 1", metric, agg, len(rows))
		}
		return asFloat64(t, rows[0]["value"])
	}

	for _, metric := range []string{"window.cumulative", "window.delta"} {
		if got := value(metric, "count", 30, 20); got != 10 {
			t.Errorf("%s count since=30 until=20 = %v, want the oldest slice 10", metric, got)
		}
		if got := value(metric, "count", 30, 0); got != 40 {
			t.Errorf("%s count since=30 until=0 = %v, want all slices 40", metric, got)
		}
		if got := value(metric, "count", 20, 10); got != 10 {
			t.Errorf("%s count since=20 until=10 = %v, want the middle slice 10", metric, got)
		}
		if got := value(metric, "count", 10, 0); got != 20 {
			t.Errorf("%s count since=10 until=0 = %v, want the newest slice 20", metric, got)
		}
		if got := value(metric, "count", 20, 0); got != 30 {
			t.Errorf("%s count since=20 = %v, want the middle and newest slices 30", metric, got)
		}
		if got := value(metric, "sum", 30, 10); got != 250 {
			t.Errorf("%s sum since=30 until=10 = %v, want the two older slices 250", metric, got)
		}
	}
}

func TestExemplarsUntilWindow(t *testing.T) {
	now := time.Now()
	oldest, newest := now.Add(-25*time.Minute), now.Add(-5*time.Minute)
	exemplar := func(ts time.Time, traceID string, value float64) string {
		return fmt.Sprintf(`[{"time_unix_nano":%d,"trace_id":%q,"span_id":%q,"value":%v}]`,
			ts.UnixNano(), traceID, "s-"+traceID, value)
	}
	oldPoint := windowHistPoint("window.exemplars", deltaTemporality, oldest, oldest, 1, 10, "[1,0,0]")
	oldPoint.Exemplars = exemplar(oldest, "trace-oldest", 10)
	newPoint := windowHistPoint("window.exemplars", deltaTemporality, newest, newest, 1, 90, "[0,1,0]")
	newPoint.Exemplars = exemplar(newest, "trace-newest", 90)
	c := newWindowCore(t, nil, nil, []writer.MetricPoint{oldPoint, newPoint})

	rows, _, err := c.QueryMetricWith("acme", "window.exemplars", "exemplars", 30, "", nil,
		MetricOpts{Until: 20})
	if err != nil {
		t.Fatalf("exemplars until: %v", err)
	}
	if len(rows) != 1 || strVal(rows[0]["trace_id"]) != "trace-oldest" {
		t.Errorf("exemplars = %v, want only trace-oldest", rows)
	}
}

func TestHistogramBucketsUntilWindow(t *testing.T) {
	cumulative, delta, _, _, _ := windowHistogramPoints()
	c := newWindowCore(t, nil, nil, append(cumulative, delta...))

	for _, metric := range []string{"window.cumulative", "window.delta"} {
		rows, _, err := c.QueryMetric("acme", metric, "buckets", 30, 10, "", nil)
		if err != nil {
			t.Fatalf("%s buckets: %v", metric, err)
		}
		if len(rows) != 3 {
			t.Fatalf("%s buckets returned %d rows, want 3", metric, len(rows))
		}
		var total float64
		got := make([]float64, len(rows))
		for i, r := range rows {
			got[i] = asFloat64(t, r["value"])
			total += got[i]
		}
		want := []float64{8, 7, 5}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s buckets = %v, want %v", metric, got, want)
				break
			}
		}
		if total != 20 {
			t.Errorf("%s bucket total = %v, want the two older slices 20", metric, total)
		}
	}
}

func TestHistogramStepStopsBeforeUntil(t *testing.T) {
	cumulative, delta, _, _, _ := windowHistogramPoints()
	c := newWindowCore(t, nil, nil, append(cumulative, delta...))

	for _, metric := range []string{"window.cumulative", "window.delta"} {
		rows, _, err := c.QueryMetricWith("acme", metric, "count", 30, "", nil,
			MetricOpts{Step: "1m", Until: 10})
		if err != nil {
			t.Fatalf("%s step: %v", metric, err)
		}
		var total float64
		end := time.Now().Add(-10 * time.Minute).UnixMicro()
		for _, r := range rows {
			if ts := asInt64(t, r["time"]); ts >= end {
				t.Errorf("%s step row time %d is not before the window end %d", metric, ts, end)
			}
			total += asFloat64(t, r["value"])
		}
		if total != 20 {
			t.Errorf("%s step count total = %v, want the two older slices 20", metric, total)
		}
	}
}

func TestWindowValidation(t *testing.T) {
	_, oldest, _, _ := windowSliceTimes()
	c := newWindowCore(t,
		[]writer.TraceSpan{windowSpan("s", oldest)},
		[]writer.LogRecord{windowLog("l", oldest)},
		[]writer.MetricPoint{windowMetric("window.gauge", "gauge", oldest, 1)},
	)

	cases := []struct {
		name string
		err  error
	}{
		{"spans negative until", func() error {
			_, _, err := c.SearchSpans("acme", "", nil, 30, -1, 0)
			return err
		}()},
		{"spans until at since", func() error {
			_, _, err := c.SearchSpans("acme", "", nil, 30, 30, 0)
			return err
		}()},
		{"logs until past since", func() error {
			_, _, err := c.SearchLogs("acme", "", "", "", "", nil, 30, 40, 0)
			return err
		}()},
		{"metrics until at default since", func() error {
			_, _, err := c.QueryMetricWith("acme", "window.gauge", "count", 0, "", nil, MetricOpts{Until: 60})
			return err
		}()},
		{"slow until past since", func() error {
			_, err := c.SlowRequests("acme", SlowOpts{Since: 30, Until: 40})
			return err
		}()},
	}
	for _, tc := range cases {
		if tc.err == nil {
			t.Errorf("%s: expected an error", tc.name)
			continue
		}
		if !IsInvalidInput(tc.err) {
			t.Errorf("%s: error %v is not classified as invalid input", tc.name, tc.err)
		}
	}

	// A valid window still works.
	if _, _, err := c.SearchSpans("acme", "", nil, 30, 20, 0); err != nil {
		t.Errorf("valid until window: %v", err)
	}
}
