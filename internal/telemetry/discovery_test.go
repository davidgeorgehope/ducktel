package telemetry

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// TestDiscoveryListsWholeHistory: the pickers list every tenant, service
// and metric ever stored, not only recent ones. A tenant that went quiet
// months ago must still be selectable, together with its services and
// metrics (a 7-day bound once hid every tenant but the active one, and left
// an old tenant's lists empty).
func TestDiscoveryListsWholeHistory(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	old := time.Now().Add(-90 * 24 * time.Hour).UnixMicro()
	now := time.Now().UnixMicro()
	span := func(svc, tenant string, ts int64) writer.TraceSpan {
		return writer.TraceSpan{
			TraceID: "t-" + svc, SpanID: "s-" + svc, ServiceName: svc, SpanName: "op",
			StartTime: ts, EndTime: ts, DurationMs: 1, StatusCode: "STATUS_CODE_OK",
			Attributes: "{}", ResourceAttributes: `{"tenant.id":"` + tenant + `"}`,
			Events: "[]", Links: "[]",
		}
	}
	w.Add([]writer.TraceSpan{span("fresh", "acme", now), span("quiet", "oldco", old)})
	w.AddMetrics([]writer.MetricPoint{
		{MetricName: "old.metric", MetricType: "gauge", Timestamp: old, ResourceAttributes: `{"tenant.id":"oldco"}`},
	})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	c := NewCore(engine)

	values := func(rows []map[string]any, col string) []any {
		var out []any
		for _, r := range rows {
			out = append(out, r[col])
		}
		return out
	}

	rows, _, err := c.ListTenants()
	if err != nil {
		t.Fatal(err)
	}
	if got := values(rows, "tenant"); len(got) != 2 || got[0] != "acme" || got[1] != "oldco" {
		t.Errorf("tenants = %v, want [acme oldco]", got)
	}
	rows, _, err = c.ListServices("oldco")
	if err != nil {
		t.Fatal(err)
	}
	if got := values(rows, "service_name"); len(got) != 1 || got[0] != "quiet" {
		t.Errorf("oldco services = %v, want [quiet]", got)
	}
	rows, _, err = c.ListMetricNames("oldco")
	if err != nil {
		t.Fatal(err)
	}
	if got := values(rows, "metric_name"); len(got) != 1 || got[0] != "old.metric" {
		t.Errorf("oldco metric names = %v, want [old.metric]", got)
	}
}

// TestDiscoveryIsCached: a second call within discoveryTTL is served from the
// cache, so a reload does not re-scan. Proved by closing the engine: a cached
// answer still comes back, an uncached one (other tenant) errors.
func TestDiscoveryIsCached(t *testing.T) {
	c := newTestCore(t)
	first, _, err := c.ListServices("acme")
	if err != nil {
		t.Fatal(err)
	}
	c.engine.Close()

	again, _, err := c.ListServices("acme")
	if err != nil {
		t.Fatalf("cached call hit the closed engine: %v", err)
	}
	if len(again) != len(first) {
		t.Errorf("cached result differs: %d vs %d rows", len(again), len(first))
	}
	if _, _, err := c.ListServices("globex"); err == nil {
		t.Error("uncached tenant should have queried the (closed) engine and failed")
	}
}

// TestDiscoveryDoesNotCacheEmpty: an empty list is not a stable answer.
// Closing the engine must make the next call fail, which it would not if the
// empty result had been cached past a flush.
func TestDiscoveryDoesNotCacheEmpty(t *testing.T) {
	c := newTestCore(t)
	rows, _, err := c.ListServices("nobody")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("nobody has services: %v", rows)
	}
	c.engine.Close()
	if _, _, err := c.ListServices("nobody"); err == nil {
		t.Error("empty discovery result was served after the engine closed")
	}
}

func attrSpan(svc, traceID, spanID string, ts time.Time, attrs, resource string) writer.TraceSpan {
	return writer.TraceSpan{
		TraceID: traceID, SpanID: spanID, ServiceName: svc, SpanName: "op",
		SpanKind: "SPAN_KIND_SERVER", StartTime: ts.UnixMicro(), EndTime: ts.UnixMicro(),
		DurationMs: 1, StatusCode: "STATUS_CODE_OK", Attributes: attrs,
		ResourceAttributes: resource, Events: "[]", Links: "[]",
	}
}

func tenantResource(svc, tenant, extra string) string {
	body := `"service.name":"` + svc + `","tenant.id":"` + tenant + `"`
	if extra != "" {
		body += "," + extra
	}
	return "{" + body + "}"
}

func newAttributeKeyCore(t *testing.T) *Core {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	w.Add([]writer.TraceSpan{
		attrSpan("api", "t1", "s1", now, `{"http.request.method":"GET","shared.key":"1"}`, tenantResource("api", "acme", `"region":"us","shared.key":"1"`)),
		attrSpan("worker", "t2", "s2", now, `{"job.id":"sync"}`, tenantResource("worker", "acme", "")),
		attrSpan("api", "t3", "s3", old, `{"ancient.key":"1"}`, tenantResource("api", "acme", "")),
		attrSpan("api", "t4", "s4", now, `not-json`, tenantResource("api", "acme", "")),
		attrSpan("api", "tg", "sg", now, `{"globex.only":"1"}`, tenantResource("api", "globex", "")),
	})
	w.AddLogs([]writer.LogRecord{{
		Timestamp: now.UnixMicro(), ServiceName: "api", SeverityText: "INFO", Body: "ok",
		Attributes: `{"log.only":"1"}`, ResourceAttributes: tenantResource("api", "acme", ""),
	}})
	w.AddMetrics([]writer.MetricPoint{{
		MetricName: "cpu.util", MetricType: "gauge", Timestamp: now.UnixMicro(),
		ValueDouble: 0.5, ServiceName: "api",
		Attributes: `{"dp.kind":"gauge"}`, ResourceAttributes: tenantResource("api", "acme", ""),
	}})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return NewCore(engine)
}

func hasAttrKey(rows []map[string]any, signal, source, key string) bool {
	for _, r := range rows {
		if r["signal"] == signal && r["source"] == source && r["key"] == key {
			return true
		}
	}
	return false
}

func attrKeysInOrder(rows []map[string]any) bool {
	prev := ""
	for _, r := range rows {
		cur := fmt.Sprint(r["signal"]) + "\x00" + fmt.Sprint(r["source"]) + "\x00" + fmt.Sprint(r["key"])
		if cur < prev {
			return false
		}
		prev = cur
	}
	return true
}

func TestListAttributeKeys(t *testing.T) {
	c := newAttributeKeyCore(t)

	rows, cols, err := c.ListAttributeKeys("acme", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 3 || cols[0] != "signal" || cols[1] != "source" || cols[2] != "key" {
		t.Fatalf("columns = %v, want [signal source key]", cols)
	}
	if !attrKeysInOrder(rows) {
		t.Errorf("keys are not ordered by signal, source, key: %v", rows)
	}
	for _, want := range []struct{ signal, source, key string }{
		{"traces", "attributes", "http.request.method"},
		{"traces", "attributes", "shared.key"},
		{"traces", "resource_attributes", "shared.key"},
		{"traces", "resource_attributes", "region"},
		{"traces", "resource_attributes", "tenant.id"},
		{"logs", "attributes", "log.only"},
		{"metrics", "attributes", "dp.kind"},
	} {
		if !hasAttrKey(rows, want.signal, want.source, want.key) {
			t.Errorf("missing %s %s %s", want.signal, want.source, want.key)
		}
	}
	if hasAttrKey(rows, "traces", "attributes", "globex.only") ||
		hasAttrKey(rows, "logs", "attributes", "globex.only") ||
		hasAttrKey(rows, "metrics", "attributes", "globex.only") {
		t.Errorf("globex key leaked into acme: %v", rows)
	}

	logs, _, err := c.ListAttributeKeys("acme", "logs", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAttrKey(logs, "logs", "attributes", "log.only") {
		t.Errorf("signal logs missing log.only: %v", logs)
	}
	if hasAttrKey(logs, "traces", "attributes", "http.request.method") {
		t.Errorf("signal logs returned a trace key: %v", logs)
	}

	traces, _, err := c.ListAttributeKeys("acme", "traces", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasAttrKey(traces, "logs", "attributes", "log.only") {
		t.Errorf("signal traces returned log.only: %v", traces)
	}
	if !hasAttrKey(traces, "traces", "attributes", "http.request.method") {
		t.Errorf("signal traces missing http.request.method: %v", traces)
	}

	metrics, _, err := c.ListAttributeKeys("acme", "metrics", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAttrKey(metrics, "metrics", "attributes", "dp.kind") {
		t.Errorf("signal metrics missing dp.kind: %v", metrics)
	}

	recent, _, err := c.ListAttributeKeys("acme", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasAttrKey(recent, "traces", "attributes", "ancient.key") {
		t.Errorf("default window returned ancient.key: %v", recent)
	}
	wide, _, err := c.ListAttributeKeys("acme", "", "", 180, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAttrKey(wide, "traces", "attributes", "ancient.key") {
		t.Errorf("180 minute window missing ancient.key: %v", wide)
	}

	api, _, err := c.ListAttributeKeys("acme", "", "api", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAttrKey(api, "traces", "attributes", "http.request.method") || hasAttrKey(api, "traces", "attributes", "job.id") {
		t.Errorf("service api keys = %v", api)
	}
	worker, _, err := c.ListAttributeKeys("acme", "", "worker", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAttrKey(worker, "traces", "attributes", "job.id") || hasAttrKey(worker, "traces", "attributes", "http.request.method") {
		t.Errorf("service worker keys = %v", worker)
	}

	limited, _, err := c.ListAttributeKeys("acme", "traces", "", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	limitedSource := map[string]int{}
	for _, r := range limited {
		limitedSource[fmt.Sprint(r["source"])]++
	}
	if limitedSource["attributes"] != 1 || limitedSource["resource_attributes"] != 1 {
		t.Errorf("limit 1 on traces = %v, want one key from each source", limited)
	}
	// logs sorts before metrics and traces. A single limit over that order
	// would return only the log key and hide the other signals.
	shared, _, err := c.ListAttributeKeys("acme", "", "", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	perSignal := map[string]int{}
	perSource := map[string]int{}
	for _, r := range shared {
		signal := fmt.Sprint(r["signal"])
		perSignal[signal]++
		perSource[signal+"\x00"+fmt.Sprint(r["source"])]++
	}
	for _, signal := range []string{"traces", "logs", "metrics"} {
		if perSignal[signal] == 0 {
			t.Errorf("limit 1 with signal omitted dropped %s: %v", signal, shared)
		}
	}
	for group, n := range perSource {
		if n > 1 {
			t.Errorf("limit 1 returned %d keys for %s: %v", n, group, shared)
		}
	}

	nobody, _, err := c.ListAttributeKeys("nobody", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nobody) != 0 {
		t.Errorf("tenant with no data returned %d rows, want 0", len(nobody))
	}

	if _, _, err := c.ListAttributeKeys("", "", "", 0, 0); !IsInvalidInput(err) {
		t.Fatalf("empty tenant error = %v, want invalid input", err)
	}
	_, _, err = c.ListAttributeKeys("acme", "spans", "", 0, 0)
	if !IsInvalidInput(err) {
		t.Fatalf("unknown signal error = %v, want invalid input", err)
	}
	const wantSignal = `signal "spans" is not supported; use "traces", "logs", or "metrics", or omit it for all three`
	if err.Error() != wantSignal {
		t.Errorf("signal error = %q, want %q", err.Error(), wantSignal)
	}
}

// TestListAttributeKeysLimitIsPerSignal: logs sort before metrics and traces.
// More log keys than the limit must not consume the budget for the other signals.
func TestListAttributeKeysLimitIsPerSignal(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	now := time.Now()
	const limit = 3
	const logKeys = limit + 5
	parts := make([]string, logKeys)
	for i := range parts {
		parts[i] = fmt.Sprintf(`"log.key.%02d":"1"`, i)
	}
	w.AddLogs([]writer.LogRecord{{
		Timestamp: now.UnixMicro(), ServiceName: "api", SeverityText: "INFO", Body: "ok",
		Attributes: "{" + strings.Join(parts, ",") + "}", ResourceAttributes: tenantResource("api", "acme", ""),
	}})
	w.Add([]writer.TraceSpan{
		attrSpan("api", "t1", "s1", now, `{"span.only":"1"}`, tenantResource("api", "acme", "")),
	})
	w.AddMetrics([]writer.MetricPoint{{
		MetricName: "cpu.util", MetricType: "gauge", Timestamp: now.UnixMicro(),
		ValueDouble: 0.5, ServiceName: "api",
		Attributes: `{"metric.only":"1"}`, ResourceAttributes: tenantResource("api", "acme", ""),
	}})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })

	rows, _, err := NewCore(engine).ListAttributeKeys("acme", "", "", 0, limit)
	if err != nil {
		t.Fatal(err)
	}
	logAttrs := 0
	for _, r := range rows {
		if r["signal"] == "logs" && r["source"] == "attributes" {
			logAttrs++
		}
	}
	if logAttrs != limit {
		t.Errorf("log attribute keys = %d, want the cap %d: %v", logAttrs, limit, rows)
	}
	if !hasAttrKey(rows, "logs", "resource_attributes", "tenant.id") {
		t.Errorf("log resource key missing beside %d attribute keys: %v", logKeys, rows)
	}
	if !hasAttrKey(rows, "traces", "attributes", "span.only") {
		t.Errorf("trace key missing beside %d log keys: %v", logKeys, rows)
	}
	if !hasAttrKey(rows, "metrics", "attributes", "metric.only") {
		t.Errorf("metric key missing beside %d log keys: %v", logKeys, rows)
	}
}

func TestListAttributeKeysTracesOnly(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		attrSpan("api", "t1", "s1", time.Now(), `{"http.request.method":"GET"}`, tenantResource("api", "acme", "")),
	})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })

	rows, _, err := NewCore(engine).ListAttributeKeys("acme", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAttrKey(rows, "traces", "attributes", "http.request.method") {
		t.Errorf("traces-only store missing http.request.method: %v", rows)
	}
	if !hasAttrKey(rows, "traces", "resource_attributes", "tenant.id") {
		t.Errorf("traces-only store missing tenant.id: %v", rows)
	}
}

func TestListAttributeKeysIsCached(t *testing.T) {
	c := newAttributeKeyCore(t)
	first, _, err := c.ListAttributeKeys("acme", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("seed returned no keys")
	}
	c.engine.Close()
	// The window start moves between calls. It must not be part of the cache
	// key, or this second call would miss and hit the closed engine.
	time.Sleep(5 * time.Millisecond)

	again, _, err := c.ListAttributeKeys("acme", "", "", 0, 0)
	if err != nil {
		t.Fatalf("cached call hit the closed engine: %v", err)
	}
	if len(again) != len(first) {
		t.Errorf("cached result differs: %d vs %d rows", len(again), len(first))
	}
	if _, _, err := c.ListAttributeKeys("globex", "", "", 0, 0); err == nil {
		t.Error("uncached tenant should have queried the closed engine and failed")
	}
}
