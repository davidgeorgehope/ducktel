package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// callTexts returns each text content block separately. call concatenates
// them, which would hide a hint that must stay out of the JSON block.
func callTexts(t *testing.T, s *sdkmcp.ClientSession, name string, args map[string]any) ([]string, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	var texts []string
	for _, c := range res.Content {
		tc, ok := c.(*sdkmcp.TextContent)
		if !ok {
			t.Fatalf("content %T, want text", c)
		}
		texts = append(texts, tc.Text)
	}
	if len(texts) == 0 {
		t.Fatal("tool returned no text content")
	}
	return texts, res.IsError
}

func laterText(texts []string) string {
	return strings.Join(texts[1:], "\n")
}

func TestSpanSearchStatusErrorStaysJSON(t *testing.T) {
	errSpan := seedSpan("api", "acme", "trace-err")
	errSpan.StatusCode = "STATUS_CODE_ERROR"
	s := newTestServerWithSpans(t, nil, []writer.TraceSpan{errSpan})

	texts, isErr := callTexts(t, s, "span_search", map[string]any{
		"tenant":  "acme",
		"filters": []map[string]any{{"key": "status_code", "value": "error"}},
	})
	if isErr {
		t.Fatalf("span_search: %v", texts)
	}
	if len(texts) != 1 {
		t.Fatalf("content blocks = %d, want only the JSON: %q", len(texts), texts)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(texts[0]), &rows); err != nil {
		t.Fatalf("body is not a JSON array: %v\n%s", err, texts[0])
	}
	if len(rows) != 1 || rows[0]["trace_id"] != "trace-err" {
		t.Fatalf("rows = %v, want the error span", rows)
	}
}

func TestSpanSearchEmptyTellsHowToRecover(t *testing.T) {
	s := newTestServer(t)

	texts, isErr := callTexts(t, s, "span_search", map[string]any{"tenant": "nobody"})
	if isErr {
		t.Fatalf("empty span_search is an error: %v", texts)
	}
	if texts[0] != "[]" {
		t.Fatalf("first content = %q, want []", texts[0])
	}
	hint := laterText(texts)
	for _, want := range []string{"tenant.id", "60 minutes", "buffer"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q:\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "flush_buffer") {
		t.Errorf("hint names flush_buffer without a flush URL:\n%s", hint)
	}

	flushed := newTestServerWith(t, func(ds *Server) {
		ds.flusher = telemetry.NewFlusher("http://127.0.0.1:9/flush", "")
	})
	texts, isErr = callTexts(t, flushed, "span_search", map[string]any{"tenant": "nobody"})
	if isErr {
		t.Fatalf("empty span_search with flush URL: %v", texts)
	}
	if texts[0] != "[]" {
		t.Fatalf("first content = %q, want []", texts[0])
	}
	if !strings.Contains(laterText(texts), "flush_buffer") {
		t.Fatalf("hint with a flush URL:\n%s", laterText(texts))
	}
}

func TestSpanSearchExplicitWindowOmitsDefault(t *testing.T) {
	s := newTestServer(t)

	texts, isErr := callTexts(t, s, "span_search", map[string]any{
		"tenant": "nobody", "since_minutes": 120,
	})
	if isErr || texts[0] != "[]" {
		t.Fatalf("isErr=%v texts=%q", isErr, texts)
	}
	hint := laterText(texts)
	if strings.Contains(hint, "default window") || strings.Contains(hint, "60") {
		t.Fatalf("explicit window says the default was used:\n%s", hint)
	}
	if !strings.Contains(hint, "tenant.id") || !strings.Contains(hint, "buffer") {
		t.Fatalf("hint = %s", hint)
	}
}

func TestSpanSearchUnrecognizedStatusSuggestsStored(t *testing.T) {
	s := newTestServer(t)

	texts, isErr := callTexts(t, s, "span_search", map[string]any{
		"tenant":  "nobody",
		"filters": []map[string]any{{"key": "status_code", "value": "eror"}},
	})
	if isErr || texts[0] != "[]" {
		t.Fatalf("isErr=%v texts=%q", isErr, texts)
	}
	if !strings.Contains(laterText(texts), "did you mean STATUS_CODE_ERROR?") {
		t.Fatalf("hint = %s", laterText(texts))
	}

	texts, isErr = callTexts(t, s, "span_search", map[string]any{
		"tenant":  "nobody",
		"filters": []map[string]any{{"key": "status_code", "value": "error"}},
	})
	if isErr || texts[0] != "[]" {
		t.Fatalf("recognized spelling: isErr=%v texts=%q", isErr, texts)
	}
	hint := laterText(texts)
	if strings.Contains(hint, "did you mean") || strings.Contains(hint, "STATUS_CODE_ERROR") {
		t.Fatalf("recognized spelling was blamed for the empty result:\n%s", hint)
	}
}

func TestLogSearchTraceIDDoesNotClaimDefaultWindow(t *testing.T) {
	s := newTestServer(t)

	texts, isErr := callTexts(t, s, "log_search", map[string]any{
		"tenant": "nobody", "trace_id": "trace-missing",
	})
	if isErr || texts[0] != "[]" {
		t.Fatalf("isErr=%v texts=%q", isErr, texts)
	}
	if strings.Contains(laterText(texts), "default window") {
		t.Fatalf("trace_id lookup claimed the default window:\n%s", laterText(texts))
	}
}

func TestMetricQueryEmptyHintsOnlyWhenThereAreNoRows(t *testing.T) {
	s := newTestServer(t)

	texts, isErr := callTexts(t, s, "metric_query", map[string]any{
		"tenant": "nobody", "metric_name": "missing.metric", "aggregation": "count",
	})
	if isErr {
		t.Fatalf("count: %v", texts)
	}
	if len(texts) != 1 {
		t.Fatalf("count of no points should stay one JSON block, got %q", texts)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(texts[0]), &rows); err != nil {
		t.Fatalf("count body: %v\n%s", err, texts[0])
	}
	if len(rows) != 1 {
		t.Fatalf("count rows = %d, want 1: %s", len(rows), texts[0])
	}

	texts, isErr = callTexts(t, s, "metric_query", map[string]any{
		"tenant": "nobody", "metric_name": "missing.metric", "aggregation": "count",
		"group_by": "service_name",
	})
	if isErr || texts[0] != "[]" {
		t.Fatalf("grouped empty: isErr=%v texts=%q", isErr, texts)
	}
	if !strings.Contains(laterText(texts), "tenant.id") {
		t.Fatalf("grouped empty hint = %s", laterText(texts))
	}
}

func TestTraceLookupEmptySummaryKeepsJSON(t *testing.T) {
	s := newTestServer(t)

	texts, isErr := callTexts(t, s, "trace_lookup", map[string]any{
		"tenant": "acme", "trace_id": "missing",
	})
	if isErr {
		t.Fatalf("trace_lookup: %v", texts)
	}
	var summary telemetry.TraceSummary
	if err := json.Unmarshal([]byte(texts[0]), &summary); err != nil {
		t.Fatalf("first block is not the summary JSON: %v\n%s", err, texts[0])
	}
	if summary.SpanCount != 0 {
		t.Fatalf("span_count = %d, want 0", summary.SpanCount)
	}
	if strings.Contains(texts[0], "buffer") {
		t.Fatalf("hint was folded into the JSON: %s", texts[0])
	}
	hint := laterText(texts)
	if !strings.Contains(hint, "tenant.id") || strings.Contains(hint, "default window") {
		t.Fatalf("hint = %s", hint)
	}

	texts, isErr = callTexts(t, s, "trace_lookup", map[string]any{
		"tenant": "acme", "trace_id": "missing", "detail": "spans",
	})
	if isErr || texts[0] != "[]" {
		t.Fatalf("detail=spans: isErr=%v texts=%q", isErr, texts)
	}
	if !strings.Contains(laterText(texts), "tenant.id") {
		t.Fatalf("spans hint = %s", laterText(texts))
	}
}

func TestCompareAndSlowEmptyHint(t *testing.T) {
	s := newTestServer(t)

	texts, isErr := callTexts(t, s, "compare_spans", map[string]any{
		"tenant": "nobody", "span_name": "op",
	})
	if isErr {
		t.Fatalf("compare_spans: %v", texts)
	}
	var cmp telemetry.SpanComparison
	if err := json.Unmarshal([]byte(texts[0]), &cmp); err != nil {
		t.Fatalf("first block is not the comparison JSON: %v\n%s", err, texts[0])
	}
	if cmp.SlowN != 0 || cmp.RestN != 0 {
		t.Fatalf("slow_n/rest_n = %d/%d, want 0/0", cmp.SlowN, cmp.RestN)
	}
	if !strings.Contains(laterText(texts), "tenant.id") {
		t.Fatalf("compare hint = %s", laterText(texts))
	}

	texts, isErr = callTexts(t, s, "slow_requests", map[string]any{"tenant": "nobody"})
	if isErr || texts[0] != "[]" {
		t.Fatalf("slow_requests: isErr=%v texts=%q", isErr, texts)
	}
	if !strings.Contains(laterText(texts), "buffer") {
		t.Fatalf("slow hint = %s", laterText(texts))
	}
}

func TestMetricQueryNullAggregateHintsMetricNames(t *testing.T) {
	s := newTestServer(t)

	texts, isErr := callTexts(t, s, "metric_query", map[string]any{
		"tenant": "nobody", "metric_name": "missing.metric", "aggregation": "avg",
	})
	if isErr {
		t.Fatalf("avg: %v", texts)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(texts[0]), &rows); err != nil {
		t.Fatalf("first block is not the JSON rows: %v\n%s", err, texts[0])
	}
	if len(rows) != 1 || rows[0]["value"] != nil {
		t.Fatalf("rows = %v, want one null value", rows)
	}
	hint := laterText(texts)
	for _, want := range []string{"tenant.id", "list_metric_names", "list_tenants"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q:\n%s", want, hint)
		}
	}
}

func TestEmptyHintNamesDiscoveryTools(t *testing.T) {
	s := newTestServer(t)

	texts, isErr := callTexts(t, s, "span_search", map[string]any{"tenant": "nobody"})
	if isErr || texts[0] != "[]" {
		t.Fatalf("isErr=%v texts=%q", isErr, texts)
	}
	hint := laterText(texts)
	if !strings.Contains(hint, "list_tenants") || !strings.Contains(hint, "list_services") {
		t.Fatalf("hint does not name the discovery tools:\n%s", hint)
	}
	if strings.Contains(hint, "list_metric_names") {
		t.Fatalf("span_search hint names list_metric_names:\n%s", hint)
	}

	texts, isErr = callTexts(t, s, "slow_requests", map[string]any{
		"tenant": "nobody", "metric_name": "missing.metric",
	})
	if isErr || texts[0] != "[]" {
		t.Fatalf("slow_requests: isErr=%v texts=%q", isErr, texts)
	}
	if !strings.Contains(laterText(texts), "list_metric_names") {
		t.Fatalf("slow_requests metric hint:\n%s", laterText(texts))
	}
}
