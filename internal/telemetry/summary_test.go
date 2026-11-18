package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// summarySpan builds a span with explicit parent, timing and status, so the
// summary tests can control the call tree exactly.
func summarySpan(svc, tenant, traceID, spanID, parent, name string, start time.Time, durMs float64, status, msg string) writer.TraceSpan {
	return writer.TraceSpan{
		TraceID: traceID, SpanID: spanID, ParentSpanID: parent,
		ServiceName: svc, SpanName: name, SpanKind: "SPAN_KIND_SERVER",
		StartTime:  start.UnixMicro(),
		EndTime:    start.Add(time.Duration(durMs * float64(time.Millisecond))).UnixMicro(),
		DurationMs: durMs, StatusCode: status, StatusMessage: msg, Attributes: "{}",
		ResourceAttributes: `{"service.name":"` + svc + `","tenant.id":"` + tenant + `"}`,
		Events:             "[]", Links: "[]",
	}
}

// newSummaryCore seeds one trace with a root, five identical child SELECT
// spans, an error span, a span of a fourth service, INFO/WARN/ERROR logs (the
// WARN body longer than the 300-rune cut), a second acme trace, a trace with
// no root span, and a globex trace reusing the first trace id so tenant
// isolation is observable.
func newSummaryCore(t *testing.T) *Core {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	base := time.Now().Add(-10 * time.Minute)

	spans := []writer.TraceSpan{
		summarySpan("web", "acme", "trace-sum", "root", "", "GET /checkout", base, 120, "STATUS_CODE_OK", ""),
		summarySpan("payments", "acme", "trace-sum", "err", "root", "charge", base.Add(10*time.Millisecond), 200, "STATUS_CODE_ERROR", "card declined"),
		summarySpan("worker", "acme", "trace-sum", "job", "root", "job", base.Add(20*time.Millisecond), 5, "STATUS_CODE_OK", ""),
		summarySpan("api", "acme", "trace-other", "other-root", "", "GET /health", base.Add(30*time.Millisecond), 7, "STATUS_CODE_OK", ""),
		summarySpan("api", "acme", "trace-noroot", "orphan", "missing-parent", "GET /orphan", base.Add(40*time.Millisecond), 3, "STATUS_CODE_OK", ""),
		summarySpan("evil", "globex", "trace-sum", "evil-root", "", "steal", base, 999, "STATUS_CODE_OK", ""),
	}
	for i := 0; i < 5; i++ {
		spans = append(spans, summarySpan("db", "acme", "trace-sum", "select-"+string(rune('a'+i)), "root", "SELECT",
			base.Add(time.Duration(i+1)*time.Millisecond), float64((i+1)*10), "STATUS_CODE_OK", ""))
	}
	w.Add(spans)

	logRec := func(svc, tenant, sev string, num int32, body string, ts time.Time) writer.LogRecord {
		return writer.LogRecord{
			Timestamp: ts.UnixMicro(), ObservedTimestamp: ts.UnixMicro(),
			TraceID: "trace-sum", SeverityText: sev, SeverityNumber: num, Body: body,
			Attributes:         "{}",
			ResourceAttributes: `{"service.name":"` + svc + `","tenant.id":"` + tenant + `"}`,
			ServiceName:        svc,
		}
	}
	w.AddLogs([]writer.LogRecord{
		logRec("web", "acme", "INFO", 9, "checkout started", base.Add(time.Millisecond)),
		logRec("web", "acme", "WARN", 13, strings.Repeat("w", 310), base.Add(2*time.Millisecond)),
		logRec("worker", "acme", "ERROR", 17, "job failed", base.Add(3*time.Millisecond)),
		logRec("web", "acme", "", 0, "no severity", base.Add(4*time.Millisecond)),
		logRec("evil", "globex", "ERROR", 17, "globex only", base.Add(time.Millisecond)),
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

func TestTraceSummaryAggregates(t *testing.T) {
	c := newSummaryCore(t)
	s, err := c.SummarizeTrace("trace-sum", "acme")
	if err != nil {
		t.Fatal(err)
	}

	if s.SpanCount != 8 {
		t.Errorf("span_count = %d, want 8 (globex span must not leak)", s.SpanCount)
	}
	if s.ErrorCount != 1 {
		t.Errorf("error_count = %d, want 1", s.ErrorCount)
	}
	if s.Root == nil || s.Root.SpanID != "root" || s.RootMissing {
		t.Fatalf("root = %+v missing=%v, want the parentless root", s.Root, s.RootMissing)
	}
	if s.DurationMs != 210 {
		t.Errorf("duration_ms = %v, want 210 (max end - min start)", s.DurationMs)
	}

	if len(s.Services) != 4 || s.Services[0].ServiceName != "db" || s.Services[0].Spans != 5 {
		t.Fatalf("services = %+v, want db with 5 spans first", s.Services)
	}

	// The root is reported as root, not as a group of its own.
	if s.GroupsTotal != 3 || len(s.Groups) != 3 {
		t.Fatalf("groups_total = %d len = %d, want 3 (root excluded)", s.GroupsTotal, len(s.Groups))
	}
	for _, g := range s.Groups {
		if g.SpanName == s.Root.SpanName && g.ServiceName == s.Root.ServiceName {
			t.Errorf("groups contain the root: %+v", g)
		}
	}
	if s.Groups[0].ServiceName != "payments" || s.Groups[0].TotalMs != 200 {
		t.Errorf("first group = %+v, want payments/charge", s.Groups[0])
	}
	selectGroup := s.Groups[1]
	if selectGroup.ServiceName != "db" || selectGroup.SpanName != "SELECT" ||
		selectGroup.Count != 5 || selectGroup.TotalMs != 150 || selectGroup.MaxMs != 50 {
		t.Errorf("SELECT group = %+v, want db/SELECT x5 total 150 max 50", selectGroup)
	}

	if len(s.Slowest) != 7 {
		t.Fatalf("slowest = %d spans, want 7 (root excluded)", len(s.Slowest))
	}
	for _, sp := range s.Slowest {
		if sp.SpanID == "root" {
			t.Errorf("slowest contains the root")
		}
	}
	if s.Slowest[0].SpanID != "err" || s.Slowest[0].DurationMs != 200 {
		t.Errorf("slowest[0] = %+v, want the error span", s.Slowest[0])
	}

	if len(s.Errors) != 1 || s.Errors[0].SpanID != "err" || s.Errors[0].StatusMessage != "card declined" {
		t.Fatalf("errors = %+v, want the charge span", s.Errors)
	}

	if s.Logs.Count != 4 {
		t.Errorf("log count = %d, want 4", s.Logs.Count)
	}
	wantSev := map[string]int{"INFO": 1, "WARN": 1, "ERROR": 1, "UNSET": 1}
	for k, v := range wantSev {
		if s.Logs.BySeverity[k] != v {
			t.Errorf("by_severity[%s] = %d, want %d (%v)", k, s.Logs.BySeverity[k], v, s.Logs.BySeverity)
		}
	}
	if len(s.Logs.Problems) != 2 {
		t.Fatalf("problems = %d, want WARN and ERROR only", len(s.Logs.Problems))
	}
	if s.Logs.Problems[0].SeverityText != "WARN" || s.Logs.Problems[1].SeverityText != "ERROR" {
		t.Errorf("problems order = %q, %q, want oldest first", s.Logs.Problems[0].SeverityText, s.Logs.Problems[1].SeverityText)
	}
	body := []rune(s.Logs.Problems[0].Body)
	if len(body) != 303 || !strings.HasSuffix(s.Logs.Problems[0].Body, "...") {
		t.Errorf("long body = %d runes, want 300 plus ...", len(body))
	}
}

func TestTraceSummaryBriefLimits(t *testing.T) {
	c := newSummaryCore(t)
	sums, err := c.TraceSummaries("acme", []string{"trace-sum"}, time.Time{}, BriefSummary)
	if err != nil {
		t.Fatal(err)
	}
	s := sums[0]
	if len(s.Groups) != 3 || s.GroupsTotal != 3 {
		t.Errorf("groups len = %d total = %d, want 3 of 3", len(s.Groups), s.GroupsTotal)
	}
	capped, err := c.TraceSummaries("acme", []string{"trace-sum"}, time.Time{}, SummaryLimits{Groups: 2})
	if err != nil {
		t.Fatal(err)
	}
	if g := capped[0]; len(g.Groups) != 2 || g.GroupsTotal != 3 {
		t.Errorf("capped groups len = %d total = %d, want 2 of 3", len(g.Groups), g.GroupsTotal)
	}
	if len(s.Slowest) != 0 {
		t.Errorf("brief slowest = %d, want 0", len(s.Slowest))
	}
	if len(s.Errors) != 1 {
		t.Errorf("brief errors = %d, want 1", len(s.Errors))
	}
	if len(s.Logs.Problems) != 1 {
		t.Errorf("brief problems = %d, want 1", len(s.Logs.Problems))
	}
	if len(s.Services) != 4 {
		t.Errorf("brief services = %d, want all 4", len(s.Services))
	}
}

func TestTraceSummaryRootMissing(t *testing.T) {
	c := newSummaryCore(t)
	s, err := c.SummarizeTrace("trace-noroot", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if s.Root == nil || !s.RootMissing || s.Root.SpanID != "orphan" {
		t.Fatalf("root = %+v missing=%v, want earliest span with root_missing", s.Root, s.RootMissing)
	}
}

func TestTraceSummaryUnknownIDSerializesEmptySlices(t *testing.T) {
	c := newSummaryCore(t)
	s, err := c.SummarizeTrace("does-not-exist", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if s.SpanCount != 0 || s.Root != nil {
		t.Fatalf("unknown id: span_count=%d root=%+v, want 0 and nil", s.SpanCount, s.Root)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"root":null`, `"services":[]`, `"groups":[]`, `"slowest":[]`,
		`"errors":[]`, `"problems":[]`, `"by_severity":{}`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("unknown id JSON lacks %s: %s", want, b)
		}
	}
}

func TestTraceSummaryTruncatesPerTrace(t *testing.T) {
	old := maxSpansPerTrace
	maxSpansPerTrace = 2
	defer func() { maxSpansPerTrace = old }()

	c := newSummaryCore(t)
	s, err := c.SummarizeTrace("trace-sum", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Truncated {
		t.Error("truncated = false with maxSpansPerTrace=2, want true")
	}
	// span_count is the trace's real size; only the aggregation is capped.
	if s.SpanCount != 8 {
		t.Errorf("span_count = %d, want the trace's 8 spans", s.SpanCount)
	}
}

func TestTraceSummariesOrdersAndDeduplicatesIDs(t *testing.T) {
	c := newSummaryCore(t)
	sums, err := c.TraceSummaries("acme", []string{"trace-other", "trace-sum", "trace-other"}, time.Time{}, FullSummary)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 3 {
		t.Fatalf("got %d summaries, want one per requested id (including duplicates)", len(sums))
	}
	if sums[0].TraceID != "trace-other" || sums[1].TraceID != "trace-sum" || sums[2].TraceID != "trace-other" {
		t.Errorf("order = %q, %q, %q, want request order", sums[0].TraceID, sums[1].TraceID, sums[2].TraceID)
	}
}

func TestTraceSummaryTenantIsolation(t *testing.T) {
	c := newSummaryCore(t)
	s, err := c.SummarizeTrace("trace-sum", "globex")
	if err != nil {
		t.Fatal(err)
	}
	if s.SpanCount != 1 || s.Root == nil || s.Root.ServiceName != "evil" {
		t.Fatalf("globex summary = %+v, want only its own span", s)
	}
	if s.Logs.Count != 1 || s.Logs.Problems[0].Body != "globex only" {
		t.Errorf("globex logs leaked or missing: %+v", s.Logs)
	}
}

func TestTraceSummaryValidation(t *testing.T) {
	c := newSummaryCore(t)
	if _, err := c.TraceSummaries("acme", nil, time.Time{}, FullSummary); !IsInvalidInput(err) {
		t.Errorf("no ids: %v, want invalid input", err)
	}
	if _, err := c.TraceSummaries("acme", []string{""}, time.Time{}, FullSummary); !IsInvalidInput(err) {
		t.Errorf("empty id: %v, want invalid input", err)
	}
	if _, err := c.TraceSummaries("", []string{"trace-sum"}, time.Time{}, FullSummary); !IsInvalidInput(err) {
		t.Errorf("no tenant: %v, want invalid input", err)
	}
	if _, err := c.SummarizeTrace("", "acme"); !IsInvalidInput(err) {
		t.Errorf("SummarizeTrace empty id: %v, want invalid input", err)
	}
}

// newSlowMetricCore seeds exemplars whose trace ids have matching spans, so
// the brief summaries in SlowRequests have something to summarise.
func newSlowMetricCore(t *testing.T) *Core {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	base := time.Now().Add(-5 * time.Minute)

	metric := writer.MetricPoint{
		MetricName: "http.server.duration", MetricType: "histogram", MetricUnit: "ms",
		Timestamp: base.UnixMicro(), StartTimestamp: base.Add(-time.Hour).UnixMicro(),
		Count: 10, Sum: 100, Min: 1, Max: 500, BucketCounts: "[5,4,1]", ExplicitBounds: "[10,100]",
		AggregationTemporality: temporalityCumulative,
		Attributes:             `{}`,
		ResourceAttributes:     `{"service.name":"web","tenant.id":"acme"}`,
		ServiceName:            "web",
		Exemplars: `[{"time_unix_nano":` + itoa(base.Add(time.Minute).UnixNano()) + `,"trace_id":"t-slow","span_id":"s1","value":420},` +
			`{"time_unix_nano":` + itoa(base.Add(2*time.Minute).UnixNano()) + `,"trace_id":"t-b","span_id":"s2","value":70},` +
			`{"time_unix_nano":` + itoa(base.Add(3*time.Minute).UnixNano()) + `,"trace_id":"t-mid","span_id":"s3","value":60},` +
			`{"time_unix_nano":` + itoa(base.Add(4*time.Minute).UnixNano()) + `,"trace_id":"t-fast","span_id":"s4","value":4}]`,
	}
	w.AddMetrics([]writer.MetricPoint{metric})
	w.Add([]writer.TraceSpan{
		summarySpan("web", "acme", "t-slow", "sp-t-slow", "", "GET /slow", base, 400, "STATUS_CODE_OK", ""),
		summarySpan("web", "acme", "t-b", "sp-t-b", "", "GET /b", base, 70, "STATUS_CODE_OK", ""),
		summarySpan("web", "acme", "t-mid", "sp-t-mid", "", "GET /mid", base, 60, "STATUS_CODE_OK", ""),
		summarySpan("web", "acme", "t-fast", "sp-t-fast", "", "GET /fast", base, 4, "STATUS_CODE_OK", ""),
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

func TestSlowRequestsFromMetricExemplars(t *testing.T) {
	c := newSlowMetricCore(t)
	rows, err := c.SlowRequests("acme", SlowOpts{MetricName: "http.server.duration"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d slow requests, want 4 exemplars", len(rows))
	}
	wantTraces := []string{"t-slow", "t-b", "t-mid", "t-fast"}
	wantValues := []float64{420, 70, 60, 4}
	for i := range rows {
		if rows[i].TraceID != wantTraces[i] || rows[i].Value != wantValues[i] {
			t.Errorf("row %d = %s/%v, want %s/%v", i, rows[i].TraceID, rows[i].Value, wantTraces[i], wantValues[i])
		}
		if rows[i].Unit != "ms" {
			t.Errorf("row %d unit = %q, want the metric's ms", i, rows[i].Unit)
		}
		if rows[i].Trace.SpanCount != 1 || rows[i].Trace.Root == nil {
			t.Errorf("row %d has no brief trace summary: %+v", i, rows[i].Trace)
		}
	}
}

func TestSlowRequestsMetricFiltersAndRange(t *testing.T) {
	c := newSlowMetricCore(t)
	rows, err := c.SlowRequests("acme", SlowOpts{
		MetricName: "http.server.duration",
		Value:      Range{Min: ptr(50), Max: ptr(100)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].TraceID != "t-b" || rows[1].TraceID != "t-mid" {
		t.Fatalf("range 50..100: got %+v, want t-b then t-mid", rows)
	}

	rows, err = c.SlowRequests("acme", SlowOpts{MetricName: "http.server.duration", ServiceName: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Errorf("service web: got %d, want 4", len(rows))
	}
	rows, err = c.SlowRequests("acme", SlowOpts{MetricName: "http.server.duration", ServiceName: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("service other: got %d, want 0", len(rows))
	}

	rows, err = c.SlowRequests("acme", SlowOpts{MetricName: "http.server.duration", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TraceID != "t-slow" {
		t.Errorf("limit 1: got %+v", rows)
	}

	if _, err := c.SlowRequests("acme", SlowOpts{
		MetricName: "http.server.duration",
		Value:      Range{Min: ptr(10), Max: ptr(1)},
	}); !IsInvalidInput(err) {
		t.Errorf("min above max: %v, want invalid input", err)
	}
}

func TestSlowRequestsFromRootSpans(t *testing.T) {
	c := newCorrelationCore(t)
	rows, err := c.SlowRequests("acme", SlowOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5 root spans (default limit)", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Value < rows[i].Value {
			t.Errorf("not slowest first: %v then %v", rows[i-1].Value, rows[i].Value)
		}
	}
	if rows[0].Unit != "ms" || rows[0].Value != 900 {
		t.Errorf("first row = %+v, want 900 ms", rows[0])
	}

	rows, err = c.SlowRequests("acme", SlowOpts{Value: Range{Min: ptr(250), Max: ptr(500)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("250..500: got %d rows, want 3", len(rows))
	}
	if _, err := c.SlowRequests("acme", SlowOpts{Value: Range{Min: ptr(10), Max: ptr(1)}}); !IsInvalidInput(err) {
		t.Errorf("min above max: %v, want invalid input", err)
	}
}

func TestSlowRequestsIgnoresChildSpans(t *testing.T) {
	c := newSummaryCore(t)
	rows, err := c.SlowRequests("acme", SlowOpts{ServiceName: "payments"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("child span returned as a request: %+v", rows)
	}
	rows, err = c.SlowRequests("acme", SlowOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the two root spans", len(rows))
	}
	if rows[0].TraceID != "trace-sum" || rows[0].Value != 120 {
		t.Errorf("first row = %+v, want trace-sum's root at 120 ms", rows[0])
	}
	if rows[0].Trace.Logs.Problems == nil || len(rows[0].Trace.Logs.Problems) != 1 {
		t.Errorf("brief summary should carry the first WARN+ log: %+v", rows[0].Trace.Logs)
	}
}

func TestSlowRequestsLimitDefaultAndCap(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	base := time.Now().Add(-5 * time.Minute)
	var spans []writer.TraceSpan
	for i := 1; i <= 25; i++ {
		spans = append(spans, summarySpan("api", "acme", "trace-"+itoa(int64(i)), "root-"+itoa(int64(i)), "", "op",
			base.Add(time.Duration(i)*time.Millisecond), float64(i), "STATUS_CODE_OK", ""))
	}
	w.Add(spans)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	c := NewCore(engine)

	rows, err := c.SlowRequests("acme", SlowOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || rows[0].Value != 25 {
		t.Errorf("default limit: got %d rows first %v, want 5 and 25", len(rows), rows[0].Value)
	}

	rows, err = c.SlowRequests("acme", SlowOpts{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 20 || rows[0].Value != 25 {
		t.Errorf("limit cap: got %d rows, want 20", len(rows))
	}
}
