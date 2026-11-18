package webapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// newCompareMux seeds ten slow (cache=miss) and ten fast (cache=hit) spans and
// returns the mux plus the Core behind it, so the endpoint's body can be
// compared with the core result for the same options.
func newCompareMux(t *testing.T) (*http.ServeMux, *telemetry.Core) {
	t.Helper()
	now := time.Now()
	var spans []writer.TraceSpan
	for i := 0; i < 20; i++ {
		dur, cache := 10.0, "hit"
		if i < 10 {
			dur, cache = 100, "miss"
		}
		spans = append(spans, writer.TraceSpan{
			TraceID: "trace-" + strconv.Itoa(i), SpanID: "span-" + strconv.Itoa(i), ParentSpanID: "",
			ServiceName: "api", SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
			StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: dur,
			StatusCode: "STATUS_CODE_OK",
			Attributes: `{"cache":"` + cache + `","request.id":"req-` + strconv.Itoa(i) + `"}`,
			ResourceAttributes: `{"service.name":"api","tenant.id":"acme","k8s.pod.name":"pod-` +
				strconv.Itoa(i%2) + `"}`,
			Events: "[]", Links: "[]",
		})
	}

	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add(spans)
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	core := telemetry.NewCore(engine)

	mux := http.NewServeMux()
	NewHandlers(core, telemetry.NewFlusher("", ""), "").Register(mux)
	return mux, core
}

func TestAPICompareMatchesCore(t *testing.T) {
	mux, core := newCompareMux(t)

	rec := doReq(t, mux, "GET",
		"/api/compare?tenant=acme&service_name=api&span_name=op&since_minutes=60&min_ms=50&max_ms=200&limit=15", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got telemetry.SpanComparison
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("compare JSON: %v\n%s", err, rec.Body.String())
	}

	minMs, maxMs := 50.0, 200.0
	want, err := core.CompareSpans("acme", telemetry.CompareOpts{
		ServiceName: "api",
		SpanName:    "op",
		Since:       60,
		Slow:        telemetry.Range{Min: &minMs, Max: &maxMs},
		Limit:       15,
	})
	if err != nil {
		t.Fatalf("core CompareSpans: %v", err)
	}
	if !reflect.DeepEqual(got, *want) {
		t.Errorf("API result\ngot:  %+v\nwant: %+v", got, *want)
	}

	if got.SlowN != 10 || got.RestN != 10 {
		t.Errorf("slow_n/rest_n = %d/%d, want 10/10", got.SlowN, got.RestN)
	}
	if len(got.Rows) == 0 || got.Rows[0].Key != "cache" || got.Rows[0].Value != "miss" {
		t.Fatalf("first row = %+v, want cache=miss", got.Rows)
	}
}

func TestAPICompareDefaultThreshold(t *testing.T) {
	mux, _ := newCompareMux(t)

	rec := doReq(t, mux, "GET", "/api/compare?tenant=acme&service_name=api&since_minutes=60", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got telemetry.SpanComparison
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("compare JSON: %v", err)
	}
	if got.ThresholdMs == nil {
		t.Fatal("expected a derived threshold_ms")
	}
	if got.SlowN == 0 || got.RestN == 0 {
		t.Errorf("default comparison counts = %d/%d, want both groups non-empty", got.SlowN, got.RestN)
	}
}

func TestAPICompareInvalidInputIs400(t *testing.T) {
	mux, _ := newCompareMux(t)

	targets := []string{
		"/api/compare?tenant=acme&min_ms=200&max_ms=50",
		"/api/compare?tenant=acme&since_minutes=10&until_minutes=10",
		"/api/compare?tenant=acme&since_minutes=30&until_minutes=-1",
		"/api/compare?service_name=api",
		"/api/compare?tenant=acme&filter=no-colon",
	}
	for _, target := range targets {
		rec := doReq(t, mux, "GET", target, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", target, rec.Code, rec.Body.String())
		}
	}
}
