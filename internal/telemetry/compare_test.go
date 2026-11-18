package telemetry

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// compareSpan seeds one span of the comparison fixture: a cache attribute, a
// unique request.id, and one of three pods, so the test can check both a real
// signal and the high-cardinality noise the tool must drop.
func compareSpan(i int, durationMs float64, cache string) writer.TraceSpan {
	now := time.Now()
	return writer.TraceSpan{
		TraceID: "trace-" + strconv.Itoa(i), SpanID: "span-" + strconv.Itoa(i), ParentSpanID: "",
		ServiceName: "api", SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: durationMs,
		StatusCode: "STATUS_CODE_OK",
		Attributes: `{"cache":"` + cache + `","request.id":"req-` + strconv.Itoa(i) + `"}`,
		ResourceAttributes: `{"service.name":"api","tenant.id":"acme","k8s.pod.name":"pod-` +
			strconv.Itoa(i%3) + `"}`,
		Events: "[]", Links: "[]",
	}
}

// newCompareCore seeds the fixture: 100 slow spans (300-500 ms, 90 misses and
// 10 hits) and 900 fast spans (10-50 ms, 45 misses).
func newCompareCore(t *testing.T) *Core {
	t.Helper()
	var spans []writer.TraceSpan
	for i := 0; i < 900; i++ {
		cache := "hit"
		if i < 45 {
			cache = "miss"
		}
		spans = append(spans, compareSpan(i, 10+float64(i%40), cache))
	}
	for i := 0; i < 100; i++ {
		cache := "miss"
		if i >= 90 {
			cache = "hit"
		}
		spans = append(spans, compareSpan(1000+i, 300+float64(i)*2, cache))
	}

	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add(spans)
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

func TestCompareSpansExplicitSlowRange(t *testing.T) {
	c := newCompareCore(t)
	minMs, maxMs := 300.0, 500.0

	cmp, err := c.CompareSpans("acme", CompareOpts{
		ServiceName: "api",
		Slow:        Range{Min: &minMs, Max: &maxMs},
	})
	if err != nil {
		t.Fatalf("CompareSpans: %v", err)
	}
	if cmp.ThresholdMs != nil {
		t.Errorf("explicit range should not report a derived threshold, got %v", *cmp.ThresholdMs)
	}
	if cmp.SlowN != 100 || cmp.RestN != 900 {
		t.Errorf("slow_n/rest_n = %d/%d, want 100/900", cmp.SlowN, cmp.RestN)
	}
	if len(cmp.Rows) == 0 {
		t.Fatal("expected comparison rows")
	}

	first := cmp.Rows[0]
	if first.Key != "cache" || first.Value != "miss" {
		t.Errorf("first row = %s=%s, want cache=miss", first.Key, first.Value)
	}
	if first.SlowPct != 90 || first.RestPct != 5 || first.Diff != 85 {
		t.Errorf("cache=miss = %v/%v diff %v, want 90/5 diff 85", first.SlowPct, first.RestPct, first.Diff)
	}

	for _, r := range cmp.Rows {
		if r.Key == "request.id" {
			t.Errorf("unique request.id should be dropped as noise, got %+v", r)
		}
		if r.Key == "resource.k8s.pod.name" && math.Abs(r.Diff) > 5 {
			t.Errorf("evenly spread pod should not look distinctive, got %+v", r)
		}
		if r.Key == "resource.service.name" {
			t.Errorf("service.name should be skipped when the service is fixed, got %+v", r)
		}
	}
}

func TestCompareSpansDerivesP95Threshold(t *testing.T) {
	c := newCompareCore(t)

	cmp, err := c.CompareSpans("acme", CompareOpts{ServiceName: "api"})
	if err != nil {
		t.Fatalf("CompareSpans: %v", err)
	}
	if cmp.ThresholdMs == nil {
		t.Fatal("default comparison should report threshold_ms")
	}
	if *cmp.ThresholdMs < 300 || *cmp.ThresholdMs > 500 {
		t.Errorf("threshold_ms = %v, want a value inside the slow spans' 300-500 ms range", *cmp.ThresholdMs)
	}
	// The p95 boundary keeps about the slowest 5% of the scope.
	if cmp.SlowN < 40 || cmp.SlowN > 60 {
		t.Errorf("slow_n = %d, want about 5%% of 1000 spans", cmp.SlowN)
	}
	if cmp.SlowN+cmp.RestN != 1000 {
		t.Errorf("slow_n + rest_n = %d, want 1000", cmp.SlowN+cmp.RestN)
	}
	if len(cmp.Rows) == 0 {
		t.Error("expected comparison rows for the derived threshold")
	}
}

func TestCompareSpansExplicitRangeMatchesFixtureCounts(t *testing.T) {
	c := newCompareCore(t)
	minMs, maxMs := 300.0, 500.0

	explicit, err := c.CompareSpans("acme", CompareOpts{
		ServiceName: "api",
		Slow:        Range{Min: &minMs, Max: &maxMs},
	})
	if err != nil {
		t.Fatalf("CompareSpans: %v", err)
	}
	if explicit.SlowN != 100 || explicit.RestN != 900 {
		t.Errorf("explicit [300,500] = %d/%d, want the fixture's 100/900",
			explicit.SlowN, explicit.RestN)
	}
}

func TestCompareSpansWindowExcludesOutsideSpans(t *testing.T) {
	now := time.Now()
	old, new := now.Add(-25*time.Minute), now.Add(-5*time.Minute)
	at := func(id string, ts time.Time, dur float64, cache string) writer.TraceSpan {
		return writer.TraceSpan{
			TraceID: "trace-" + id, SpanID: "span-" + id, ParentSpanID: "",
			ServiceName: "api", SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
			StartTime: ts.UnixMicro(), EndTime: ts.UnixMicro(), DurationMs: dur,
			StatusCode: "STATUS_CODE_OK", Attributes: `{"cache":"` + cache + `"}`,
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
			Events:             "[]", Links: "[]",
		}
	}
	spans := []writer.TraceSpan{
		at("old-slow-1", old, 400, "miss"),
		at("old-slow-2", old, 400, "miss"),
		at("old-slow-3", old, 400, "miss"),
		at("old-slow-4", old, 400, "miss"),
		at("old-slow-5", old, 400, "miss"),
		at("old-fast-1", old, 10, "hit"),
		at("old-fast-2", old, 10, "hit"),
		at("old-fast-3", old, 10, "hit"),
		at("old-fast-4", old, 10, "hit"),
		at("old-fast-5", old, 10, "hit"),
	}
	for i := 0; i < 10; i++ {
		spans = append(spans, at("new-"+strconv.Itoa(i), new, 10+float64(i%20), "hit"))
	}

	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add(spans)
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	c := NewCore(engine)

	minMs, maxMs := 300.0, 500.0
	cmp, err := c.CompareSpans("acme", CompareOpts{
		ServiceName: "api",
		Since:       30,
		Until:       20,
		Slow:        Range{Min: &minMs, Max: &maxMs},
	})
	if err != nil {
		t.Fatalf("CompareSpans: %v", err)
	}
	if cmp.SlowN != 5 || cmp.RestN != 5 {
		t.Errorf("windowed slow_n/rest_n = %d/%d, want the old slice's 5/5", cmp.SlowN, cmp.RestN)
	}
	if len(cmp.Rows) == 0 || cmp.Rows[0].Key != "cache" || cmp.Rows[0].Diff != 100 {
		t.Errorf("windowed top row = %+v, want cache with a 100 point difference", cmp.Rows)
	}
}

func TestCompareSpansSamplingKeepsTrueCounts(t *testing.T) {
	c := newCompareCore(t)

	saved := compareSampleCap
	compareSampleCap = 50
	defer func() { compareSampleCap = saved }()

	minMs, maxMs := 300.0, 500.0
	cmp, err := c.CompareSpans("acme", CompareOpts{
		ServiceName: "api",
		Slow:        Range{Min: &minMs, Max: &maxMs},
	})
	if err != nil {
		t.Fatalf("CompareSpans: %v", err)
	}
	if !cmp.Sampled {
		t.Error("Sampled should be true when both groups exceed the cap")
	}
	if cmp.SlowN != 100 || cmp.RestN != 900 {
		t.Errorf("sampled slow_n/rest_n = %d/%d, want the true 100/900", cmp.SlowN, cmp.RestN)
	}
	if len(cmp.Rows) == 0 {
		t.Error("expected rows from the sample")
	}
}

func TestCompareSpansEmptyScope(t *testing.T) {
	c := newWindowCore(t, nil, nil, nil)

	cmp, err := c.CompareSpans("acme", CompareOpts{ServiceName: "api"})
	if err != nil {
		t.Fatalf("empty scope should not error: %v", err)
	}
	if cmp.SlowN != 0 || cmp.RestN != 0 {
		t.Errorf("empty scope counts = %d/%d, want 0/0", cmp.SlowN, cmp.RestN)
	}
	if len(cmp.Rows) != 0 {
		t.Errorf("empty scope rows = %+v, want none", cmp.Rows)
	}
}

func TestCompareSpansInvalidInput(t *testing.T) {
	c := newWindowCore(t, nil, nil, nil)

	minMs, maxMs := 500.0, 100.0
	cases := []struct {
		name   string
		tenant string
		opts   CompareOpts
	}{
		{"missing tenant", "", CompareOpts{}},
		{"bad slow range", "acme", CompareOpts{Slow: Range{Min: &minMs, Max: &maxMs}}},
		{"until after since", "acme", CompareOpts{Since: 10, Until: 10}},
		{"negative until", "acme", CompareOpts{Until: -1}},
	}
	for _, tc := range cases {
		_, err := c.CompareSpans(tc.tenant, tc.opts)
		if err == nil {
			t.Errorf("%s: expected an error", tc.name)
			continue
		}
		if !IsInvalidInput(err) {
			t.Errorf("%s: should classify as invalid input: %v", tc.name, err)
		}
	}
}
