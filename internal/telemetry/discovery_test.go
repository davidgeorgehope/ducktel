package telemetry

import (
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
