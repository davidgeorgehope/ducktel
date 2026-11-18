package webapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// newSpanNameFilterMux seeds two spans with different names, so the REST
// filter can be checked against the span_name column rather than attributes.
func newSpanNameFilterMux(t *testing.T) *http.ServeMux {
	t.Helper()
	now := time.Now()
	span := func(traceID, name string) writer.TraceSpan {
		return writer.TraceSpan{
			TraceID: traceID, SpanID: "span-" + traceID, ParentSpanID: "",
			ServiceName: "api", SpanName: name, SpanKind: "SPAN_KIND_SERVER",
			StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 100,
			StatusCode: "STATUS_CODE_OK", Attributes: "{}",
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme"}`,
			Events:             "[]", Links: "[]",
		}
	}

	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		span("trace-mcp", "POST /mcp"),
		span("trace-orders", "GET /orders"),
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	mux := http.NewServeMux()
	NewHandlers(telemetry.NewCore(engine), telemetry.NewFlusher("", ""), "").Register(mux)
	return mux
}

func TestAPISpanNameFilter(t *testing.T) {
	mux := newSpanNameFilterMux(t)

	for _, tc := range []struct {
		name   string
		filter string
		want   []string
	}{
		{"equal", "filter=" + url.QueryEscape("span_name:POST /mcp"), []string{"POST /mcp"}},
		{"not equal", "filter_not=" + url.QueryEscape("span_name:POST /mcp"), []string{"GET /orders"}},
	} {
		rec := doReq(t, mux, "GET", "/api/traces?tenant=acme&"+tc.filter, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", tc.name, rec.Code, rec.Body.String())
		}
		var resp rowsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: bad JSON: %v", tc.name, err)
		}
		if len(resp.Rows) != len(tc.want) {
			t.Fatalf("%s returned %d rows, want %d: %v", tc.name, len(resp.Rows), len(tc.want), resp.Rows)
		}
		for i, row := range resp.Rows {
			if got := row["span_name"]; got != tc.want[i] {
				t.Errorf("%s row %d span_name = %v, want %s", tc.name, i, got, tc.want[i])
			}
		}
	}
}
