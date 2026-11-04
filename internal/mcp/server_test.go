package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// This file tests the MCP adapter plumbing only — argument unmarshaling,
// error propagation into CallToolResult, tool registration. The underlying
// query logic (tenant isolation, SQL construction, injection resistance) is
// tested once, directly, in internal/telemetry — see core_test.go there.
// Re-testing those business rules here through the MCP protocol would just be
// slower coverage of the same code path.

// seedSpan mirrors what the receiver actually stores: service.name and
// tenant.id arrive as RESOURCE attributes.
func seedSpan(svc, tenant, traceID string) writer.TraceSpan {
	now := time.Now()
	return writer.TraceSpan{
		TraceID: traceID, SpanID: "s-" + svc, ParentSpanID: "",
		ServiceName: svc, SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 5,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: `{"service.name":"` + svc + `","tenant.id":"` + tenant + `"}`,
		Events:             "[]", Links: "[]",
	}
}

// newTestServer seeds a store and returns a connected MCP client session, so
// tests exercise the real protocol rather than calling methods directly.
func newTestServer(t *testing.T) *sdkmcp.ClientSession {
	t.Helper()
	dir := t.TempDir()

	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		seedSpan("api", "acme", "trace-acme-1"),
		seedSpan("worker", "acme", "trace-acme-2"),
	})
	w.AddLogs([]writer.LogRecord{
		{Timestamp: time.Now().UnixMicro(), ServiceName: "api", SeverityText: "ERROR",
			Body: "connection refused", Attributes: "{}",
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`},
		{Timestamp: time.Now().UnixMicro(), ServiceName: "api", SeverityText: "INFO",
			Body: "handled request", Attributes: "{}",
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`},
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

// call invokes a tool and returns its text content plus whether it errored.
func call(t *testing.T, s *sdkmcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

// countRows counts objects in a JSON array response.
func countRows(t *testing.T, body string) int {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("response is not a JSON array: %v\n%s", err, body)
	}
	return len(rows)
}

// --- tool registration ---

func TestToolsAreRegistered(t *testing.T) {
	s := newTestServer(t)

	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has no description — models need it to discriminate", tool.Name)
		}
	}
	for _, want := range []string{"trace_lookup", "span_search", "log_search", "metric_query"} {
		if !got[want] {
			t.Errorf("tool %q not registered", want)
		}
	}
	// Deliberately no domain-specific tool names.
	for _, forbidden := range []string{"escalation_calibration", "trajectory_trace", "tool_call_stats"} {
		if got[forbidden] {
			t.Errorf("domain-specific tool %q should not exist in a generic backend", forbidden)
		}
	}
}

func TestFlushToolNotRegisteredWithoutFlushURL(t *testing.T) {
	s := newTestServer(t) // constructed with flushURL == ""

	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "flush_buffer" {
			t.Error("flush_buffer should not be advertised without a flush URL")
		}
	}
}

// --- adapter smoke tests: args in, Core error/result out via CallToolResult ---

func TestTraceLookupAdapterHappyPath(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "trace_lookup", map[string]any{
		"trace_id": "trace-acme-1", "tenant": "acme",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	if n := countRows(t, body); n != 1 {
		t.Errorf("got %d spans, want 1 for trace-acme-1\n%s", n, body)
	}
}

func TestTraceLookupAdapterPropagatesCoreValidationError(t *testing.T) {
	s := newTestServer(t)

	// Core.LookupTrace rejects a missing tenant; the adapter must surface that
	// as CallToolResult.IsError rather than swallowing or panicking.
	body, isErr := call(t, s, "trace_lookup", map[string]any{"trace_id": "trace-acme-1"})
	if !isErr {
		t.Errorf("trace_lookup without tenant should error, got: %s", body)
	}
}

func TestSpanSearchAdapterHappyPath(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "span_search", map[string]any{"tenant": "acme", "service_name": "worker"})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	if n := countRows(t, body); n != 1 {
		t.Errorf("got %d rows, want 1\n%s", n, body)
	}
}

func TestMetricQueryAdapterPropagatesCoreValidationError(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "metric_query", map[string]any{"tenant": "acme", "aggregation": "avg"})
	if !isErr {
		t.Errorf("metric_query without metric_name should error, got: %s", body)
	}
}

func TestLogSearchAdapterHappyPath(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "log_search", map[string]any{"tenant": "acme", "severity": "error"})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	if n := countRows(t, body); n != 1 {
		t.Errorf("got %d rows, want the single ERROR record\n%s", n, body)
	}
}

func TestLogSearchAdapterPropagatesCoreValidationError(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "log_search", map[string]any{})
	if !isErr {
		t.Errorf("log_search without tenant should error, got: %s", body)
	}
}
