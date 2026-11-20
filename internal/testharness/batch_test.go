package testharness

import (
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// Locked with the payment-service error_rate scenario below. Seed 1 and
// n=8 produce a payment error span and a WARN log.
const (
	lockedSeed int64 = 1
	lockedN          = 8
)

func TestEmitBatchDeterministic(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.Endpoint = ""
	a := emitBatch(t, cfg, lockedSeed, lockedN, now)
	b := emitBatch(t, cfg, lockedSeed, lockedN, now)
	if !reflect.DeepEqual(batchView(a), batchView(b)) {
		t.Fatalf("two batches from seed %d n %d differ\n%#v\n%#v", lockedSeed, lockedN, batchView(a), batchView(b))
	}
}

func TestEmitBatchPaymentWarn(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.Endpoint = ""
	cfg.ErrorRate = 0
	cfg.Scenarios = []Scenario{{
		Name:          "payment-service-error_rate",
		TargetService: "payment-service",
		Type:          "error_rate",
		ErrorRate:     1,
	}}
	batch := emitBatch(t, cfg, lockedSeed, lockedN, now)

	warns := 0
	payErr := 0
	for _, l := range batch.Logs {
		if l.Severity == "WARN" && l.SeverityNum == 13 {
			warns++
		}
	}
	for _, s := range batch.Spans {
		if s.Service == "payment-service" && s.Status == 2 {
			payErr++
		}
	}
	if warns == 0 || payErr == 0 {
		t.Fatalf("seed %d n %d: WARN logs %d, payment error spans %d", lockedSeed, lockedN, warns, payErr)
	}
}

func TestEmitBatchMetricNames(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.Endpoint = ""
	batch := emitBatch(t, cfg, lockedSeed, lockedN, now)
	got := map[string]bool{}
	for _, m := range batch.Metrics {
		got[m.Name] = true
	}
	want := MetricNames()
	if len(got) != len(want) {
		t.Fatalf("metric names %v, want %v", got, want)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("batch missing metric %s", name)
		}
	}
}

func TestEmitBatchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cfg := DefaultConfig()
	cfg.Endpoint = srv.URL
	cfg.ErrorRate = 0
	h := New(cfg)
	h.SetRand(rand.New(rand.NewSource(lockedSeed)))
	_, err := h.EmitBatch(1, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err == nil {
		t.Fatal("EmitBatch swallowed a non-2xx response")
	}
}

func emitBatch(t *testing.T, cfg Config, seed int64, n int, now time.Time) Batch {
	t.Helper()
	h := New(cfg)
	h.SetRand(rand.New(rand.NewSource(seed)))
	batch, err := h.EmitBatch(n, now)
	if err != nil {
		t.Fatalf("EmitBatch: %v", err)
	}
	return batch
}

type spanView struct {
	ID, TraceID, Name string
	Duration          int64
}

type logView struct {
	Body, Severity string
	SeverityNum    int
}

type batchSnapshot struct {
	Spans   []spanView
	Logs    []logView
	Metrics []string
}

func batchView(b Batch) batchSnapshot {
	out := batchSnapshot{}
	for _, s := range b.Spans {
		out.Spans = append(out.Spans, spanView{
			ID: s.SpanID, TraceID: s.TraceID, Name: s.Name,
			Duration: s.EndNano - s.StartNano,
		})
	}
	for _, l := range b.Logs {
		out.Logs = append(out.Logs, logView{Body: l.Body, Severity: l.Severity, SeverityNum: l.SeverityNum})
	}
	for _, m := range b.Metrics {
		out.Metrics = append(out.Metrics, m.Name)
	}
	return out
}
