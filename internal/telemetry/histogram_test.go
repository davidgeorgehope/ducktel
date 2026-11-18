package telemetry

import (
	"math"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

const deltaTemporality = "AGGREGATION_TEMPORALITY_DELTA"

func histPoint(route, temp string, start, ts time.Time, count uint64, sum float64, buckets string) writer.MetricPoint {
	return writer.MetricPoint{
		MetricName: "http.server.duration", MetricType: "histogram", MetricUnit: "ms",
		Timestamp: ts.UnixMicro(), StartTimestamp: start.UnixMicro(),
		Count: count, Sum: sum, Min: 1, Max: 500,
		BucketCounts: buckets, ExplicitBounds: "[10,100]",
		AggregationTemporality: temp,
		Attributes:             `{"http.route":"` + route + `"}`,
		ResourceAttributes:     `{"service.name":"web","tenant.id":"acme"}`,
		ServiceName:            "web",
	}
}

// newHistogramCore seeds two routes:
//
//	/a  cumulative, started before the window: 100 -> 110 -> 130 observations,
//	    then the process restarts inside the window: 4 -> 6.
//	    Increments: 10 + 20 (first point is the baseline) + 4 + 2 = 36,
//	    sum 100 + 200 + 20 + 30 = 350, buckets [15,16,5].
//	/b  delta: 3 + 1 = 4 observations, sum 56, buckets [3,1,0].
func newHistogramCore(t *testing.T) *Core {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	restart := now.Add(-10 * time.Minute)
	c := temporalityCumulative
	w.AddMetrics([]writer.MetricPoint{
		// Outside the 60-minute window entirely.
		histPoint("/a", c, old, now.Add(-90*time.Minute), 50, 500, "[25,20,5]"),
		histPoint("/a", c, old, now.Add(-50*time.Minute), 100, 1000, "[50,40,10]"),
		histPoint("/a", c, old, now.Add(-40*time.Minute), 110, 1100, "[55,44,11]"),
		histPoint("/a", c, old, now.Add(-30*time.Minute), 130, 1300, "[60,55,15]"),
		histPoint("/a", c, restart, now.Add(-9*time.Minute), 4, 20, "[4,0,0]"),
		histPoint("/a", c, restart, now.Add(-5*time.Minute), 6, 50, "[5,1,0]"),
		histPoint("/b", deltaTemporality, now.Add(-20*time.Minute), now.Add(-19*time.Minute), 3, 6, "[3,0,0]"),
		histPoint("/b", deltaTemporality, now.Add(-19*time.Minute), now.Add(-18*time.Minute), 1, 50, "[0,1,0]"),
		// A count that drops without a new start time is a reset: 50 is the
		// baseline, 2 is counted whole.
		renamed(histPoint("/r", c, old, now.Add(-30*time.Minute), 50, 500, "[50,0,0]"), "reset.duration"),
		renamed(histPoint("/r", c, old, now.Add(-20*time.Minute), 2, 9, "[2,0,0]"), "reset.duration"),
		{MetricName: "exp.duration", MetricType: "exponential_histogram", Timestamp: now.Add(-time.Minute).UnixMicro(),
			StartTimestamp: now.Add(-2 * time.Minute).UnixMicro(), Count: 7, Sum: 70,
			AggregationTemporality: deltaTemporality, BucketCounts: `{"scale":0}`,
			Attributes: "{}", ResourceAttributes: `{"tenant.id":"acme"}`},
	})
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

func metricValues(t *testing.T, c *Core, metric, agg, groupBy string) map[string]float64 {
	t.Helper()
	rows, _, err := c.QueryMetric("acme", metric, agg, 60, groupBy, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", metric, agg, err)
	}
	out := map[string]float64{}
	for _, r := range rows {
		key := ""
		if groupBy != "" {
			key, _ = r[groupBy].(string)
		}
		out[key] = toFloat(r["value"])
	}
	return out
}

func renamed(p writer.MetricPoint, name string) writer.MetricPoint {
	p.MetricName = name
	return p
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestHistogramCountAndSum(t *testing.T) {
	c := newHistogramCore(t)
	if got := metricValues(t, c, "http.server.duration", "count", "")[""]; got != 40 {
		t.Errorf("count = %v, want 40 (36 on /a + 4 on /b)", got)
	}
	if got := metricValues(t, c, "http.server.duration", "sum", "")[""]; got != 406 {
		t.Errorf("sum = %v, want 406", got)
	}
}

func TestHistogramGroupedByRoute(t *testing.T) {
	c := newHistogramCore(t)
	counts := metricValues(t, c, "http.server.duration", "count", "attr.http.route")
	if counts["/a"] != 36 || counts["/b"] != 4 {
		t.Errorf("counts = %v, want /a=36 /b=4", counts)
	}
	avg := metricValues(t, c, "http.server.duration", "avg", "attr.http.route")
	if !near(avg["/a"], 350.0/36) || !near(avg["/b"], 14) {
		t.Errorf("avg = %v, want /a=%v /b=14", avg, 350.0/36)
	}
}

func TestHistogramPercentiles(t *testing.T) {
	c := newHistogramCore(t)
	p50 := metricValues(t, c, "http.server.duration", "p50", "attr.http.route")
	// /b: 4 observations, rank 2 falls in [0,10) holding 3 -> 10*2/3.
	if !near(p50["/b"], 20.0/3) {
		t.Errorf("p50 /b = %v, want %v", p50["/b"], 20.0/3)
	}
	// /a: buckets [15,16,5], rank 18 falls in [10,100) -> 10 + 90*3/16.
	if !near(p50["/a"], 10+90*3.0/16) {
		t.Errorf("p50 /a = %v, want %v", p50["/a"], 10+90*3.0/16)
	}
	// p99 of /a lands in the +Inf bucket: the highest finite bound.
	if got := metricValues(t, c, "http.server.duration", "p99", "attr.http.route")["/a"]; got != 100 {
		t.Errorf("p99 /a = %v, want 100", got)
	}
	// Ungrouped: buckets [18,17,5], rank 20 in [10,100) -> 10 + 90*2/17.
	if got := metricValues(t, c, "http.server.duration", "p50", "")[""]; !near(got, 10+90*2.0/17) {
		t.Errorf("p50 = %v, want %v", got, 10+90*2.0/17)
	}
}

func TestHistogramResetWithoutNewStart(t *testing.T) {
	c := newHistogramCore(t)
	if got := metricValues(t, c, "reset.duration", "count", "")[""]; got != 2 {
		t.Errorf("count = %v, want 2", got)
	}
}

func TestHistogramMinMax(t *testing.T) {
	c := newHistogramCore(t)
	if got := metricValues(t, c, "http.server.duration", "max", "")[""]; got != 500 {
		t.Errorf("max = %v, want 500", got)
	}
	if got := metricValues(t, c, "http.server.duration", "min", "")[""]; got != 1 {
		t.Errorf("min = %v, want 1", got)
	}
}

func TestExponentialHistogram(t *testing.T) {
	c := newHistogramCore(t)
	if got := metricValues(t, c, "exp.duration", "count", "")[""]; got != 7 {
		t.Errorf("count = %v, want 7", got)
	}
	if _, _, err := c.QueryMetric("acme", "exp.duration", "p95", 60, "", nil); !IsInvalidInput(err) {
		t.Errorf("p95 on an exponential histogram: err = %v, want invalid input", err)
	}
}

// TestHistogramOlderThanProbeWindow pins the metric_type probe fallback: the
// recent probe window (the last hour) finds nothing, so the full requested
// window must still decide that this is a histogram. Otherwise count would
// fall through to count(*) of points (1) instead of observations (7).
func TestHistogramOlderThanProbeWindow(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	now := time.Now()
	w.AddMetrics([]writer.MetricPoint{
		histPoint("/old", deltaTemporality, now.Add(-3*time.Hour), now.Add(-90*time.Minute), 7, 70, "[7,0,0]"),
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	c := NewCore(engine)

	rows, _, err := c.QueryMetric("acme", "http.server.duration", "count", 180, "", nil)
	if err != nil {
		t.Fatalf("count over a 3h window: %v", err)
	}
	if len(rows) != 1 || toFloat(rows[0]["value"]) != 7 {
		t.Errorf("count = %v, want one row with 7 (the old histogram was aggregated as a gauge)", rows)
	}
}

func TestHistogramOtherTenantSeesNothing(t *testing.T) {
	c := newHistogramCore(t)
	rows, _, err := c.QueryMetric("globex", "http.server.duration", "count", 60, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if v := toFloat(r["value"]); v != 0 {
			t.Errorf("globex count = %v, want nothing", v)
		}
	}
}

func TestBucketQuantile(t *testing.T) {
	inf := math.Inf(1)
	bs := []bucket{{math.Inf(-1), 10, 0}, {10, 100, 10}, {100, inf, 0}}
	if v, ok := bucketQuantile(0.5, bs); !ok || v != 55 {
		t.Errorf("p50 = %v %v, want 55", v, ok)
	}
	if _, ok := bucketQuantile(0.5, []bucket{{10, 100, 0}}); ok {
		t.Error("empty histogram should have no quantile")
	}
	if v, _ := bucketQuantile(0.9, []bucket{{math.Inf(-1), 10, 1}, {10, inf, 9}}); v != 10 {
		t.Errorf("+Inf bucket = %v, want the highest finite bound 10", v)
	}
}

// TestHistogramBuckets: aggregation=buckets returns the distribution itself,
// per group, with le null for the bucket above the last bound.
func TestHistogramBuckets(t *testing.T) {
	c := newHistogramCore(t)
	rows, cols, err := c.QueryMetric("acme", "http.server.duration", "buckets", 60, "attr.http.route", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 3 || cols[0] != "attr.http.route" || cols[1] != "le" || cols[2] != "value" {
		t.Fatalf("columns = %v", cols)
	}
	got := map[string][]float64{}
	var lastLe []any
	for _, r := range rows {
		route := r["attr.http.route"].(string)
		got[route] = append(got[route], toFloat(r["value"]))
		if route == "/a" {
			lastLe = append(lastLe, r["le"])
		}
	}
	want := map[string][]float64{"/a": {15, 16, 5}, "/b": {3, 1, 0}}
	for route, w := range want {
		g := got[route]
		if len(g) != len(w) || g[0] != w[0] || g[1] != w[1] || g[2] != w[2] {
			t.Errorf("%s buckets = %v, want %v", route, g, w)
		}
	}
	if len(lastLe) != 3 || toFloat(lastLe[0]) != 10 || toFloat(lastLe[1]) != 100 || lastLe[2] != nil {
		t.Errorf("/a le = %v, want [10 100 <nil>]", lastLe)
	}

	// Filtered to one group, as the dashboard does when a bar is clicked.
	rows, _, err = c.QueryMetric("acme", "http.server.duration", "buckets", 60, "",
		[]AttrFilter{{Key: "http.route", Value: "/b"}})
	if err != nil || len(rows) != 3 || toFloat(rows[0]["value"]) != 3 {
		t.Errorf("filtered buckets = %v, %v", rows, err)
	}
}

func TestBucketsOnNonHistogramIsInvalidInput(t *testing.T) {
	c := newHistogramCore(t)
	if _, _, err := c.QueryMetric("acme", "exp.duration", "buckets", 60, "", nil); !IsInvalidInput(err) {
		t.Errorf("exponential histogram: err = %v, want invalid input", err)
	}
}

// TestMetricTypeAndUnitViaGroupBy: the dashboard learns a metric's type and
// unit through the same metric_query an agent uses.
func TestMetricTypeAndUnitViaGroupBy(t *testing.T) {
	c := newHistogramCore(t)
	rows, _, err := c.QueryMetric("acme", "http.server.duration", "count", 60, "metric_type,metric_unit", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["metric_type"] != "histogram" || rows[0]["metric_unit"] != "ms" || toFloat(rows[0]["value"]) != 40 {
		t.Errorf("rows = %v, want one histogram/ms row with count 40", rows)
	}
}
