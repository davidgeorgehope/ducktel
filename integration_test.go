package ducktel_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collectlogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectmetricsv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collecttracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/davidgeorgehope/ducktel/internal/cli"
	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/receiver"
	"github.com/davidgeorgehope/ducktel/internal/testharness"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func startTestServer(t *testing.T) (string, *writer.Writer, func()) {
	t.Helper()
	return startTestServerWithToken(t, "")
}

// startTestServerWithToken is startTestServer with an optional bearer token, so
// tests can exercise the authenticated path.
func startTestServerWithToken(t *testing.T, token string) (string, *writer.Writer, func()) {
	t.Helper()

	dataDir, err := os.MkdirTemp("", "ducktel-test-*")
	if err != nil {
		t.Fatal(err)
	}

	w := writer.New(dataDir, 1*time.Hour, 1000)
	w.Start()

	port := 14318
	r := receiver.New("localhost", port, w, token, "", nil)
	go r.Start()

	addr := fmt.Sprintf("http://localhost:%d", port)
	for i := 0; i < 20; i++ {
		resp, err := http.Get(addr + "/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cleanup := func() {
		r.Stop(context.Background())
		if err := w.Stop(); err != nil {
			t.Errorf("final flush: %v", err)
		}
		os.RemoveAll(dataDir)
	}

	return addr, w, cleanup
}

func testResource() *resourcev1.Resource {
	return &resourcev1.Resource{
		Attributes: []*commonv1.KeyValue{
			{Key: "service.name", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "test-service"}}},
			{Key: "host.name", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "localhost"}}},
		},
	}
}

func sendProto(t *testing.T, addr, path string, msg proto.Message) {
	t.Helper()
	body, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	resp, err := http.Post(addr+path, "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("sending to %s: %v", path, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s returned status %d", path, resp.StatusCode)
	}
}

func TestTraces(t *testing.T) {
	addr, w, cleanup := startTestServer(t)
	defer cleanup()
	dataDir := w.DataDir()

	now := uint64(time.Now().UnixNano())
	sendProto(t, addr, "/v1/traces", &collecttracev1.ExportTraceServiceRequest{
		ResourceSpans: []*tracev1.ResourceSpans{
			{
				Resource: testResource(),
				ScopeSpans: []*tracev1.ScopeSpans{
					{
						Spans: []*tracev1.Span{
							{
								TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
								SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
								Name:              "test-span",
								Kind:              tracev1.Span_SPAN_KIND_SERVER,
								StartTimeUnixNano: now - 100_000_000,
								EndTimeUnixNano:   now,
								Status:            &tracev1.Status{Code: tracev1.Status_STATUS_CODE_OK},
								Attributes: []*commonv1.KeyValue{
									{Key: "http.method", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "GET"}}},
								},
							},
						},
					},
				},
			},
		},
	})

	w.Flush()

	engine, err := query.Open(dataDir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()

	// Verify trace stored
	results, _, err := engine.Query("SELECT span_name, status_code, duration_ms, resource_attributes FROM traces")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 span, got %d", len(results))
	}
	if results[0]["span_name"] != "test-span" {
		t.Errorf("span_name = %v", results[0]["span_name"])
	}
	// Verify resource attributes preserved
	ra, _ := results[0]["resource_attributes"].(string)
	if ra == "" || ra == "{}" {
		t.Errorf("resource_attributes should not be empty, got %v", ra)
	}
}

func TestLogs(t *testing.T) {
	addr, w, cleanup := startTestServer(t)
	defer cleanup()
	dataDir := w.DataDir()

	now := uint64(time.Now().UnixNano())
	sendProto(t, addr, "/v1/logs", &collectlogsv1.ExportLogsServiceRequest{
		ResourceLogs: []*logsv1.ResourceLogs{
			{
				Resource: testResource(),
				ScopeLogs: []*logsv1.ScopeLogs{
					{
						LogRecords: []*logsv1.LogRecord{
							{
								TimeUnixNano:         now,
								ObservedTimeUnixNano: now,
								SeverityNumber:       logsv1.SeverityNumber_SEVERITY_NUMBER_ERROR,
								SeverityText:         "ERROR",
								Body:                 &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "something went wrong"}},
								TraceId:              []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
								SpanId:               []byte{1, 2, 3, 4, 5, 6, 7, 8},
								Attributes: []*commonv1.KeyValue{
									{Key: "error.type", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "RuntimeError"}}},
								},
							},
							{
								TimeUnixNano:         now + 1000,
								ObservedTimeUnixNano: now + 1000,
								SeverityNumber:       logsv1.SeverityNumber_SEVERITY_NUMBER_INFO,
								SeverityText:         "INFO",
								Body:                 &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "request processed"}},
							},
						},
					},
				},
			},
		},
	})

	w.Flush()

	engine, err := query.Open(dataDir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()

	// Verify logs stored
	results, _, err := engine.Query("SELECT service_name, severity_text, body, trace_id FROM logs ORDER BY timestamp")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 logs, got %d", len(results))
	}
	if results[0]["severity_text"] != "ERROR" {
		t.Errorf("first log severity = %v, want ERROR", results[0]["severity_text"])
	}
	if results[0]["body"] != "something went wrong" {
		t.Errorf("first log body = %v", results[0]["body"])
	}
	if results[0]["service_name"] != "test-service" {
		t.Errorf("service_name = %v", results[0]["service_name"])
	}
	if results[1]["severity_text"] != "INFO" {
		t.Errorf("second log severity = %v, want INFO", results[1]["severity_text"])
	}
}

func TestMetrics(t *testing.T) {
	addr, w, cleanup := startTestServer(t)
	defer cleanup()
	dataDir := w.DataDir()

	now := uint64(time.Now().UnixNano())
	sendProto(t, addr, "/v1/metrics", &collectmetricsv1.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricsv1.ResourceMetrics{
			{
				Resource: testResource(),
				ScopeMetrics: []*metricsv1.ScopeMetrics{
					{
						Metrics: []*metricsv1.Metric{
							{
								Name:        "http.request.duration",
								Description: "Duration of HTTP requests",
								Unit:        "ms",
								Data: &metricsv1.Metric_Histogram{
									Histogram: &metricsv1.Histogram{
										AggregationTemporality: metricsv1.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
										DataPoints: []*metricsv1.HistogramDataPoint{
											{
												TimeUnixNano:   now,
												Count:          10,
												Sum:            ptrFloat64(1500.0),
												Min:            ptrFloat64(50.0),
												Max:            ptrFloat64(500.0),
												BucketCounts:   []uint64{2, 5, 2, 1},
												ExplicitBounds: []float64{100, 200, 400},
												Attributes: []*commonv1.KeyValue{
													{Key: "http.method", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "POST"}}},
												},
											},
										},
									},
								},
							},
							{
								Name: "system.cpu.utilization",
								Unit: "1",
								Data: &metricsv1.Metric_Gauge{
									Gauge: &metricsv1.Gauge{
										DataPoints: []*metricsv1.NumberDataPoint{
											{
												TimeUnixNano: now,
												Value:        &metricsv1.NumberDataPoint_AsDouble{AsDouble: 0.75},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	})

	w.Flush()

	engine, err := query.Open(dataDir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()

	// Verify metrics stored
	results, _, err := engine.Query("SELECT metric_name, metric_type, service_name, value_double, count, sum FROM metrics ORDER BY metric_name")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 metric points, got %d", len(results))
	}

	// Histogram
	if results[0]["metric_name"] != "http.request.duration" {
		t.Errorf("metric_name = %v", results[0]["metric_name"])
	}
	if results[0]["metric_type"] != "histogram" {
		t.Errorf("metric_type = %v", results[0]["metric_type"])
	}

	// Gauge
	if results[1]["metric_name"] != "system.cpu.utilization" {
		t.Errorf("metric_name = %v", results[1]["metric_name"])
	}
	if results[1]["metric_type"] != "gauge" {
		t.Errorf("metric_type = %v", results[1]["metric_type"])
	}
	if results[1]["value_double"] != 0.75 {
		t.Errorf("gauge value = %v, want 0.75", results[1]["value_double"])
	}
}

func ptrFloat64(f float64) *float64 {
	return &f
}

// seedSpans writes two spans with distinct services and flushes them, giving
// the injection tests a populated store to attack.
func seedSpans(t *testing.T, dir string) {
	t.Helper()
	w := writer.New(dir, 1*time.Hour, 1000)
	now := time.Now()
	w.Add([]writer.TraceSpan{
		{TraceID: "t1", SpanID: "s1", ServiceName: "svc-a", SpanName: "op",
			StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 1,
			StatusCode: "STATUS_CODE_OK", Attributes: "{}", ResourceAttributes: "{}", Events: "[]", Links: "[]"},
		{TraceID: "t2", SpanID: "s2", ServiceName: "svc-b", SpanName: "op",
			StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 2,
			StatusCode: "STATUS_CODE_ERROR", Attributes: "{}", ResourceAttributes: "{}", Events: "[]", Links: "[]"},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding spans: %v", err)
	}
}

// TestQueryBindsParameters verifies that values passed as args are treated as
// data, not SQL. Before parameterization the filters interpolated values into
// the statement, so an OR-payload widened the result set and a stacked DROP
// VIEW destroyed the database.
func TestQueryBindsParameters(t *testing.T) {
	dir := t.TempDir()
	seedSpans(t, dir)

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()

	// A legitimate filter still works and matches exactly one row.
	results, _, err := engine.Query("SELECT service_name FROM traces WHERE service_name = ?", "svc-a")
	if err != nil {
		t.Fatalf("legit query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("legit filter returned %d rows, want 1", len(results))
	}

	// Every payload must be treated as a literal value: no rows match, and
	// crucially no stacked statement executes.
	payloads := []string{
		"svc-a' OR '1'='1",
		"' UNION SELECT name FROM sqlite_master -- ",
		"x'; DROP VIEW traces; --",
		"' UNION SELECT column0, column0, column0, column0, 0, 0, column0 FROM read_csv('/etc/passwd') -- ",
	}
	for _, p := range payloads {
		got, _, err := engine.Query("SELECT service_name FROM traces WHERE service_name = ?", p)
		if err != nil {
			t.Errorf("payload %q returned error: %v", p, err)
			continue
		}
		if len(got) != 0 {
			t.Errorf("payload %q matched %d rows, want 0 — value was interpreted as SQL", p, len(got))
		}
	}

	// The view must still be queryable after the DROP attempt.
	results, _, err = engine.Query("SELECT count(*) AS c FROM traces")
	if err != nil {
		t.Fatalf("traces view was destroyed by an injected statement: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("count query returned %d rows, want 1", len(results))
	}
}

// TestDescribeRejectsUnknownView verifies the identifier allowlist, since view
// names cannot be bound as parameters.
func TestDescribeRejectsUnknownView(t *testing.T) {
	dir := t.TempDir()
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()

	for _, v := range []string{"traces", "logs", "metrics"} {
		if _, err := engine.Describe(v); err != nil {
			t.Errorf("Describe(%q) returned error: %v", v, err)
		}
	}

	for _, v := range []string{
		"traces; DROP VIEW traces",
		"traces WHERE 1=1",
		"nonexistent",
		"",
	} {
		if _, err := engine.Describe(v); err == nil {
			t.Errorf("Describe(%q) succeeded, want rejection", v)
		}
	}
}

// TestNonFiniteFloatsAreSanitized guards against NaN/Inf reaching the store.
// OTLP permits them in float fields, but Go's JSON encoder rejects them
// ("json: unsupported value: NaN"), so without sanitizing at ingest a single
// bad measurement breaks `ducktel metrics --format json` for the whole result
// set — the CLI's default output format.
func TestNonFiniteFloatsAreSanitized(t *testing.T) {
	addr, w, cleanup := startTestServer(t)
	defer cleanup()
	dataDir := w.DataDir()

	now := fmt.Sprintf("%d", time.Now().UnixNano())

	// JSON ingestion is used because NaN/Inf are expressible as JSON strings.
	// Covers gauge, histogram min/max/sum, summary quantiles, and exemplars.
	bodies := []string{
		`{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"nan-svc"}}]},"scopeMetrics":[{"scope":{"name":"t"},"metrics":[{"name":"g","gauge":{"dataPoints":[{"timeUnixNano":"` + now + `","asDouble":"NaN"}]}}]}]}]}`,
		`{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"nan-svc"}}]},"scopeMetrics":[{"scope":{"name":"t"},"metrics":[{"name":"h","histogram":{"aggregationTemporality":2,"dataPoints":[{"timeUnixNano":"` + now + `","count":"5","sum":"NaN","min":"NaN","max":"Infinity","bucketCounts":["1"],"explicitBounds":["1"]}]}}]}]}]}`,
		`{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"nan-svc"}}]},"scopeMetrics":[{"scope":{"name":"t"},"metrics":[{"name":"s","summary":{"dataPoints":[{"timeUnixNano":"` + now + `","count":"3","sum":"NaN","quantileValues":[{"quantile":"0.5","value":"NaN"}]}]}}]}]}]}`,
	}
	for i, b := range bodies {
		resp, err := http.Post(addr+"/v1/metrics", "application/json", strings.NewReader(b))
		if err != nil {
			t.Fatalf("posting body %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("body %d returned status %d", i, resp.StatusCode)
		}
	}

	w.Flush()

	engine, err := query.Open(dataDir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()

	results, columns, err := engine.Query(
		"SELECT metric_name, value_double, sum, min, max, quantile_values, exemplars FROM metrics ORDER BY 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 metric points, got %d", len(results))
	}

	// No float in the result set may be NaN/Inf.
	for _, row := range results {
		for _, col := range []string{"value_double", "sum", "min", "max"} {
			if v, ok := row[col].(float64); ok && (math.IsNaN(v) || math.IsInf(v, 0)) {
				t.Errorf("%s of metric %v is still non-finite: %v", col, row["metric_name"], v)
			}
		}
	}

	// The actual regression: the default output format must not fail.
	if err := cli.FormatResults(io.Discard, results, columns, "json"); err != nil {
		t.Errorf("--format json failed on non-finite input: %v", err)
	}
}

// --- writer durability regression tests ---

func testSpan() writer.TraceSpan {
	now := time.Now()
	return writer.TraceSpan{
		TraceID: "t", SpanID: "s", ServiceName: "svc", SpanName: "op",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 1,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: "{}", Events: "[]", Links: "[]",
	}
}

func spanCount(t *testing.T, dir string) int {
	t.Helper()
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()
	rows, _, err := engine.Query("SELECT count(*) AS c FROM traces")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return int(rows[0]["c"].(int64))
}

// blockedDir makes writes to a signal fail by putting a regular file where the
// signal directory needs to be, returning a func that unblocks it.
func blockedDir(t *testing.T, dir, signal string) func() {
	t.Helper()
	path := filepath.Join(dir, signal)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return func() { os.Remove(path) }
}

// TestFlushFailureRetriesInsteadOfDropping covers a data-loss bug: Flush took
// the buffers and cleared them before writing, so a failed write discarded
// everything buffered. Records must survive and be written by a later flush.
func TestFlushFailureRetriesInsteadOfDropping(t *testing.T) {
	dir := t.TempDir()
	unblock := blockedDir(t, dir, "traces")

	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{testSpan(), testSpan(), testSpan()})

	if err := w.Flush(); err == nil {
		t.Fatal("expected the first flush to fail")
	}

	unblock()
	if err := w.Flush(); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if got := spanCount(t, dir); got != 3 {
		t.Errorf("after a failed flush and a retry, %d spans stored, want 3", got)
	}
}

// TestFlushIsAtomic verifies writes land via rename, leaving no partial or
// temporary file behind. A truncated .parquet is silently reported as zero
// rows by DuckDB, so it would hide data loss rather than surface it.
func TestFlushIsAtomic(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{testSpan()})
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var files []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		files = append(files, filepath.Base(p))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	if len(files) != 1 {
		t.Errorf("expected exactly 1 committed file, got %d: %v", len(files), files)
	}
	for _, f := range files {
		if strings.HasPrefix(f, ".") {
			t.Errorf("temporary file left behind: %s", f)
		}
	}
}

// TestFlushErrorsAreReported verifies background flush failures reach OnError.
// Previously Add() and the ticker discarded Flush's error, so a writer that
// could not write anything looked healthy indefinitely.
func TestFlushErrorsAreReported(t *testing.T) {
	dir := t.TempDir()
	blockedDir(t, dir, "traces")

	w := writer.New(dir, 10*time.Millisecond, 1000)
	errs := make(chan error, 4)
	w.OnError(func(err error) { errs <- err })
	w.Add([]writer.TraceSpan{testSpan()})
	w.Start()
	defer w.Stop()

	select {
	case <-errs:
	case <-time.After(2 * time.Second):
		t.Error("no flush error was reported for a persistently failing writer")
	}
}

// TestStopReturnsFinalFlushError verifies a failed shutdown flush is not
// swallowed, since Stop is the last chance to notice lost telemetry.
func TestStopReturnsFinalFlushError(t *testing.T) {
	dir := t.TempDir()
	blockedDir(t, dir, "traces")

	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{testSpan()})
	w.Start()

	if err := w.Stop(); err == nil {
		t.Error("Stop() returned nil despite an unflushable buffer")
	}
}

// --- receiver hardening regression tests ---

// TestRequestBodySizeLimit verifies an oversized body is rejected by the size
// cap before being decoded. The receiver is unauthenticated, so without a cap
// one client could make the process allocate without limit.
//
// The status code alone cannot distinguish a capped server from an uncapped
// one: garbage bytes are invalid protobuf and yield 400 either way, but the
// uncapped server has already buffered the whole body by then. So this asserts
// on the cap's specific error message, and confirms a large-but-valid payload
// under the limit is still accepted.
func TestRequestBodySizeLimit(t *testing.T) {
	addr, _, cleanup := startTestServer(t)
	defer cleanup()

	// Just over the receiver's 32 MiB cap (maxBodyBytes in internal/receiver).
	oversized := bytes.Repeat([]byte("A"), 32<<20+1)
	resp, err := http.Post(addr+"/v1/traces", "application/x-protobuf", bytes.NewReader(oversized))
	if err != nil {
		t.Fatalf("posting oversized body: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized body returned %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if !strings.Contains(string(body), "exceeds") {
		t.Errorf("oversized body was not rejected by the size cap (body: %q) — "+
			"it was buffered and then failed protobuf decoding instead", strings.TrimSpace(string(body)))
	}
}

// TestHealthEndpointIsNotCatchAll verifies the health handler is bound to
// /health rather than GET /, which previously answered 200 for any unknown path
// and masked typos in OTLP endpoints.
func TestHealthEndpointIsNotCatchAll(t *testing.T) {
	addr, _, cleanup := startTestServer(t)
	defer cleanup()

	resp, err := http.Get(addr + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health returned %d, want 200", resp.StatusCode)
	}

	resp2, err := http.Get(addr + "/not-a-real-endpoint")
	if err != nil {
		t.Fatalf("GET /not-a-real-endpoint: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path returned %d, want 404", resp2.StatusCode)
	}
}

// --- view refresh regression tests ---

// seedTrace writes one span with the given service and flushes it.
func seedTrace(t *testing.T, dir, service string) {
	t.Helper()
	w := writer.New(dir, 1*time.Hour, 1000)
	now := time.Now()
	w.Add([]writer.TraceSpan{{
		TraceID: "t", SpanID: "s", ServiceName: service, SpanName: "op",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 1,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: "{}", Events: "[]", Links: "[]",
	}})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding %s: %v", service, err)
	}
}

// TestViewsRefreshAfterDataArrives covers a staleness bug: DuckDB resolves a
// read_parquet glob when the view is created. An engine opened before any data
// existed installed a placeholder `SELECT ... WHERE false` view and kept
// returning zero rows forever, even once Parquet files were on disk. Anything
// long-lived (a serve process, an agent's periodic check loop) silently saw
// nothing.
func TestViewsRefreshAfterDataArrives(t *testing.T) {
	dir := t.TempDir()

	// Open before any data exists, so the empty-placeholder path is taken.
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()

	before, _, err := engine.Query("SELECT count(*) AS c FROM traces")
	if err != nil {
		t.Fatalf("query before data: %v", err)
	}
	if before[0]["c"].(int64) != 0 {
		t.Fatalf("expected 0 rows before data, got %v", before[0]["c"])
	}

	// Data arrives while the engine is still open.
	seedTrace(t, dir, "late-svc")

	after, _, err := engine.Query("SELECT service_name FROM traces")
	if err != nil {
		t.Fatalf("query after data: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("same engine returned %d rows after data was written, want 1 — "+
			"views are stale", len(after))
	}

	// A later batch must be picked up too.
	seedTrace(t, dir, "later-svc")
	count, _, err := engine.Query("SELECT count(*) AS c FROM traces")
	if err != nil {
		t.Fatalf("query after second batch: %v", err)
	}
	if got := count[0]["c"].(int64); got != 2 {
		t.Errorf("after a second batch, count = %d, want 2", got)
	}
}

// TestDescribeRefreshesViews verifies schema introspection also observes data
// that arrived after the engine was opened.
func TestDescribeRefreshesViews(t *testing.T) {
	dir := t.TempDir()
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()

	seedTrace(t, dir, "svc")

	cols, err := engine.Describe("traces")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(cols) == 0 {
		t.Error("Describe returned no columns for a populated store — view is stale")
	}
}

// TestPascalCase covers the helper that replaced the deprecated strings.Title.
// Only the first rune of each hyphen-separated segment is upper-cased, and
// hyphens are dropped, so "product-service" yields an RPC-style identifier.
func TestPascalCase(t *testing.T) {
	cases := map[string]string{
		"product-service":      "ProductService",
		"api-gateway":          "ApiGateway",
		"notification-service": "NotificationService",
		"single":               "Single",
		"a-b-c":                "ABC",
		"":                     "",
		"-lead":                "Lead",
		"trail-":               "Trail",
		"double--dash":         "DoubleDash",
		"UPPER-case":           "UPPERCase",
		"örebro-service":       "ÖrebroService",
	}
	for in, want := range cases {
		if got := testharness.PascalCase(in); got != want {
			t.Errorf("PascalCase(%q) = %q, want %q", in, got, want)
		}
	}
}
