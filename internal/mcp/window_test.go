package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func windowSpan(traceID string, ts time.Time, durationMs float64) writer.TraceSpan {
	return writer.TraceSpan{
		TraceID: traceID, SpanID: "s-" + traceID, ParentSpanID: "",
		ServiceName: "api", SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
		StartTime: ts.UnixMicro(), EndTime: ts.UnixMicro(), DurationMs: durationMs,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
		Events:             "[]", Links: "[]",
	}
}

// newWindowServer seeds the oldest and newest of three 10-minute slices and
// returns a connected MCP client session.
func newWindowServer(t *testing.T) *sdkmcp.ClientSession {
	t.Helper()
	now := time.Now()
	oldest, newest := now.Add(-25*time.Minute), now.Add(-5*time.Minute)

	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		windowSpan("trace-oldest", oldest, 50),
		windowSpan("trace-newest", newest, 5),
	})
	w.AddLogs([]writer.LogRecord{
		{Timestamp: oldest.UnixMicro(), ServiceName: "api", SeverityText: "INFO",
			Body: "oldest", Attributes: "{}",
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`},
		{Timestamp: newest.UnixMicro(), ServiceName: "api", SeverityText: "INFO",
			Body: "newest", Attributes: "{}",
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`},
	})
	w.AddMetrics([]writer.MetricPoint{
		{MetricName: "window.gauge", MetricType: "gauge", Timestamp: oldest.UnixMicro(),
			ValueDouble: 1, ResourceAttributes: `{"tenant.id":"acme"}`},
		{MetricName: "window.gauge", MetricType: "gauge", Timestamp: newest.UnixMicro(),
			ValueDouble: 4, ResourceAttributes: `{"tenant.id":"acme"}`},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "ducktel-test", Version: "test"}, nil)
	NewServer(engine, "", "").Register(srv)
	clientT, serverT := sdkmcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestMCPUntilWindowOnEveryTool(t *testing.T) {
	s := newWindowServer(t)

	// span_search: only the oldest slice.
	body, isErr := call(t, s, "span_search", map[string]any{
		"tenant": "acme", "since_minutes": 30, "until_minutes": 20,
	})
	if isErr {
		t.Fatalf("span_search: %s", body)
	}
	var spans []map[string]any
	if err := json.Unmarshal([]byte(body), &spans); err != nil {
		t.Fatalf("span_search is not JSON: %v\n%s", err, body)
	}
	if len(spans) != 1 || spans[0]["trace_id"] != "trace-oldest" {
		t.Errorf("span_search = %v, want only trace-oldest", spans)
	}

	// log_search: only the oldest slice.
	body, isErr = call(t, s, "log_search", map[string]any{
		"tenant": "acme", "since_minutes": 30, "until_minutes": 20,
	})
	if isErr {
		t.Fatalf("log_search: %s", body)
	}
	var logs []map[string]any
	if err := json.Unmarshal([]byte(body), &logs); err != nil {
		t.Fatalf("log_search is not JSON: %v\n%s", err, body)
	}
	if len(logs) != 1 || logs[0]["body"] != "oldest" {
		t.Errorf("log_search = %v, want only the oldest record", logs)
	}

	// metric_query: the oldest slice's value only.
	body, isErr = call(t, s, "metric_query", map[string]any{
		"tenant": "acme", "metric_name": "window.gauge", "aggregation": "sum",
		"since_minutes": 30, "until_minutes": 20,
	})
	if isErr {
		t.Fatalf("metric_query: %s", body)
	}
	var metrics []map[string]any
	if err := json.Unmarshal([]byte(body), &metrics); err != nil {
		t.Fatalf("metric_query is not JSON: %v\n%s", err, body)
	}
	if len(metrics) != 1 || metrics[0]["value"].(float64) != 1 {
		t.Errorf("metric_query = %v, want one row with value 1", metrics)
	}

	// slow_requests: only the oldest span.
	body, isErr = call(t, s, "slow_requests", map[string]any{
		"tenant": "acme", "since_minutes": 30, "until_minutes": 20,
	})
	if isErr {
		t.Fatalf("slow_requests: %s", body)
	}
	var slow []map[string]any
	if err := json.Unmarshal([]byte(body), &slow); err != nil {
		t.Fatalf("slow_requests is not JSON: %v\n%s", err, body)
	}
	if len(slow) != 1 || slow[0]["trace_id"] != "trace-oldest" {
		t.Errorf("slow_requests = %v, want only trace-oldest", slow)
	}
}

func TestMCPUntilValidationIsToolError(t *testing.T) {
	s := newWindowServer(t)

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"span_search", map[string]any{"tenant": "acme", "since_minutes": 10, "until_minutes": 10}},
		{"log_search", map[string]any{"tenant": "acme", "since_minutes": 10, "until_minutes": 10}},
		{"metric_query", map[string]any{"tenant": "acme", "metric_name": "window.gauge", "aggregation": "count", "since_minutes": 10, "until_minutes": 10}},
		{"slow_requests", map[string]any{"tenant": "acme", "since_minutes": 10, "until_minutes": 10}},
		{"span_search", map[string]any{"tenant": "acme", "since_minutes": 30, "until_minutes": -1}},
	}
	for _, tc := range cases {
		body, isErr := call(t, s, tc.tool, tc.args)
		if !isErr {
			t.Errorf("%s with %v should be a tool error, got: %s", tc.tool, tc.args, body)
		}
	}
}
