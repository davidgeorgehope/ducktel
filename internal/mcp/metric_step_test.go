package mcp

import (
	"encoding/json"
	"testing"
)

// TestMetricQueryStepAndResourceGroup exercises the MCP adapter wiring for the
// step parameter and resource.<key> grouping. The seeded histogram points are
// one per resource service, so grouping by resource.service.name is visible in
// the result columns.
func TestMetricQueryStepAndResourceGroup(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "metric_query", map[string]any{
		"tenant": "acme", "metric_name": "http.server.duration", "aggregation": "count",
		"group_by": "resource.service.name", "step": "1m",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("response is not a JSON array: %v\n%s", err, body)
	}
	if len(rows) == 0 {
		t.Fatalf("no rows: %s", body)
	}
	for _, r := range rows {
		if _, ok := r["time"]; !ok {
			t.Errorf("row has no time column: %v", r)
		}
		if _, ok := r["resource.service.name"]; !ok {
			t.Errorf("row has no resource.service.name column: %v", r)
		}
	}
}

func TestMetricQueryRejectsBadStepThroughMCP(t *testing.T) {
	s := newTestServer(t)

	body, isErr := call(t, s, "metric_query", map[string]any{
		"tenant": "acme", "metric_name": "http.server.duration", "aggregation": "count",
		"step": "nope",
	})
	if !isErr {
		t.Errorf("bad step should error, got: %s", body)
	}
}
