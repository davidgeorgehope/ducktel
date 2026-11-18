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
	rows, _, err := c.QueryMetric("acme", metric, agg, 60, 0, groupBy, nil)
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
	if _, _, err := c.QueryMetric("acme", "exp.duration", "p95", 60, 0, "", nil); !IsInvalidInput(err) {
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

	rows, _, err := c.QueryMetric("acme", "http.server.duration", "count", 180, 0, "", nil)
	if err != nil {
		t.Fatalf("count over a 3h window: %v", err)
	}
	if len(rows) != 1 || toFloat(rows[0]["value"]) != 7 {
		t.Errorf("count = %v, want one row with 7 (the old histogram was aggregated as a gauge)", rows)
	}
}

func TestHistogramOtherTenantSeesNothing(t *testing.T) {
	c := newHistogramCore(t)
	rows, _, err := c.QueryMetric("globex", "http.server.duration", "count", 60, 0, "", nil)
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
	rows, cols, err := c.QueryMetric("acme", "http.server.duration", "buckets", 60, 0, "attr.http.route", nil)
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
	rows, _, err = c.QueryMetric("acme", "http.server.duration", "buckets", 60, 0, "",
		[]AttrFilter{{Key: "http.route", Value: "/b"}})
	if err != nil || len(rows) != 3 || toFloat(rows[0]["value"]) != 3 {
		t.Errorf("filtered buckets = %v, %v", rows, err)
	}
}

func TestBucketsOnNonHistogramIsInvalidInput(t *testing.T) {
	c := newHistogramCore(t)
	if _, _, err := c.QueryMetric("acme", "exp.duration", "buckets", 60, 0, "", nil); !IsInvalidInput(err) {
		t.Errorf("exponential histogram: err = %v, want invalid input", err)
	}
}

// TestMetricTypeAndUnitViaGroupBy: the dashboard learns a metric's type and
// unit through the same metric_query an agent uses.
func TestMetricTypeAndUnitViaGroupBy(t *testing.T) {
	c := newHistogramCore(t)
	rows, _, err := c.QueryMetric("acme", "http.server.duration", "count", 60, 0, "metric_type,metric_unit", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["metric_type"] != "histogram" || rows[0]["metric_unit"] != "ms" || toFloat(rows[0]["value"]) != 40 {
		t.Errorf("rows = %v, want one histogram/ms row with count 40", rows)
	}
}

// TestHistogramMixedTemporalities exercises delta and cumulative series in one
// metric, including a cumulative counter reset and a cumulative series that
// started inside the window. Expectations are computed by hand from the
// documented rules: a baseline contributes nothing, a reset is counted whole,
// an in-window start is counted whole, and delta points are already increments.
func TestHistogramMixedTemporalities(t *testing.T) {
	base := stepBase()
	cumStart := base.Add(-time.Hour)
	hist := func(route, temp string, start, ts time.Time, count uint64, sum float64, buckets string) writer.MetricPoint {
		p := stepHistPoint("mixed.duration", temp, start, ts, count, sum, buckets)
		p.Attributes = `{"route":"` + route + `"}`
		return p
	}
	c := newStepCore(t, []writer.MetricPoint{
		// Cumulative, started before the window: first point is the baseline.
		hist("/cum", temporalityCumulative, cumStart, base, 90, 900, "[50,40,0]"),
		hist("/cum", temporalityCumulative, cumStart, base.Add(time.Minute), 100, 1000, "[55,45,0]"),
		hist("/cum", temporalityCumulative, cumStart, base.Add(2*time.Minute), 120, 1200, "[60,60,0]"),
		// A reset: the count drops and 4 is counted whole.
		hist("/cum", temporalityCumulative, cumStart, base.Add(3*time.Minute), 4, 20, "[4,0,0]"),
		// Cumulative, started inside the window: the first point counts whole.
		hist("/cumnew", temporalityCumulative, base, base, 7, 70, "[2,5,0]"),
		hist("/cumnew", temporalityCumulative, base, base.Add(2*time.Minute), 9, 90, "[3,6,0]"),
		// Delta points are already increments.
		hist("/delta", deltaTemporality, base, base, 3, 30, "[1,2,0]"),
		hist("/delta", deltaTemporality, base, base.Add(time.Minute), 1, 10, "[0,1,0]"),
	})

	// Without step, grouped by the series' route.
	byRoute := func(metric, agg, groupBy string) map[string]float64 {
		t.Helper()
		rows, _, err := c.QueryMetricWith("acme", metric, agg, 60, groupBy, nil, MetricOpts{})
		if err != nil {
			t.Fatalf("%s %s: %v", metric, agg, err)
		}
		out := map[string]float64{}
		for _, r := range rows {
			key := ""
			if groupBy != "" {
				key = r[groupBy].(string)
			}
			out[key] = toFloat(r["value"])
		}
		return out
	}
	check := func(what string, got, want map[string]float64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
		for k, w := range want {
			if g, ok := got[k]; !ok || !near(g, w) {
				t.Errorf("%s[%q] = %v (present %v), want %v", what, k, g, ok, w)
			}
		}
	}

	check("count", byRoute("mixed.duration", "count", "attr.route"),
		map[string]float64{"/cum": 34, "/cumnew": 9, "/delta": 4})
	check("sum", byRoute("mixed.duration", "sum", "attr.route"),
		map[string]float64{"/cum": 320, "/cumnew": 90, "/delta": 40})
	check("p95", byRoute("mixed.duration", "p95", "attr.route"),
		map[string]float64{
			"/cum":    10 + 90*(0.95*34-14)/20,
			"/cumnew": 10 + 90*(0.95*9-3)/6,
			"/delta":  10 + 90*(0.95*4-1)/3,
		})
	check("count ungrouped", byRoute("mixed.duration", "count", ""), map[string]float64{"": 47})
	check("p95 ungrouped", byRoute("mixed.duration", "p95", ""),
		map[string]float64{"": 10 + 90*(0.95*47-18)/29})

	// Buckets, without step: one row per group and upper bound. le is null for
	// the +Inf bucket, so it is keyed by +Inf here.
	bucketMap := func(rows []map[string]any, groupBy string) map[string]map[float64]float64 {
		t.Helper()
		out := map[string]map[float64]float64{}
		for _, r := range rows {
			g := ""
			if groupBy != "" {
				g = r[groupBy].(string)
			}
			if out[g] == nil {
				out[g] = map[float64]float64{}
			}
			le := math.Inf(1)
			if v, ok := r["le"]; ok && v != nil {
				le = toFloat(v)
			}
			out[g][le] = toFloat(r["value"])
		}
		return out
	}
	brows, bcols, err := c.QueryMetricWith("acme", "mixed.duration", "buckets", 60, "attr.route", nil, MetricOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(bcols) != 3 || bcols[0] != "attr.route" || bcols[1] != "le" || bcols[2] != "value" {
		t.Fatalf("bucket columns = %v", bcols)
	}
	wantBuckets := map[string]map[float64]float64{
		"/cum":    {10: 14, 100: 20, math.Inf(1): 0},
		"/cumnew": {10: 3, 100: 6, math.Inf(1): 0},
		"/delta":  {10: 1, 100: 3, math.Inf(1): 0},
	}
	gotBuckets := bucketMap(brows, "attr.route")
	if len(gotBuckets) != len(wantBuckets) {
		t.Fatalf("bucket groups = %v, want %v", gotBuckets, wantBuckets)
	}
	for g, want := range wantBuckets {
		for hi, w := range want {
			if got, ok := gotBuckets[g][hi]; !ok || got != w {
				t.Errorf("buckets[%q][%v] = %v (present %v), want %v", g, hi, got, ok, w)
			}
		}
	}

	// With step, one row per time bucket and group. A baseline is still a
	// zero-count row for count, but contributes no bucket row (its increment
	// list is null).
	type stepKey struct {
		ts    int64
		route string
	}
	stepMap := func(agg string) map[stepKey]float64 {
		t.Helper()
		rows, _, err := c.QueryMetricWith("acme", "mixed.duration", agg, 60, "attr.route", nil, MetricOpts{Step: "1m"})
		if err != nil {
			t.Fatalf("%s with step: %v", agg, err)
		}
		out := map[stepKey]float64{}
		for _, r := range rows {
			out[stepKey{asInt64(t, r["time"]), r["attr.route"].(string)}] = toFloat(r["value"])
		}
		return out
	}
	wantStepCount := map[stepKey]float64{
		{base.UnixMicro(), "/cum"}:                         0,
		{base.UnixMicro(), "/cumnew"}:                      7,
		{base.UnixMicro(), "/delta"}:                       3,
		{base.Add(time.Minute).UnixMicro(), "/cum"}:        10,
		{base.Add(time.Minute).UnixMicro(), "/delta"}:      1,
		{base.Add(2 * time.Minute).UnixMicro(), "/cum"}:    20,
		{base.Add(2 * time.Minute).UnixMicro(), "/cumnew"}: 2,
		{base.Add(3 * time.Minute).UnixMicro(), "/cum"}:    4,
	}
	gotStepCount := stepMap("count")
	if len(gotStepCount) != len(wantStepCount) {
		t.Fatalf("step count rows = %v, want %v", gotStepCount, wantStepCount)
	}
	for k, w := range wantStepCount {
		if got, ok := gotStepCount[k]; !ok || got != w {
			t.Errorf("step count[%v] = %v (present %v), want %v", k, got, ok, w)
		}
	}

	wantStepP95 := map[stepKey]float64{
		{base.UnixMicro(), "/cumnew"}:                      10 + 90*(0.95*7-2)/5,
		{base.UnixMicro(), "/delta"}:                       10 + 90*(0.95*3-1)/2,
		{base.Add(time.Minute).UnixMicro(), "/cum"}:        10 + 90*(0.95*10-5)/5,
		{base.Add(time.Minute).UnixMicro(), "/delta"}:      10 + 90*(0.95*1-0)/1,
		{base.Add(2 * time.Minute).UnixMicro(), "/cum"}:    10 + 90*(0.95*20-5)/15,
		{base.Add(2 * time.Minute).UnixMicro(), "/cumnew"}: 10 + 90*(0.95*2-1)/1,
		{base.Add(3 * time.Minute).UnixMicro(), "/cum"}:    10 * (0.95 * 4) / 4,
	}
	gotStepP95 := stepMap("p95")
	if len(gotStepP95) != len(wantStepP95) {
		t.Fatalf("step p95 rows = %v, want %v", gotStepP95, wantStepP95)
	}
	for k, w := range wantStepP95 {
		if got, ok := gotStepP95[k]; !ok || !near(got, w) {
			t.Errorf("step p95[%v] = %v (present %v), want %v", k, got, ok, w)
		}
	}

	wantStepBuckets := map[stepKey]map[float64]float64{
		{base.UnixMicro(), "/cumnew"}:                      {10: 2, 100: 5, math.Inf(1): 0},
		{base.UnixMicro(), "/delta"}:                       {10: 1, 100: 2, math.Inf(1): 0},
		{base.Add(time.Minute).UnixMicro(), "/cum"}:        {10: 5, 100: 5, math.Inf(1): 0},
		{base.Add(time.Minute).UnixMicro(), "/delta"}:      {10: 0, 100: 1, math.Inf(1): 0},
		{base.Add(2 * time.Minute).UnixMicro(), "/cum"}:    {10: 5, 100: 15, math.Inf(1): 0},
		{base.Add(2 * time.Minute).UnixMicro(), "/cumnew"}: {10: 1, 100: 1, math.Inf(1): 0},
		{base.Add(3 * time.Minute).UnixMicro(), "/cum"}:    {10: 4, 100: 0, math.Inf(1): 0},
	}
	stepBucketMap := func() map[stepKey]map[float64]float64 {
		t.Helper()
		rows, _, err := c.QueryMetricWith("acme", "mixed.duration", "buckets", 60, "attr.route", nil, MetricOpts{Step: "1m"})
		if err != nil {
			t.Fatalf("buckets with step: %v", err)
		}
		out := map[stepKey]map[float64]float64{}
		for _, r := range rows {
			k := stepKey{asInt64(t, r["time"]), r["attr.route"].(string)}
			if out[k] == nil {
				out[k] = map[float64]float64{}
			}
			le := math.Inf(1)
			if v, ok := r["le"]; ok && v != nil {
				le = toFloat(v)
			}
			out[k][le] = toFloat(r["value"])
		}
		return out
	}
	gotStepBuckets := stepBucketMap()
	if len(gotStepBuckets) != len(wantStepBuckets) {
		t.Fatalf("step bucket groups = %v, want %v", gotStepBuckets, wantStepBuckets)
	}
	for k, want := range wantStepBuckets {
		for hi, w := range want {
			if got, ok := gotStepBuckets[k][hi]; !ok || got != w {
				t.Errorf("step buckets[%v][%v] = %v (present %v), want %v", k, hi, got, ok, w)
			}
		}
	}
}

// TestHistogramMixedTemporalitiesUngroupedStep shares one group between every
// series: the step path merges bucket lists across series within a (time,
// group, bounds), so this pins that merging as well as the per-series one.
func TestHistogramMixedTemporalitiesUngroupedStep(t *testing.T) {
	base := stepBase()
	cumStart := base.Add(-time.Hour)
	hist := func(route, temp string, start, ts time.Time, count uint64, sum float64, buckets string) writer.MetricPoint {
		p := stepHistPoint("mixed.duration", temp, start, ts, count, sum, buckets)
		p.Attributes = `{"route":"` + route + `"}`
		return p
	}
	c := newStepCore(t, []writer.MetricPoint{
		hist("/cum", temporalityCumulative, cumStart, base, 90, 900, "[50,40,0]"),
		hist("/cum", temporalityCumulative, cumStart, base.Add(time.Minute), 100, 1000, "[55,45,0]"),
		hist("/cum", temporalityCumulative, cumStart, base.Add(2*time.Minute), 120, 1200, "[60,60,0]"),
		hist("/cum", temporalityCumulative, cumStart, base.Add(3*time.Minute), 4, 20, "[4,0,0]"),
		hist("/cumnew", temporalityCumulative, base, base, 7, 70, "[2,5,0]"),
		hist("/cumnew", temporalityCumulative, base, base.Add(2*time.Minute), 9, 90, "[3,6,0]"),
		hist("/delta", deltaTemporality, base, base, 3, 30, "[1,2,0]"),
		hist("/delta", deltaTemporality, base, base.Add(time.Minute), 1, 10, "[0,1,0]"),
	})

	minutes := []int64{
		base.UnixMicro(),
		base.Add(time.Minute).UnixMicro(),
		base.Add(2 * time.Minute).UnixMicro(),
		base.Add(3 * time.Minute).UnixMicro(),
	}

	values := func(agg string) map[int64]float64 {
		t.Helper()
		rows, _, err := c.QueryMetricWith("acme", "mixed.duration", agg, 60, "", nil, MetricOpts{Step: "1m"})
		if err != nil {
			t.Fatalf("%s ungrouped step: %v", agg, err)
		}
		out := map[int64]float64{}
		for _, r := range rows {
			out[asInt64(t, r["time"])] = toFloat(r["value"])
		}
		return out
	}

	wantCounts := []float64{10, 11, 22, 4}
	counts := values("count")
	if len(counts) != len(minutes) {
		t.Fatalf("ungrouped step count rows = %v, want %d", counts, len(minutes))
	}
	for i, ts := range minutes {
		if got := counts[ts]; got != wantCounts[i] {
			t.Errorf("ungrouped step count[%d] = %v, want %v", i, got, wantCounts[i])
		}
	}

	wantP95 := []float64{
		10 + 90*(0.95*10-3)/7,
		10 + 90*(0.95*11-5)/6,
		10 + 90*(0.95*22-6)/16,
		10 * (0.95 * 4) / 4,
	}
	p95 := values("p95")
	if len(p95) != len(minutes) {
		t.Fatalf("ungrouped step p95 rows = %v, want %d", p95, len(minutes))
	}
	for i, ts := range minutes {
		if got := p95[ts]; !near(got, wantP95[i]) {
			t.Errorf("ungrouped step p95[%d] = %v, want %v", i, got, wantP95[i])
		}
	}

	brows, _, err := c.QueryMetricWith("acme", "mixed.duration", "buckets", 60, "", nil, MetricOpts{Step: "1m"})
	if err != nil {
		t.Fatalf("buckets ungrouped step: %v", err)
	}
	gotBuckets := map[int64]map[float64]float64{}
	for _, r := range brows {
		ts := asInt64(t, r["time"])
		if gotBuckets[ts] == nil {
			gotBuckets[ts] = map[float64]float64{}
		}
		le := math.Inf(1)
		if v, ok := r["le"]; ok && v != nil {
			le = toFloat(v)
		}
		gotBuckets[ts][le] = toFloat(r["value"])
	}
	wantBuckets := []map[float64]float64{
		{10: 3, 100: 7, math.Inf(1): 0},
		{10: 5, 100: 6, math.Inf(1): 0},
		{10: 6, 100: 16, math.Inf(1): 0},
		{10: 4, 100: 0, math.Inf(1): 0},
	}
	if len(gotBuckets) != len(minutes) {
		t.Fatalf("ungrouped step bucket rows = %v, want %d", gotBuckets, len(minutes))
	}
	for i, ts := range minutes {
		for hi, w := range wantBuckets[i] {
			if got, ok := gotBuckets[ts][hi]; !ok || got != w {
				t.Errorf("ungrouped step buckets[%d][%v] = %v (present %v), want %v", i, hi, got, ok, w)
			}
		}
	}
}
