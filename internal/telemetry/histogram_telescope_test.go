package telemetry

import (
	"math"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// These tests pin the telescoping segment rewrite against hand-computed
// increments, including the cases the rewrite is easiest to get wrong: a
// reset inside one step bucket, the first point of an in-window series, a
// bucket list length change, and a series with a single point.

type histStepKey struct {
	ts    int64
	group string
}

func histStepValues(t *testing.T, c *Core, metric, agg, groupBy string) map[histStepKey]float64 {
	t.Helper()
	rows, _, err := c.QueryMetricWith("acme", metric, agg, 60, groupBy, nil, MetricOpts{Step: "1m"})
	if err != nil {
		t.Fatalf("%s %s with step: %v", metric, agg, err)
	}
	out := map[histStepKey]float64{}
	for _, r := range rows {
		g := ""
		if groupBy != "" {
			g, _ = r[groupBy].(string)
		}
		out[histStepKey{asInt64(t, r["time"]), g}] = toFloat(r["value"])
	}
	return out
}

func histGroupValues(t *testing.T, c *Core, metric, agg, groupBy string) map[string]float64 {
	t.Helper()
	rows, _, err := c.QueryMetric("acme", metric, agg, 60, groupBy, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", metric, agg, err)
	}
	out := map[string]float64{}
	for _, r := range rows {
		g := ""
		if groupBy != "" {
			g, _ = r[groupBy].(string)
		}
		out[g] = toFloat(r["value"])
	}
	return out
}

// histBucketMap keys a bucket result by group and upper bound (+Inf for the
// unbounded bucket, whose reported le is null).
func histBucketMap(t *testing.T, c *Core, metric, groupBy, step string) map[histStepKey]map[float64]float64 {
	t.Helper()
	rows, _, err := c.QueryMetricWith("acme", metric, "buckets", 60, groupBy, nil, MetricOpts{Step: step})
	if err != nil {
		t.Fatalf("%s buckets: %v", metric, err)
	}
	out := map[histStepKey]map[float64]float64{}
	for _, r := range rows {
		g := ""
		if groupBy != "" {
			g, _ = r[groupBy].(string)
		}
		k := histStepKey{0, g}
		if step != "" {
			k.ts = asInt64(t, r["time"])
		}
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

func histFor(metric, route string) func(temp string, start, ts time.Time, count uint64, sum float64, buckets string) writer.MetricPoint {
	return func(temp string, start, ts time.Time, count uint64, sum float64, buckets string) writer.MetricPoint {
		p := stepHistPoint(metric, temp, start, ts, count, sum, buckets)
		if route != "" {
			p.Attributes = `{"route":"` + route + `"}`
		}
		return p
	}
}

func TestHistogramStepResetInsideBucket(t *testing.T) {
	base := stepBase()
	start := base.Add(-time.Hour)
	h := histFor("reset.bucket", "")
	c := newStepCore(t, []writer.MetricPoint{
		h(temporalityCumulative, start, base.Add(10*time.Second), 10, 100, "[4,6,0]"),
		h(temporalityCumulative, start, base.Add(20*time.Second), 15, 150, "[6,9,0]"),
		h(temporalityCumulative, start, base.Add(30*time.Second), 3, 30, "[1,2,0]"),
		h(temporalityCumulative, start, base.Add(50*time.Second), 7, 70, "[3,4,0]"),
		h(temporalityCumulative, start, base.Add(time.Minute+10*time.Second), 9, 90, "[4,5,0]"),
	})

	counts := histStepValues(t, c, "reset.bucket", "count", "")
	if got := counts[histStepKey{base.UnixMicro(), ""}]; got != 12 {
		t.Errorf("first bucket count = %v, want 12 (5 before the reset + 3 whole + 4)", got)
	}
	if got := counts[histStepKey{base.Add(time.Minute).UnixMicro(), ""}]; got != 2 {
		t.Errorf("second bucket count = %v, want 2", got)
	}
	sums := histStepValues(t, c, "reset.bucket", "sum", "")
	if got := sums[histStepKey{base.UnixMicro(), ""}]; got != 120 {
		t.Errorf("first bucket sum = %v, want 120", got)
	}
	if got := sums[histStepKey{base.Add(time.Minute).UnixMicro(), ""}]; got != 20 {
		t.Errorf("second bucket sum = %v, want 20", got)
	}

	buckets := histBucketMap(t, c, "reset.bucket", "", "1m")
	wantFirst := map[float64]float64{10: 5, 100: 7, math.Inf(1): 0}
	for hi, want := range wantFirst {
		if got := buckets[histStepKey{base.UnixMicro(), ""}][hi]; got != want {
			t.Errorf("first bucket le=%v = %v, want %v", hi, got, want)
		}
	}
	wantSecond := map[float64]float64{10: 1, 100: 1, math.Inf(1): 0}
	for hi, want := range wantSecond {
		if got := buckets[histStepKey{base.Add(time.Minute).UnixMicro(), ""}][hi]; got != want {
			t.Errorf("second bucket le=%v = %v, want %v", hi, got, want)
		}
	}

	p95 := histStepValues(t, c, "reset.bucket", "p95", "")
	if got, want := p95[histStepKey{base.UnixMicro(), ""}], 10+90*(0.95*12-5)/7; !near(got, want) {
		t.Errorf("first bucket p95 = %v, want %v", got, want)
	}
	if got, want := p95[histStepKey{base.Add(time.Minute).UnixMicro(), ""}], 10+90*(0.95*2-1)/1; !near(got, want) {
		t.Errorf("second bucket p95 = %v, want %v", got, want)
	}

	// The buckets and both point increments must still add up to the
	// whole-window answer.
	if got := histGroupValues(t, c, "reset.bucket", "count", "")[""]; got != 14 {
		t.Errorf("whole-window count = %v, want 14", got)
	}
	if got := histGroupValues(t, c, "reset.bucket", "sum", "")[""]; got != 140 {
		t.Errorf("whole-window sum = %v, want 140", got)
	}
}

func TestHistogramFirstPointBeforeAndInsideWindow(t *testing.T) {
	base := stepBase()
	before := histFor("window.start", "/before")
	inside := histFor("window.start", "/inside")
	c := newStepCore(t, []writer.MetricPoint{
		// Started before the window: the first point is only a baseline.
		before(temporalityCumulative, base.Add(-time.Hour), base, 100, 1000, "[50,40,10]"),
		before(temporalityCumulative, base.Add(-time.Hour), base.Add(time.Minute), 110, 1100, "[55,44,11]"),
		// Started inside the window: the first point counts whole, as if it
		// were a reset at the very first point of the series.
		inside(temporalityCumulative, base, base, 7, 70, "[2,5,0]"),
		inside(temporalityCumulative, base, base.Add(2*time.Minute), 9, 90, "[3,6,0]"),
	})

	wholeCount := histGroupValues(t, c, "window.start", "count", "attr.route")
	wholeSum := histGroupValues(t, c, "window.start", "sum", "attr.route")
	if wholeCount["/before"] != 10 || wholeCount["/inside"] != 9 {
		t.Errorf("whole-window counts = %v, want /before=10 /inside=9", wholeCount)
	}
	if wholeSum["/before"] != 100 || wholeSum["/inside"] != 90 {
		t.Errorf("whole-window sums = %v, want /before=100 /inside=90", wholeSum)
	}

	stepCount := histStepValues(t, c, "window.start", "count", "attr.route")
	wantStep := map[histStepKey]float64{
		{base.UnixMicro(), "/before"}:                      0,
		{base.Add(time.Minute).UnixMicro(), "/before"}:     10,
		{base.UnixMicro(), "/inside"}:                      7,
		{base.Add(2 * time.Minute).UnixMicro(), "/inside"}: 2,
	}
	if len(stepCount) != len(wantStep) {
		t.Fatalf("step counts = %v, want %v", stepCount, wantStep)
	}
	for k, want := range wantStep {
		if got := stepCount[k]; got != want {
			t.Errorf("step count[%v] = %v, want %v", k, got, want)
		}
	}

	stepBuckets := histBucketMap(t, c, "window.start", "attr.route", "1m")
	wantBuckets := map[histStepKey]map[float64]float64{
		{base.Add(time.Minute).UnixMicro(), "/before"}:     {10: 5, 100: 4, math.Inf(1): 1},
		{base.UnixMicro(), "/inside"}:                      {10: 2, 100: 5, math.Inf(1): 0},
		{base.Add(2 * time.Minute).UnixMicro(), "/inside"}: {10: 1, 100: 1, math.Inf(1): 0},
	}
	if len(stepBuckets) != len(wantBuckets) {
		t.Fatalf("step buckets = %v, want %v", stepBuckets, wantBuckets)
	}
	for k, want := range wantBuckets {
		for hi, w := range want {
			if got := stepBuckets[k][hi]; got != w {
				t.Errorf("step buckets[%v][%v] = %v, want %v", k, hi, got, w)
			}
		}
	}
}

func TestHistogramBucketListLengthChange(t *testing.T) {
	base := stepBase()
	start := base.Add(-time.Hour)
	h := histFor("len.change", "")
	c := newStepCore(t, []writer.MetricPoint{
		h(temporalityCumulative, start, base, 10, 100, "[2,5,3]"),
		h(temporalityCumulative, start, base.Add(20*time.Second), 15, 150, "[3,7,5]"),
		// A length change without a reset: the diff at this point is dropped,
		// but its scalar count and sum increments still count.
		h(temporalityCumulative, start, base.Add(40*time.Second), 20, 200, "[4,9,7,0]"),
		h(temporalityCumulative, start, base.Add(time.Minute+10*time.Second), 25, 250, "[5,11,9,0]"),
	})

	if got := histGroupValues(t, c, "len.change", "count", "")[""]; got != 15 {
		t.Errorf("whole-window count = %v, want 15", got)
	}
	if got := histGroupValues(t, c, "len.change", "sum", "")[""]; got != 150 {
		t.Errorf("whole-window sum = %v, want 150", got)
	}
	buckets := histBucketMap(t, c, "len.change", "", "")
	want := map[float64]float64{10: 2, 100: 4, math.Inf(1): 4}
	for hi, w := range want {
		if got := buckets[histStepKey{0, ""}][hi]; got != w {
			t.Errorf("whole-window le=%v = %v, want %v (the length-change diff is dropped)", hi, got, w)
		}
	}
	if got := histGroupValues(t, c, "len.change", "p95", "")[""]; got != 100 {
		t.Errorf("whole-window p95 = %v, want the highest finite bound 100", got)
	}

	stepCount := histStepValues(t, c, "len.change", "count", "")
	if got := stepCount[histStepKey{base.UnixMicro(), ""}]; got != 10 {
		t.Errorf("first bucket count = %v, want 10 (the length change still has a scalar increment)", got)
	}
	if got := stepCount[histStepKey{base.Add(time.Minute).UnixMicro(), ""}]; got != 5 {
		t.Errorf("second bucket count = %v, want 5", got)
	}
	stepBuckets := histBucketMap(t, c, "len.change", "", "1m")
	wantStep := map[histStepKey]map[float64]float64{
		{base.UnixMicro(), ""}:                  {10: 1, 100: 2, math.Inf(1): 2},
		{base.Add(time.Minute).UnixMicro(), ""}: {10: 1, 100: 2, math.Inf(1): 2},
	}
	for k, w := range wantStep {
		for hi, val := range w {
			if got := stepBuckets[k][hi]; got != val {
				t.Errorf("step buckets[%v][%v] = %v, want %v", k, hi, got, val)
			}
		}
	}
}

func TestHistogramSinglePointSeries(t *testing.T) {
	base := stepBase()
	pre := histFor("single.pt", "/pre")
	fresh := histFor("single.pt", "/fresh")
	c := newStepCore(t, []writer.MetricPoint{
		// One point, started before the window: baseline only.
		pre(temporalityCumulative, base.Add(-time.Hour), base, 8, 80, "[1,3,4]"),
		// One point, started inside the window: counted whole.
		fresh(temporalityCumulative, base, base, 5, 50, "[2,2,1]"),
	})

	if got := histGroupValues(t, c, "single.pt", "count", "")[""]; got != 5 {
		t.Errorf("whole-window count = %v, want 5", got)
	}
	if got := histGroupValues(t, c, "single.pt", "sum", "")[""]; got != 50 {
		t.Errorf("whole-window sum = %v, want 50", got)
	}
	buckets := histBucketMap(t, c, "single.pt", "attr.route", "")
	if _, ok := buckets[histStepKey{0, "/pre"}]; ok {
		t.Errorf("single baseline point produced a bucket row: %v", buckets[histStepKey{0, "/pre"}])
	}
	want := map[float64]float64{10: 2, 100: 2, math.Inf(1): 1}
	for hi, w := range want {
		if got := buckets[histStepKey{0, "/fresh"}][hi]; got != w {
			t.Errorf("in-window single point le=%v = %v, want %v", hi, got, w)
		}
	}
	if got := histGroupValues(t, c, "single.pt", "p95", "attr.route")["/fresh"]; got != 100 {
		t.Errorf("in-window single point p95 = %v, want 100", got)
	}

	stepCount := histStepValues(t, c, "single.pt", "count", "attr.route")
	if got := stepCount[histStepKey{base.UnixMicro(), "/pre"}]; got != 0 {
		t.Errorf("baseline-only step count = %v, want 0", got)
	}
	if got := stepCount[histStepKey{base.UnixMicro(), "/fresh"}]; got != 5 {
		t.Errorf("in-window single point step count = %v, want 5", got)
	}
	stepBuckets := histBucketMap(t, c, "single.pt", "attr.route", "1m")
	if _, ok := stepBuckets[histStepKey{base.UnixMicro(), "/pre"}]; ok {
		t.Errorf("baseline-only step produced a bucket row: %v", stepBuckets[histStepKey{base.UnixMicro(), "/pre"}])
	}
	for hi, w := range want {
		if got := stepBuckets[histStepKey{base.UnixMicro(), "/fresh"}][hi]; got != w {
			t.Errorf("in-window single point step bucket le=%v = %v, want %v", hi, got, w)
		}
	}
}
