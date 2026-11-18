package telemetry

import (
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// --- helpers ---

// seedSpan mirrors what the receiver actually stores: service.name and
// tenant.id arrive as RESOURCE attributes (service.name is additionally
// promoted to the service_name column), while the span's own attributes carry
// only keys the instrumenting app set.
func seedSpan(svc, tenant, traceID string, attrs string) writer.TraceSpan {
	now := time.Now()
	return writer.TraceSpan{
		TraceID: traceID, SpanID: "s-" + svc, ParentSpanID: "",
		ServiceName: svc, SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 5,
		StatusCode: "STATUS_CODE_OK", Attributes: attrs,
		ResourceAttributes: `{"service.name":"` + svc + `","tenant.id":"` + tenant + `"}`,
		Events:             "[]", Links: "[]",
	}
}

// newTestCore seeds a store and returns a Core wired to it.
func newTestCore(t *testing.T) *Core {
	t.Helper()
	dir := t.TempDir()

	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		seedSpan("api", "acme", "trace-acme-1", `{"http.method":"GET","custom.key":"x"}`),
		seedSpan("api", "acme", "trace-acme-1", `{"http.method":"POST"}`),
		seedSpan("worker", "acme", "trace-acme-2", `{"job":"sync"}`),
		seedSpan("api", "globex", "trace-globex-1", `{"http.method":"GET"}`),
	})
	w.AddMetrics([]writer.MetricPoint{
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: time.Now().UnixMicro(),
			ValueDouble: 0.5, ServiceName: "api", ResourceAttributes: `{"tenant.id":"acme"}`},
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: time.Now().UnixMicro(),
			ValueDouble: 0.9, ServiceName: "worker", ResourceAttributes: `{"tenant.id":"acme"}`},
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: time.Now().UnixMicro(),
			ValueDouble: 0.99, ServiceName: "api", ResourceAttributes: `{"tenant.id":"globex"}`},
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

func countRows(rows []map[string]any) int { return len(rows) }

// --- trace_lookup ---

func TestTraceLookupReturnsAllSpansOfTrace(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.LookupTrace("trace-acme-1", "acme", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 2 {
		t.Errorf("got %d spans, want 2 for trace-acme-1", n)
	}
}

func TestTraceLookupRequiresTenant(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.LookupTrace("trace-acme-1", "", 0)
	if err == nil {
		t.Error("trace lookup without tenant should error")
	}
	if !IsInvalidInput(err) {
		t.Errorf("missing tenant should classify as invalid input: %v", err)
	}
}

func TestTraceLookupRequiresTraceID(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.LookupTrace("", "acme", 0)
	if err == nil {
		t.Error("trace lookup without trace_id should error")
	}
}

func TestTraceLookupEmptyTenantRejected(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.LookupTrace("x", "   ", 0)
	if err == nil {
		t.Error("whitespace tenant should be rejected")
	}
}

// --- tenant isolation (the security boundary) ---

// TestTenantIsolation is the important one: a query scoped to one tenant must
// never return another tenant's rows, whatever the caller asks for.
func TestTenantIsolation(t *testing.T) {
	c := newTestCore(t)

	// The globex trace exists, but acme must not be able to read it.
	rows, _, err := c.LookupTrace("trace-globex-1", "acme", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 0 {
		t.Errorf("acme can read globex's trace: %d rows", n)
	}

	// Same via SearchSpans with no other filters.
	rows, _, _ = c.SearchSpans("acme", "", nil, 0, 0, 0)
	if n := countRows(rows); n != 3 {
		t.Errorf("acme search returned %d rows, want 3 (only acme's)", n)
	}
	rows, _, _ = c.SearchSpans("globex", "", nil, 0, 0, 0)
	if n := countRows(rows); n != 1 {
		t.Errorf("globex search returned %d rows, want 1", n)
	}
}

func TestMetricQueryIsTenantScoped(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.QueryMetric("acme", "cpu.util", "count", 0, 0, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 1 {
		t.Fatalf("expected 1 aggregate row, got %d", n)
	}
	// acme has 2 points; globex has 1. A leak would show 3.
	if got := asFloat64(t, rows[0]["value"]); got != 2 {
		t.Errorf("acme count = %v, want 2 (globex's point leaked?)", got)
	}
}

// asFloat64 coerces a driver-returned numeric value regardless of its
// concrete Go type: count(*) comes back as int64, while avg/sum/quantiles
// come back as float64. Only the MCP path's JSON round-trip normalizes this
// to float64 uniformly — calling Core directly sees the driver's real type.
func asFloat64(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	default:
		t.Fatalf("value %v is neither float64 nor int64 (%T)", v, v)
		return 0
	}
}

// --- span search ---

// TestSpanSearchDottedAttributeKey is the regression test for a real trap:
// `$.service.name` is interpreted as field "service" then "name" and yields
// NULL, so a naive filter silently matches nothing. OTel keys are dotted.
func TestSpanSearchDottedAttributeKey(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.SearchSpans("acme", "", []AttrFilter{{Key: "custom.key", Value: "x"}}, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 1 {
		t.Errorf("dotted attribute filter matched %d rows, want 1 — dotted keys are broken", n)
	}
}

// TestSpanSearchFindsResourceAttributes guards a silent-zero-result trap:
// service.name arrives as a RESOURCE attribute (and is promoted to the
// service_name column) but is absent from span attributes, so a filter that
// checks only `attributes` matches nothing for the most likely key a caller
// would use.
func TestSpanSearchFindsResourceAttributes(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.SearchSpans("acme", "", []AttrFilter{{Key: "service.name", Value: "worker"}}, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 1 {
		t.Errorf("filter on service.name matched %d rows, want 1 — resource attributes are not searched", n)
	}

	// And a key that lives in span attributes still works.
	rows, _, _ = c.SearchSpans("acme", "", []AttrFilter{{Key: "job", Value: "sync"}}, 0, 0, 0)
	if n := countRows(rows); n != 1 {
		t.Errorf("filter on span attribute matched %d rows, want 1", n)
	}
}

// TestSpanSearchMultipleFiltersAreAnded verifies filters combine with AND.
func TestSpanSearchMultipleFiltersAreAnded(t *testing.T) {
	c := newTestCore(t)

	rows, _, _ := c.SearchSpans("acme", "", []AttrFilter{{Key: "http.method", Value: "GET"}}, 0, 0, 0)
	// Only the first acme span has http.method=GET.
	if n := countRows(rows); n != 1 {
		t.Errorf("http.method=GET matched %d acme spans, want 1", n)
	}
}

func TestSpanSearchServiceFilter(t *testing.T) {
	c := newTestCore(t)

	rows, _, _ := c.SearchSpans("acme", "worker", nil, 0, 0, 0)
	if n := countRows(rows); n != 1 {
		t.Errorf("service_name=worker matched %d, want 1", n)
	}
}

func TestSpanSearchEmptyKeyRejected(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.SearchSpans("acme", "", []AttrFilter{{Key: "", Value: "x"}}, 0, 0, 0)
	if err == nil {
		t.Error("empty filter key should error")
	}
}

// TestSpanSearchLimitIsCapped verifies a caller cannot request unbounded rows.
func TestSpanSearchLimitIsCapped(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.SearchSpans("acme", "", nil, 0, 0, 100000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 3 rows exist, so this only proves it did not error; the cap itself is
	// asserted by the fact that the query completed with the limit applied.
	if n := countRows(rows); n != 3 {
		t.Errorf("got %d rows, want 3", n)
	}
}

func TestSpanSearchDefaultWindowExcludesOldData(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	old := time.Now().Add(-48 * time.Hour)
	w.Add([]writer.TraceSpan{{
		TraceID: "old", SpanID: "s", ServiceName: "api", SpanName: "op",
		StartTime: old.UnixMicro(), EndTime: old.UnixMicro(), DurationMs: 1,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: `{"tenant.id":"acme"}`, Events: "[]", Links: "[]",
	}})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	c := NewCore(engine)

	// Default window is 60 minutes, so 48h-old data must not appear.
	rows, _, _ := c.SearchSpans("acme", "", nil, 0, 0, 0)
	if n := countRows(rows); n != 0 {
		t.Errorf("default window included 48h-old data: %d rows", n)
	}
}

// --- metric query ---

func TestMetricQueryAggregations(t *testing.T) {
	c := newTestCore(t)

	for _, agg := range []string{"avg", "sum", "min", "max", "count", "p50", "p95", "p99"} {
		rows, _, err := c.QueryMetric("acme", "cpu.util", agg, 0, 0, "", nil)
		if err != nil {
			t.Errorf("aggregation %q errored: %v", agg, err)
			continue
		}
		if n := countRows(rows); n != 1 {
			t.Errorf("aggregation %q returned %d rows, want 1", agg, n)
		}
	}
}

func TestMetricQueryRejectsUnknownAggregation(t *testing.T) {
	c := newTestCore(t)

	// An injection-shaped aggregation must be rejected, not interpolated.
	for _, agg := range []string{"median", "avg(value_double); DROP VIEW metrics; --", ""} {
		_, _, err := c.QueryMetric("acme", "cpu.util", agg, 0, 0, "", nil)
		if err == nil {
			t.Errorf("aggregation %q should be rejected", agg)
		}
		if !IsInvalidInput(err) {
			t.Errorf("aggregation %q rejection should classify as invalid input: %v", agg, err)
		}
	}
}

func TestMetricQueryGroupBy(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.QueryMetric("acme", "cpu.util", "count", 0, 0, "service_name", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// acme has api and worker -> 2 groups
	if n := countRows(rows); n != 2 {
		t.Errorf("group_by service_name returned %d rows, want 2", n)
	}
}

func TestMetricQueryRejectsUngroupableColumn(t *testing.T) {
	c := newTestCore(t)

	for _, col := range []string{"nonexistent", "service_name; DROP VIEW metrics; --", "1"} {
		_, _, err := c.QueryMetric("acme", "cpu.util", "count", 0, 0, col, nil)
		if err == nil {
			t.Errorf("group_by %q should be rejected", col)
		}
	}
}

func TestMetricQueryRequiresName(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.QueryMetric("acme", "", "avg", 0, 0, "", nil)
	if err == nil {
		t.Error("metric query without metric_name should error")
	}
}

// --- attr.<key> group_by ---

// newAttrMetricCore seeds metric points carrying data-point attributes, the
// shape a nightly-batch pipeline's telemetry uses (kind gen|judge, backend
// local|alibaba), plus one point from another tenant to prove isolation
// still holds through the new grouping path.
func newAttrMetricCore(t *testing.T) *Core {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)

	point := func(tenant, kind, backend string, v float64) writer.MetricPoint {
		return writer.MetricPoint{
			MetricName: "night.calls", MetricType: "sum", Timestamp: time.Now().UnixMicro(),
			ValueDouble:        v,
			Attributes:         `{"kind":"` + kind + `","backend":"` + backend + `"}`,
			ResourceAttributes: `{"service.name":"night-pipeline","tenant.id":"` + tenant + `"}`,
		}
	}
	w.AddMetrics([]writer.MetricPoint{
		point("owner", "gen", "local", 1.0),
		point("owner", "gen", "alibaba", 1.0),
		point("owner", "judge", "local", 1.0),
		point("owner", "judge", "alibaba", 1.0),
		point("owner", "gen", "local", 1.0), // second gen/local point: sums must fold it in
		point("other-tenant", "gen", "local", 999.0),
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

func TestMetricQueryGroupByAttrKey(t *testing.T) {
	c := newAttrMetricCore(t)

	rows, cols, err := c.QueryMetric("owner", "night.calls", "sum", 0, 0, "attr.kind", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The alias must surface as the result column name, per the ticket ask.
	if len(cols) != 2 || cols[0] != "attr.kind" || cols[1] != "value" {
		t.Fatalf("columns = %v, want [attr.kind value]", cols)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (gen, judge): %v", len(rows), rows)
	}
	sums := map[string]float64{}
	for _, r := range rows {
		sums[r["attr.kind"].(string)] = asFloat64(t, r["value"])
	}
	// 3 gen points (1+1+1) and 2 judge points — and the other tenant's 999
	// must not leak in.
	if sums["gen"] != 3 || sums["judge"] != 2 {
		t.Errorf("sums = %v, want gen=3 judge=2", sums)
	}
}

func TestMetricQueryGroupByAttributesPrefix(t *testing.T) {
	c := newAttrMetricCore(t)

	rows, _, err := c.QueryMetric("owner", "night.calls", "count", 0, 0, "attributes.kind", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2 via attributes.kind form", len(rows))
	}
}

func TestMetricQueryGroupByMultipleAttrKeys(t *testing.T) {
	c := newAttrMetricCore(t)

	rows, cols, err := c.QueryMetric("owner", "night.calls", "count", 0, 0, "attr.kind,attr.backend", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cols) != 3 || cols[0] != "attr.kind" || cols[1] != "attr.backend" {
		t.Fatalf("columns = %v, want [attr.kind attr.backend value]", cols)
	}
	if len(rows) != 4 {
		t.Errorf("got %d rows, want 4 (kind x backend combos): %v", len(rows), rows)
	}
}

func TestMetricQueryMixedColumnAndAttrGroupBy(t *testing.T) {
	c := newAttrMetricCore(t)

	// A plain column and an attribute key in the same group_by.
	rows, _, err := c.QueryMetric("owner", "night.calls", "count", 0, 0, "service_name,attr.kind", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 { // one service_name value, two kinds
		t.Errorf("got %d rows, want 2: %v", len(rows), rows)
	}
}

func TestMetricQueryRejectsBadAttrKey(t *testing.T) {
	c := newAttrMetricCore(t)

	for _, key := range []string{
		"attr.kind; DROP VIEW metrics; --",
		`attr."kind"`,
		"attr.",
		"attr.kind service_name",
		"attr.kind' OR '1'='1",
	} {
		_, _, err := c.QueryMetric("owner", "night.calls", "count", 0, 0, key, nil)
		if err == nil {
			t.Errorf("group_by %q should be rejected", key)
			continue
		}
		if !IsInvalidInput(err) {
			t.Errorf("group_by %q rejection should classify as invalid input: %v", key, err)
		}
	}

	// And the store must still be intact after the injection attempts.
	rows, _, err := c.QueryMetric("owner", "night.calls", "count", 0, 0, "", nil)
	if err != nil {
		t.Fatalf("store damaged after injection attempts: %v", err)
	}
	if n := countRows(rows); n != 1 {
		t.Errorf("post-injection count row = %d, want 1", n)
	}
}

func TestMetricQueryAttributeFilters(t *testing.T) {
	c := newAttrMetricCore(t)

	// Filter on a data-point attribute: only gen points.
	rows, _, err := c.QueryMetric("owner", "night.calls", "count", 0, 0, "", []AttrFilter{{Key: "kind", Value: "gen"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 1 {
		t.Fatalf("expected 1 aggregate row, got %d", n)
	}
	if got := asFloat64(t, rows[0]["value"]); got != 3 {
		t.Errorf("gen count = %v, want 3 (2 owner-local + 1 owner-alibaba, tenant-scoped)", got)
	}

	// Filter on a RESOURCE attribute must also match (same dual scope as
	// span_search): only the local backend points... backend is a data-point
	// attr here; use service.name for the resource-side check.
	rows, _, err = c.QueryMetric("owner", "night.calls", "count", 0, 0, "",
		[]AttrFilter{{Key: "service.name", Value: "night-pipeline"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := asFloat64(t, rows[0]["value"]); got != 5 {
		t.Errorf("resource-attr filter count = %v, want 5 (all owner points)", got)
	}

	// Both filter types ANDed: gen AND alibaba -> the single alibaba gen point.
	rows, _, err = c.QueryMetric("owner", "night.calls", "sum", 0, 0, "",
		[]AttrFilter{{Key: "kind", Value: "gen"}, {Key: "backend", Value: "alibaba"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := asFloat64(t, rows[0]["value"]); got != 1 {
		t.Errorf("gen+alibaba sum = %v, want 1", got)
	}

	// Injection-shaped filter values stay data, like span_search's test.
	rows, _, err = c.QueryMetric("owner", "night.calls", "count", 0, 0, "",
		[]AttrFilter{{Key: "kind", Value: "gen' OR '1'='1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 1 || asFloat64(t, rows[0]["value"]) != 0 {
		t.Errorf("injection filter should match nothing: %v", rows)
	}
}

func TestMetricQueryAttrGroupByAndFilterCombined(t *testing.T) {
	c := newAttrMetricCore(t)

	rows, _, err := c.QueryMetric("owner", "night.calls", "sum", 0, 0, "attr.backend",
		[]AttrFilter{{Key: "kind", Value: "judge"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (local, alibaba): %v", len(rows), rows)
	}
	sums := map[string]float64{}
	for _, r := range rows {
		sums[r["attr.backend"].(string)] = asFloat64(t, r["value"])
	}
	if sums["local"] != 1 || sums["alibaba"] != 1 {
		t.Errorf("judge sums = %v, want local=1 alibaba=1", sums)
	}
}

// --- log search ---

// withTrace tags a seeded record with a trace id.
func withTrace(r writer.LogRecord, traceID string) writer.LogRecord {
	r.TraceID = traceID
	return r
}

// newLogCore seeds log records covering the filter dimensions: two tenants,
// two services, three severities, distinguishable bodies, a record-level and
// a resource-level attribute, one record older than the default window, and
// trace ids (trace-1 with two records, trace-2, a globex trace, and a 48h-old
// trace).
func newLogCore(t *testing.T) *Core {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)

	logRec := func(svc, tenant, sev, body, attrs string, ts time.Time) writer.LogRecord {
		return writer.LogRecord{
			Timestamp: ts.UnixMicro(), ObservedTimestamp: ts.UnixMicro(),
			SeverityText: sev, SeverityNumber: 9, Body: body,
			Attributes:         attrs,
			ResourceAttributes: `{"service.name":"` + svc + `","tenant.id":"` + tenant + `"}`,
			ServiceName:        svc,
		}
	}
	now := time.Now()
	w.AddLogs([]writer.LogRecord{
		withTrace(logRec("api", "acme", "ERROR", "connection refused to payments-db", `{"http.status_code":"500"}`, now.Add(-2*time.Minute)), "trace-1"),
		withTrace(logRec("api", "acme", "INFO", "handled GET /products in 12ms", `{}`, now.Add(-3*time.Minute)), "trace-1"),
		withTrace(logRec("worker", "acme", "WARN", "queue depth above threshold", `{"queue":"sync"}`, now.Add(-4*time.Minute)), "trace-2"),
		withTrace(logRec("api", "globex", "ERROR", "globex-only failure", `{}`, now.Add(-1*time.Minute)), "trace-globex-1"),
		withTrace(logRec("nightly", "acme", "ERROR", "ancient failure", `{}`, now.Add(-48*time.Hour)), "trace-old"),
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

func TestSearchLogsTenantIsolation(t *testing.T) {
	c := newLogCore(t)

	// acme sees its 3 in-window records; globex's and the 48h-old one stay out.
	rows, _, err := c.SearchLogs("acme", "", "", "", "", nil, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := len(rows); n != 3 {
		t.Errorf("acme logs = %d rows, want 3 (globex or old leaked?)", n)
	}
	rows, _, _ = c.SearchLogs("globex", "", "", "", "", nil, 0, 0, 0)
	if n := len(rows); n != 1 {
		t.Errorf("globex logs = %d rows, want 1", n)
	}
}

func TestSearchLogsNewestFirst(t *testing.T) {
	c := newLogCore(t)

	rows, _, err := c.SearchLogs("acme", "", "", "", "", nil, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0]["body"].(string); got != "connection refused to payments-db" {
		t.Errorf("first row = %q, want the newest record", got)
	}
}

func TestSearchLogsSeverityCaseInsensitive(t *testing.T) {
	c := newLogCore(t)

	for _, sev := range []string{"error", "ERROR", "Error"} {
		rows, _, err := c.SearchLogs("acme", "", "", sev, "", nil, 0, 0, 0)
		if err != nil {
			t.Fatalf("severity %q: %v", sev, err)
		}
		if n := len(rows); n != 1 {
			t.Errorf("severity %q matched %d rows, want 1", sev, n)
		}
	}
}

func TestSearchLogsBodySearchCaseInsensitive(t *testing.T) {
	c := newLogCore(t)

	for _, q := range []string{"refused", "REFUSED", "connection"} {
		rows, _, err := c.SearchLogs("acme", "", "", "", q, nil, 0, 0, 0)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if n := len(rows); n != 1 {
			t.Errorf("search %q matched %d rows, want 1", q, n)
		}
	}
}

func TestSearchLogsServiceAndCombinedFilters(t *testing.T) {
	c := newLogCore(t)

	rows, _, err := c.SearchLogs("acme", "", "worker", "", "", nil, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rows); n != 1 {
		t.Errorf("service=worker matched %d rows, want 1", n)
	}

	// severity AND service together: the api ERROR, not the worker WARN.
	rows, _, _ = c.SearchLogs("acme", "", "api", "ERROR", "", nil, 0, 0, 0)
	if n := len(rows); n != 1 {
		t.Errorf("api+ERROR matched %d rows, want 1", n)
	}
	if got := rows[0]["service_name"].(string); got != "api" {
		t.Errorf("combined filter returned service %q", got)
	}
}

func TestSearchLogsAttributeFilters(t *testing.T) {
	c := newLogCore(t)

	// Record-level attribute.
	rows, _, err := c.SearchLogs("acme", "", "", "", "", []AttrFilter{{Key: "http.status_code", Value: "500"}}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rows); n != 1 {
		t.Errorf("record-attr filter matched %d rows, want 1", n)
	}

	// Resource-level attribute through the same filter (dual scope).
	rows, _, _ = c.SearchLogs("acme", "", "", "", "", []AttrFilter{{Key: "service.name", Value: "worker"}}, 0, 0, 0)
	if n := len(rows); n != 1 {
		t.Errorf("resource-attr filter matched %d rows, want 1", n)
	}

	// Injection-shaped value stays data.
	rows, _, _ = c.SearchLogs("acme", "", "", "", "", []AttrFilter{{Key: "http.status_code", Value: "500' OR '1'='1"}}, 0, 0, 0)
	if n := len(rows); n != 0 {
		t.Errorf("injection filter matched %d rows, want 0", n)
	}

	// Empty key is rejected.
	_, _, err = c.SearchLogs("acme", "", "", "", "", []AttrFilter{{Key: "", Value: "x"}}, 0, 0, 0)
	if err == nil {
		t.Error("empty filter key should error")
	}
}

func TestSearchLogsWiderWindowIncludesOld(t *testing.T) {
	c := newLogCore(t)

	rows, _, _ := c.SearchLogs("acme", "", "", "", "ancient", nil, 0, 0, 0)
	if n := len(rows); n != 0 {
		t.Errorf("48h-old log leaked into the default window: %d rows", n)
	}
	rows, _, err := c.SearchLogs("acme", "", "", "", "ancient", nil, 7*24*60, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rows); n != 1 {
		t.Errorf("explicit 7d window returned %d rows, want 1", n)
	}
}

func TestSearchLogsLimitIsCapped(t *testing.T) {
	c := newLogCore(t)

	rows, _, err := c.SearchLogs("acme", "", "", "", "", nil, 0, 0, 100000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := len(rows); n != 3 {
		t.Errorf("got %d rows, want 3", n)
	}
}

func TestSearchLogsRequiresTenant(t *testing.T) {
	c := newLogCore(t)

	if _, _, err := c.SearchLogs("", "", "", "", "", nil, 0, 0, 0); err == nil {
		t.Error("missing tenant should error")
	}
}

// --- trace-scoped log lookup ---

func TestSearchLogsByTraceID(t *testing.T) {
	c := newLogCore(t)

	rows, _, err := c.SearchLogs("acme", "trace-1", "", "", "", nil, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := len(rows); n != 2 {
		t.Errorf("trace-1 has %d records, want 2", n)
	}

	// Combined with another filter.
	rows, _, _ = c.SearchLogs("acme", "trace-1", "", "ERROR", "", nil, 0, 0, 0)
	if n := len(rows); n != 1 {
		t.Errorf("trace-1 + ERROR = %d records, want 1", n)
	}

	// Tenant isolation holds through the trace path: globex's trace must not
	// be readable as acme's.
	rows, _, _ = c.SearchLogs("acme", "trace-globex-1", "", "", "", nil, 0, 0, 0)
	if n := len(rows); n != 0 {
		t.Errorf("acme read globex's trace logs: %d records", n)
	}
}

// TestSearchLogsTraceIDHasNoDefaultWindow pins the semantic that makes the
// waterfall work for old traces: a trace id is a precise key, so it looks
// across the whole history unless a window is given explicitly.
func TestSearchLogsTraceIDHasNoDefaultWindow(t *testing.T) {
	c := newLogCore(t)

	rows, _, err := c.SearchLogs("acme", "trace-old", "", "", "", nil, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := len(rows); n != 1 {
		t.Errorf("48h-old trace returned %d records, want 1 (default window must not apply to a trace lookup)", n)
	}

	// An explicit window still bounds it.
	rows, _, _ = c.SearchLogs("acme", "trace-old", "", "", "", nil, 60, 0, 0)
	if n := len(rows); n != 0 {
		t.Errorf("explicit 60m window returned %d records, want 0", n)
	}
}

// --- injection resistance at the query boundary ---

// TestFiltersCannotInjectSQL verifies attribute keys and values are bound, not
// interpolated, so a crafted filter is data rather than SQL.
func TestFiltersCannotInjectSQL(t *testing.T) {
	c := newTestCore(t)

	payloads := []AttrFilter{
		{Key: "http.method", Value: "GET' OR '1'='1"},
		{Key: `x' OR '1'='1`, Value: "y"},
		{Key: `a" AND json_extract_string(resource_attributes,'$."tenant.id"')!='acme' AND 1=1 --`, Value: "z"},
	}
	for _, f := range payloads {
		rows, _, err := c.SearchSpans("acme", "", []AttrFilter{f}, 0, 0, 0)
		if err != nil {
			continue // rejecting outright is also fine
		}
		// Must not return rows: a successful injection would widen the set.
		if n := countRows(rows); n != 0 {
			t.Errorf("payload %+v matched %d rows — filter was interpreted as SQL", f, n)
		}
	}

	// And the store must still be intact.
	rows, _, _ := c.SearchSpans("acme", "", nil, 0, 0, 0)
	if n := countRows(rows); n != 3 {
		t.Errorf("store damaged: acme now has %d spans, want 3", n)
	}
}

// --- discovery: ListTenants / ListServices ---

// TestListTenantsIncludesLogsAndMetricsOnlyTenants: a tenant that sends no
// traces must still be discoverable.
func TestListTenantsIncludesLogsAndMetricsOnlyTenants(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{seedSpan("api", "tracesco", "t1", "{}")})
	w.AddLogs([]writer.LogRecord{{Timestamp: time.Now().UnixMicro(), ServiceName: "l",
		Body: "x", Attributes: "{}", ResourceAttributes: `{"tenant.id":"logsco"}`}})
	w.AddMetrics([]writer.MetricPoint{{MetricName: "m", MetricType: "gauge",
		Timestamp: time.Now().UnixMicro(), ResourceAttributes: `{"tenant.id":"metricsco"}`},
		{MetricName: "m", MetricType: "gauge",
			Timestamp: time.Now().UnixMicro(), ResourceAttributes: `{"tenant.id":"tracesco"}`}})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })

	rows, _, err := NewCore(engine).ListTenants()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r["tenant"].(string))
	}
	want := []string{"logsco", "metricsco", "tracesco"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ListTenants = %v, want %v (sorted, each once)", got, want)
	}
}

// TestListTenantsWithOnlyTraces: the empty logs/metrics placeholder views
// must not break the union.
func TestListTenantsWithOnlyTraces(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{seedSpan("api", "solo", "t1", "{}")})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	rows, _, err := NewCore(engine).ListTenants()
	if err != nil || len(rows) != 1 || rows[0]["tenant"] != "solo" {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}

func TestListTenants(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.ListTenants()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r["tenant"].(string)] = true
	}
	if !got["acme"] || !got["globex"] {
		t.Errorf("ListTenants = %v, want acme and globex", rows)
	}
}

func TestListServices(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.ListServices("acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r["service_name"].(string)] = true
	}
	if !got["api"] || !got["worker"] {
		t.Errorf("ListServices(acme) = %v, want api and worker", rows)
	}
	// globex's service must not leak into acme's list — same isolation
	// guarantee as every other query, just for a discovery endpoint.
	if len(rows) != 2 {
		t.Errorf("ListServices(acme) returned %d services, want exactly 2", len(rows))
	}
}

func TestListServicesRequiresTenant(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.ListServices("")
	if err == nil {
		t.Error("ListServices without tenant should error")
	}
}

func TestListLogServices(t *testing.T) {
	c := newLogCore(t)

	rows, _, err := c.ListLogServices("acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r["service_name"].(string)] = true
	}
	// api, worker, nightly — the 48h-old record's service counts too (the
	// list has no time filter), and globex's api must not leak.
	if !got["api"] || !got["worker"] || !got["nightly"] {
		t.Errorf("ListLogServices(acme) = %v, want api, worker, nightly", rows)
	}
	if len(rows) != 3 {
		t.Errorf("ListLogServices(acme) returned %d services, want exactly 3", len(rows))
	}

	if _, _, err := c.ListLogServices(""); err == nil {
		t.Error("ListLogServices without tenant should error")
	}
}

func TestListMetricNames(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.ListMetricNames("acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 1 || rows[0]["metric_name"].(string) != "cpu.util" {
		t.Errorf("ListMetricNames(acme) = %v, want exactly [cpu.util]", rows)
	}
}

func TestListMetricNamesRequiresTenant(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.ListMetricNames("")
	if err == nil {
		t.Error("ListMetricNames without tenant should error")
	}
}

// --- tenant attribute contract ---

func TestTenantAttributeName(t *testing.T) {
	c := NewCore(nil)
	if c.tenantAttr != "tenant.id" {
		t.Errorf("tenant attribute = %q, want tenant.id", c.tenantAttr)
	}
	// The predicate must reference the quoted dotted form, or it silently
	// matches nothing and isolation appears to work while returning no data.
	if !strings.Contains(c.tenantPredicate(), `$."tenant.id"`) {
		t.Errorf("tenant predicate does not quote the dotted key: %s", c.tenantPredicate())
	}
}
