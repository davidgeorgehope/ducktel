package telemetry

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// stepBase is a minute boundary inside the default window, so expected bucket
// starts are exact. Tests place points on or after it within their minutes.
func stepBase() time.Time {
	return time.Now().Add(-5 * time.Minute).Truncate(time.Minute)
}

func newStepCore(t *testing.T, points []writer.MetricPoint) *Core {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.AddMetrics(points)
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

func gaugePoint(metric string, ts time.Time, v float64, resource string) writer.MetricPoint {
	return writer.MetricPoint{
		MetricName: metric, MetricType: "gauge", Timestamp: ts.UnixMicro(),
		ValueDouble: v, ResourceAttributes: resource,
	}
}

func stepHistPoint(metric, temp string, start, ts time.Time, count uint64, sum float64, buckets string) writer.MetricPoint {
	return writer.MetricPoint{
		MetricName: metric, MetricType: "histogram", MetricUnit: "ms",
		Timestamp: ts.UnixMicro(), StartTimestamp: start.UnixMicro(),
		Count: count, Sum: sum, Min: 1, Max: 500,
		BucketCounts: buckets, ExplicitBounds: "[10,100]",
		AggregationTemporality: temp,
		Attributes:             "{}",
		ResourceAttributes:     `{"service.name":"web","tenant.id":"acme"}`,
		ServiceName:            "web",
	}
}

func asInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	default:
		t.Fatalf("value %v is not an integer (%T)", v, v)
		return 0
	}
}

func TestGaugeStepReturnsOneRowPerMinute(t *testing.T) {
	base := stepBase()
	c := newStepCore(t, []writer.MetricPoint{
		gaugePoint("cpu.step", base, 10, `{"tenant.id":"acme"}`),
		gaugePoint("cpu.step", base.Add(20*time.Second), 20, `{"tenant.id":"acme"}`),
		gaugePoint("cpu.step", base.Add(time.Minute), 30, `{"tenant.id":"acme"}`),
		gaugePoint("cpu.step", base.Add(2*time.Minute), 40, `{"tenant.id":"acme"}`),
		gaugePoint("cpu.step", base.Add(2*time.Minute+10*time.Second), 50, `{"tenant.id":"acme"}`),
		gaugePoint("cpu.step", base, 999, `{"tenant.id":"globex"}`),
	})

	rows, cols, err := c.QueryMetricWith("acme", "cpu.step", "avg", 60, "", nil, MetricOpts{Step: "1m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cols) != 2 || cols[0] != "time" || cols[1] != "value" {
		t.Fatalf("columns = %v, want [time value]", cols)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 minutes: %v", len(rows), rows)
	}
	wantTimes := []int64{base.UnixMicro(), base.Add(time.Minute).UnixMicro(), base.Add(2 * time.Minute).UnixMicro()}
	wantValues := []float64{15, 30, 45}
	for i, r := range rows {
		if got := asInt64(t, r["time"]); got != wantTimes[i] {
			t.Errorf("row %d time = %d, want %d", i, got, wantTimes[i])
		}
		if got := asFloat64(t, r["value"]); got != wantValues[i] {
			t.Errorf("row %d value = %v, want %v", i, got, wantValues[i])
		}
	}
}

func TestGaugeStepGroupedByResourceAttribute(t *testing.T) {
	base := stepBase()
	c := newStepCore(t, []writer.MetricPoint{
		gaugePoint("cpu.host", base, 10, `{"host.name":"host-a","tenant.id":"acme"}`),
		gaugePoint("cpu.host", base, 30, `{"host.name":"host-b","tenant.id":"acme"}`),
		gaugePoint("cpu.host", base.Add(time.Minute), 20, `{"host.name":"host-a","tenant.id":"acme"}`),
		gaugePoint("cpu.host", base.Add(time.Minute), 40, `{"host.name":"host-b","tenant.id":"acme"}`),
		gaugePoint("cpu.host", base, 999, `{"host.name":"host-a","tenant.id":"globex"}`),
	})

	rows, cols, err := c.QueryMetricWith("acme", "cpu.host", "avg", 60, "resource.host.name", nil, MetricOpts{Step: "1m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cols) != 3 || cols[0] != "time" || cols[1] != "resource.host.name" || cols[2] != "value" {
		t.Fatalf("columns = %v, want [time resource.host.name value]", cols)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 (2 minutes x 2 hosts): %v", len(rows), rows)
	}
	type pair struct {
		host string
		val  float64
	}
	want := []pair{{"host-a", 10}, {"host-b", 30}, {"host-a", 20}, {"host-b", 40}}
	for i, r := range rows {
		if got := r["resource.host.name"]; got != want[i].host {
			t.Errorf("row %d resource.host.name = %v, want %s", i, got, want[i].host)
		}
		if got := asFloat64(t, r["value"]); got != want[i].val {
			t.Errorf("row %d value = %v, want %v", i, got, want[i].val)
		}
	}
	if got := asInt64(t, rows[0]["time"]); got != base.UnixMicro() {
		t.Errorf("first row time = %d, want %d", got, base.UnixMicro())
	}

	// The long form is the same grouping, with the key as given as the column.
	rows, cols, err = c.QueryMetricWith("acme", "cpu.host", "avg", 60, "resource_attributes.host.name", nil, MetricOpts{Step: "1m"})
	if err != nil {
		t.Fatalf("long-form resource group: %v", err)
	}
	if len(cols) != 3 || cols[1] != "resource_attributes.host.name" || len(rows) != 4 {
		t.Fatalf("long form columns=%v rows=%d", cols, len(rows))
	}
}

func TestDeltaHistogramStepP95CountAndBuckets(t *testing.T) {
	base := stepBase()
	points := []writer.MetricPoint{
		stepHistPoint("req.duration", deltaTemporality, base.Add(-time.Minute), base, 4, 20, "[4,0,0]"),
		stepHistPoint("req.duration", deltaTemporality, base, base.Add(time.Minute), 2, 200, "[0,2,0]"),
		stepHistPoint("req.duration", deltaTemporality, base.Add(time.Minute), base.Add(2*time.Minute), 1, 150, "[0,0,1]"),
	}
	c := newStepCore(t, points)

	rows, cols, err := c.QueryMetricWith("acme", "req.duration", "p95", 60, "", nil, MetricOpts{Step: "1m"})
	if err != nil {
		t.Fatalf("p95: %v", err)
	}
	if len(cols) != 2 || cols[0] != "time" || cols[1] != "value" {
		t.Fatalf("p95 columns = %v", cols)
	}
	wantP95 := []float64{9.5, 95.5, 100}
	if len(rows) != len(wantP95) {
		t.Fatalf("p95 rows = %v", rows)
	}
	for i, r := range rows {
		if got := asFloat64(t, r["value"]); !near(got, wantP95[i]) {
			t.Errorf("p95 row %d = %v, want %v", i, got, wantP95[i])
		}
	}

	counts, _, err := c.QueryMetricWith("acme", "req.duration", "count", 60, "", nil, MetricOpts{Step: "1m"})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	wantCounts := []float64{4, 2, 1}
	for i, r := range counts {
		if got := asFloat64(t, r["value"]); got != wantCounts[i] {
			t.Errorf("count row %d = %v, want %v", i, got, wantCounts[i])
		}
	}

	brows, bcols, err := c.QueryMetricWith("acme", "req.duration", "buckets", 60, "", nil, MetricOpts{Step: "1m"})
	if err != nil {
		t.Fatalf("buckets: %v", err)
	}
	if len(bcols) != 3 || bcols[0] != "time" || bcols[1] != "le" || bcols[2] != "value" {
		t.Fatalf("buckets columns = %v, want [time le value]", bcols)
	}
	if len(brows) != 9 {
		t.Fatalf("buckets rows = %d, want 9 (3 minutes x 3 buckets): %v", len(brows), brows)
	}
	wantBuckets := [][]float64{{4, 0, 0}, {0, 2, 0}, {0, 0, 1}}
	for i, r := range brows {
		minute, bucket := i/3, i%3
		if got := asInt64(t, r["time"]); got != base.Add(time.Duration(minute)*time.Minute).UnixMicro() {
			t.Errorf("bucket row %d time = %d", i, got)
		}
		if got := asFloat64(t, r["value"]); got != wantBuckets[minute][bucket] {
			t.Errorf("bucket row %d value = %v, want %v", i, got, wantBuckets[minute][bucket])
		}
	}
}

func TestCumulativeHistogramStepCountsIncrements(t *testing.T) {
	base := stepBase()
	start := base.Add(-time.Hour)
	points := []writer.MetricPoint{
		stepHistPoint("cum.duration", temporalityCumulative, start, base, 100, 1000, "[50,40,10]"),
		stepHistPoint("cum.duration", temporalityCumulative, start, base.Add(time.Minute), 110, 1100, "[55,44,11]"),
		stepHistPoint("cum.duration", temporalityCumulative, start, base.Add(2*time.Minute), 130, 1300, "[60,55,15]"),
	}
	c := newStepCore(t, points)

	// The whole window still reports the same total increments as before.
	whole, _, err := c.QueryMetric("acme", "cum.duration", "count", 60, 0, "", nil)
	if err != nil {
		t.Fatalf("count without step: %v", err)
	}
	if len(whole) != 1 || asFloat64(t, whole[0]["value"]) != 30 {
		t.Fatalf("whole-window count = %v, want 30", whole)
	}

	rows, _, err := c.QueryMetricWith("acme", "cum.duration", "count", 60, "", nil, MetricOpts{Step: "1m"})
	if err != nil {
		t.Fatalf("count with step: %v", err)
	}
	// The first point is the baseline (the series started before the window),
	// so the first bucket holds no increment; the later points contribute
	// 10 and 20 rather than their running totals 110 and 130.
	want := []float64{0, 10, 20}
	if len(rows) != len(want) {
		t.Fatalf("count rows = %v", rows)
	}
	for i, r := range rows {
		if got := asFloat64(t, r["value"]); got != want[i] {
			t.Errorf("count row %d = %v, want %v", i, got, want[i])
		}
	}

	sums, _, err := c.QueryMetricWith("acme", "cum.duration", "sum", 60, "", nil, MetricOpts{Step: "1m"})
	if err != nil {
		t.Fatalf("sum with step: %v", err)
	}
	wantSums := []float64{0, 100, 200}
	for i, r := range sums {
		if got := asFloat64(t, r["value"]); got != wantSums[i] {
			t.Errorf("sum row %d = %v, want %v", i, got, wantSums[i])
		}
	}
}

func TestMetricStepValidation(t *testing.T) {
	c := NewCore(nil)

	for _, step := range []string{"0", "0s", "500ms", "nope", "1"} {
		_, _, err := c.QueryMetricWith("acme", "m", "avg", 60, "", nil, MetricOpts{Step: step})
		if err == nil {
			t.Errorf("step %q should be rejected", step)
			continue
		}
		if !IsInvalidInput(err) {
			t.Errorf("step %q rejection should classify as invalid input: %v", step, err)
		}
	}

	// 60 minutes at 1s would be 3600 buckets; the error must name the
	// smallest step that fits.
	_, _, err := c.QueryMetricWith("acme", "m", "avg", 60, "", nil, MetricOpts{Step: "1s"})
	if !IsInvalidInput(err) {
		t.Fatalf("too many buckets: err = %v, want invalid input", err)
	}
	if !strings.Contains(err.Error(), "1500") || !strings.Contains(err.Error(), "smallest allowed step") ||
		!strings.Contains(err.Error(), "2.4s") {
		t.Errorf("bucket guard error does not name the limit and smallest step: %v", err)
	}

	for _, key := range []string{"resource.host name", "resource.", `resource."host"`, "resource.host' OR '1'='1"} {
		_, _, err := c.QueryMetricWith("acme", "m", "count", 60, key, nil, MetricOpts{})
		if err == nil {
			t.Errorf("group_by %q should be rejected", key)
			continue
		}
		if !IsInvalidInput(err) {
			t.Errorf("group_by %q rejection should classify as invalid input: %v", key, err)
		}
	}
}

func TestExemplarsIgnoreStep(t *testing.T) {
	now := time.Now()
	exemplar := `[{"time_unix_nano":` + strconv.FormatInt(now.UnixNano(), 10) + `,"trace_id":"trace-1","span_id":"span-1","value":42}]`
	c := newStepCore(t, []writer.MetricPoint{{
		MetricName: "ex.step", MetricType: "histogram",
		Timestamp: now.UnixMicro(), StartTimestamp: now.Add(-time.Minute).UnixMicro(),
		Count: 1, Sum: 42, Min: 42, Max: 42, BucketCounts: "[0,1]", ExplicitBounds: "[10]",
		AggregationTemporality: deltaTemporality, Attributes: "{}",
		ResourceAttributes: `{"tenant.id":"acme"}`, Exemplars: exemplar,
	}})

	rows, cols, err := c.QueryMetricWith("acme", "ex.step", "exemplars", 60, "", nil, MetricOpts{Step: "1m"})
	if err != nil {
		t.Fatalf("exemplars with step: %v", err)
	}
	if len(cols) != 4 || cols[0] != "timestamp" {
		t.Fatalf("columns = %v, want the flat exemplar columns without time", cols)
	}
	if len(rows) != 1 || rows[0]["trace_id"] != "trace-1" {
		t.Fatalf("rows = %v, want the single exemplar", rows)
	}
}
