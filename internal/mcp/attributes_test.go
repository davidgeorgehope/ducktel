package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func paySpan() writer.TraceSpan {
	now := time.Now()
	return writer.TraceSpan{
		TraceID: "trace-pay", SpanID: "span-pay",
		ServiceName: "pay", SpanName: "charge", SpanKind: "SPAN_KIND_SERVER",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 3,
		StatusCode:         "STATUS_CODE_OK",
		Attributes:         `{"http.route":"route-pay-secret"}`,
		ResourceAttributes: `{"service.name":"pay","tenant.id":"acme"}`,
		Events:             "[]", Links: "[]",
	}
}

func TestSpanSearchOmitsAttributesUnlessAsked(t *testing.T) {
	s := newTestServerWithSpans(t, nil, []writer.TraceSpan{paySpan()})

	body, isErr := call(t, s, "span_search", map[string]any{
		"tenant": "acme", "service_name": "pay",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	if strings.Contains(body, "\n") || strings.Contains(body, ": ") {
		t.Errorf("span_search JSON is not compact:\n%s", body)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, body)
	}
	if len(rows) != 1 || rows[0]["span_id"] != "span-pay" {
		t.Fatalf("rows = %v, want the pay span", rows)
	}
	if _, ok := rows[0]["attributes"]; ok || strings.Contains(body, "route-pay-secret") {
		t.Errorf("default span_search included attributes:\n%s", body)
	}

	body, isErr = call(t, s, "span_search", map[string]any{
		"tenant": "acme", "service_name": "pay", "include_attributes": true,
	})
	if isErr {
		t.Fatalf("include_attributes: %s", body)
	}
	rows = nil
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, body)
	}
	attr, _ := rows[0]["attributes"].(string)
	if !strings.Contains(attr, "route-pay-secret") {
		t.Errorf("include_attributes did not return the attribute, got %#v\n%s", rows[0]["attributes"], body)
	}
}

func TestTraceLookupSpansKeepAttributesUnlessDeclined(t *testing.T) {
	s := newTestServerWithSpans(t, nil, []writer.TraceSpan{paySpan()})

	body, isErr := call(t, s, "trace_lookup", map[string]any{
		"tenant": "acme", "trace_id": "trace-pay", "detail": "spans",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, body)
	}
	attr, _ := rows[0]["attributes"].(string)
	if !strings.Contains(attr, "route-pay-secret") {
		t.Errorf("detail=spans dropped attributes:\n%s", body)
	}
	if strings.Contains(body, "\n") {
		t.Errorf("detail=spans JSON is not compact:\n%s", body)
	}

	body, isErr = call(t, s, "trace_lookup", map[string]any{
		"tenant": "acme", "trace_id": "trace-pay", "detail": "spans",
		"include_attributes": false,
	})
	if isErr {
		t.Fatalf("include_attributes false: %s", body)
	}
	rows = nil
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, body)
	}
	if len(rows) != 1 || rows[0]["span_id"] != "span-pay" {
		t.Fatalf("rows = %v, want the pay span", rows)
	}
	if _, ok := rows[0]["attributes"]; ok || strings.Contains(body, "route-pay-secret") {
		t.Errorf("include_attributes false still returned attributes:\n%s", body)
	}
}

func TestTraceLookupSummaryIgnoresIncludeAttributes(t *testing.T) {
	s := newTestServerWithSpans(t, nil, []writer.TraceSpan{paySpan()})

	body, isErr := call(t, s, "trace_lookup", map[string]any{
		"tenant": "acme", "trace_id": "trace-pay", "include_attributes": false,
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(body), &summary); err != nil {
		t.Fatalf("summary is not an object: %v\n%s", err, body)
	}
	if summary["trace_id"] != "trace-pay" {
		t.Errorf("summary = %v", summary)
	}
	if _, ok := summary["span_count"]; !ok {
		t.Errorf("summary lost span_count: %s", body)
	}
}
