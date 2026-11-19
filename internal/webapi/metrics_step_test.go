package webapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// newMetricsStepMux seeds one gauge with two resource hosts over two minutes.
func newMetricsStepMux(t *testing.T) *http.ServeMux {
	t.Helper()
	dir := t.TempDir()
	base := time.Now().Add(-5 * time.Minute).Truncate(time.Minute)

	w := writer.New(dir, time.Hour, 1000)
	point := func(host string, ts time.Time, v float64) writer.MetricPoint {
		return writer.MetricPoint{
			MetricName: "cpu.host", MetricType: "gauge", Timestamp: ts.UnixMicro(),
			ValueDouble: v, ResourceAttributes: `{"host.name":"` + host + `","tenant.id":"acme"}`,
		}
	}
	w.AddMetrics([]writer.MetricPoint{
		point("host-a", base, 10),
		point("host-b", base, 30),
		point("host-a", base.Add(time.Minute), 20),
		point("host-b", base.Add(time.Minute), 40),
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

func TestMetricsEndpointStepAndResourceGroup(t *testing.T) {
	mux := newMetricsStepMux(t)

	target := "/api/metrics?tenant=acme&metric_name=cpu.host&aggregation=avg" +
		"&group_by=resource.host.name&step=1m&since_minutes=60"
	rec := doReq(t, mux, "GET", target, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp rowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(resp.Columns) != 3 || resp.Columns[0] != "time" || resp.Columns[1] != "resource.host.name" {
		t.Fatalf("columns = %v, want [time resource.host.name value]", resp.Columns)
	}
	if len(resp.Rows) != 4 {
		t.Fatalf("rows = %v, want 4 (2 minutes x 2 hosts)", resp.Rows)
	}
	for _, r := range resp.Rows {
		if _, ok := r["time"]; !ok {
			t.Errorf("row has no time column: %v", r)
		}
		if r["resource.host.name"] != "host-a" && r["resource.host.name"] != "host-b" {
			t.Errorf("row host = %v", r["resource.host.name"])
		}
	}
}

func TestMetricsEndpointRejectsBadStep(t *testing.T) {
	mux := newMetricsStepMux(t)

	for _, step := range []string{"nope", "1s"} {
		target := "/api/metrics?tenant=acme&metric_name=cpu.host&aggregation=avg&step=" + step
		rec := doReq(t, mux, "GET", target, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("step %q: status = %d, want 400: %s", step, rec.Code, rec.Body.String())
		}
	}
}

func TestMetricsParamsHelpMentionsStepAndResource(t *testing.T) {
	mux := newMetricsStepMux(t)

	rec := doReq(t, mux, "GET", "/api/metrics?tenant=acme", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	for _, want := range []string{"step", "resource.<key>"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("help does not mention %q: %s", want, rec.Body.String())
		}
	}
}
