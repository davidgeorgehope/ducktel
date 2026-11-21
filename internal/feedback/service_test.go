package feedback_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/feedback"
	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func validFields(id, tenant string) feedback.Fields {
	return feedback.Fields{
		ReportID: id, Tenant: tenant, ToolName: "log_search",
		Category: "bug", Severity: "high",
		Summary:     "rows hid the error",
		Details:     "the tool returned an empty list",
		ProposedFix: "include the error string ghp_FAKE_DO_NOT_SCRUB",
		ExampleCall: `log_search(tenant="acme")`,
	}
}

func memService(t *testing.T, write func(feedback.Report)) *feedback.Service {
	t.Helper()
	svc, err := feedback.New(feedback.Options{
		Write: func(ctx context.Context, r feedback.Report) error {
			if write != nil {
				write(r)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestRateLimitIsPerTenantAndSkipsInvalidAndRetries(t *testing.T) {
	var writes atomic.Int32
	svc := memService(t, func(feedback.Report) { writes.Add(1) })
	ctx := context.Background()

	_, err := svc.SubmitJSON(ctx, []byte(`{"tenant":"acme","category":"nope"}`))
	var fe *feedback.FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("invalid = %v", err)
	}

	for i := 0; i < 20; i++ {
		r, err := feedback.ParseFields(validFields(fmt.Sprintf("id-%d", i), "acme"))
		if err != nil {
			t.Fatal(err)
		}
		res, err := svc.Submit(ctx, r)
		if err != nil || !res.Stored {
			t.Fatalf("report %d: %v %+v", i, err, res)
		}
	}
	again, err := feedback.ParseFields(validFields("id-0", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx, again); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 20 {
		t.Fatalf("writes = %d, want 20", writes.Load())
	}

	blocked, err := feedback.ParseFields(validFields("id-21", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx, blocked); !errors.Is(err, feedback.ErrRateLimited) {
		t.Fatalf("21st = %v, want rate limit", err)
	}
	other, err := feedback.ParseFields(validFields("id-other", "globex"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx, other); err != nil {
		t.Fatalf("other tenant: %v", err)
	}
	if writes.Load() != 21 {
		t.Fatalf("writes = %d, want 21", writes.Load())
	}
}

func TestSameReportIDWritesOneLog(t *testing.T) {
	var writes atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	svc := memService(t, func(feedback.Report) {
		writes.Add(1)
		once.Do(func() { close(started) })
		<-release
	})
	r, err := feedback.ParseFields(validFields("same", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	errCh := make(chan error, 2)
	go func() {
		_, err := svc.Submit(ctx, r)
		errCh <- err
	}()
	<-started
	go func() {
		_, err := svc.Submit(ctx, r)
		errCh <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	if writes.Load() != 1 {
		t.Fatalf("writes = %d, want 1", writes.Load())
	}
}

func TestDirectWriteIsQueryableAndIsolated(t *testing.T) {
	dir := t.TempDir()
	old, err := feedback.ParseFields(validFields("old-1", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	rec := old.LogRecord(time.Now().Add(-10 * 24 * time.Hour))
	w := writer.New(dir, time.Hour, 1000)
	w.AddLogs([]writer.LogRecord{rec})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	svc, err := feedback.New(feedback.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := feedback.ParseFields(validFields("new-1", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Submit(context.Background(), fresh)
	if err != nil || !res.Stored || res.Sink != "off" || res.ReportID != "new-1" {
		t.Fatalf("submit = %+v %v", res, err)
	}
	if _, err := svc.Submit(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	rows, _, err := engine.Query(`SELECT service_name, scope_name, severity_text, severity_number,
		body, attributes, resource_attributes, trace_id, span_id, flags
		FROM logs WHERE service_name = ? ORDER BY timestamp`, feedback.ServiceName)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (retry wrote another log?)", len(rows))
	}
	got := rows[1]
	if got["service_name"] != feedback.ServiceName || got["scope_name"] != "ducktel.feedback" {
		t.Fatalf("service/scope = %v %v", got["service_name"], got["scope_name"])
	}
	if got["severity_text"] != "ERROR" || number(got["severity_number"]) != 17 {
		t.Fatalf("severity = %v %v, want ERROR 17", got["severity_text"], got["severity_number"])
	}
	if got["trace_id"] != "" || got["span_id"] != "" || number(got["flags"]) != 0 {
		t.Fatalf("trace/span/flags = %v %v %v", got["trace_id"], got["span_id"], got["flags"])
	}
	for _, text := range []string{fresh.Summary, fresh.Details, fresh.ProposedFix, fresh.ExampleCall} {
		if !strings.Contains(got["body"].(string), text) {
			t.Fatalf("body missing %q\n%s", text, got["body"])
		}
	}
	var attrs map[string]string
	if err := json.Unmarshal([]byte(got["attributes"].(string)), &attrs); err != nil {
		t.Fatal(err)
	}
	if attrs["report_id"] != "new-1" || attrs["tool_name"] != "log_search" ||
		attrs["category"] != "bug" || attrs["severity"] != "high" ||
		attrs["proposed_fix"] != fresh.ProposedFix {
		t.Fatalf("attributes = %v", attrs)
	}
	var resource map[string]string
	if err := json.Unmarshal([]byte(got["resource_attributes"].(string)), &resource); err != nil {
		t.Fatal(err)
	}
	if resource["service.name"] != feedback.ServiceName || resource["tenant.id"] != "acme" {
		t.Fatalf("resource = %v", resource)
	}

	core := telemetry.NewCore(engine)
	mine, _, err := core.SearchLogs("acme", "", feedback.ServiceName, "", "", nil, 60, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 {
		t.Fatalf("log_search acme = %d, want the fresh row only", len(mine))
	}
	other, _, err := core.SearchLogs("globex", "", "", "", "", nil, 0, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("log_search globex = %d, want 0", len(other))
	}
	week, _, err := core.ListFeedback("acme", time.Now().Add(-168*time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(week) != 1 || week[0]["report_id"] != "new-1" {
		t.Fatalf("week = %v", week)
	}
	all, _, err := core.ListFeedback("acme", time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all history = %d, want 2", len(all))
	}
	hidden, _, err := core.ListFeedback("globex", time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 0 {
		t.Fatalf("globex list = %v", hidden)
	}
	one, _, err := core.ListFeedback("acme", time.Time{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 {
		t.Fatalf("limit 1 = %d", len(one))
	}
}

func TestLockedDirectoryWritesNothing(t *testing.T) {
	dir := t.TempDir()
	release, err := writer.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	svc, err := feedback.New(feedback.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	r, err := feedback.ParseFields(validFields("locked-1", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Submit(context.Background(), r)
	if err == nil || !errors.Is(err, writer.ErrLocked) || !strings.Contains(err.Error(), "--feedback-url") {
		t.Fatalf("err = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "logs", "*", "*.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("wrote %v", matches)
	}
}

func TestRemotePostFlushesAndRetriesSinkWithoutASecondPost(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if r.Header.Get("Authorization") != "Bearer flush-tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		var got feedback.Report
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("body: %v", err)
		}
		if got.ProposedFix != validFields("", "").ProposedFix {
			t.Errorf("proposed_fix = %q", got.ProposedFix)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"report_id":"r1","status":"stored"}`))
	}))
	defer srv.Close()

	var flushes atomic.Int32
	var sinkCalls atomic.Int32
	svc, err := feedback.New(feedback.Options{
		Endpoint:   srv.URL + "?token=supersecret",
		Token:      "flush-tok",
		WebhookURL: "http://127.0.0.1:1/hook?token=supersecret",
		HTTPClient: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.String(), "/hook") {
				sinkCalls.Add(1)
				return nil, errors.New("dial failed")
			}
			return http.DefaultTransport.RoundTrip(r)
		})},
		AfterStore: func(context.Context) error {
			flushes.Add(1)
			return errors.New("flush rejected")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := feedback.ParseFields(validFields("r1", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Submit(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stored || res.Sink != "webhook" || res.FlushError == "" || res.SinkError == "" {
		t.Fatalf("result = %+v", res)
	}
	if strings.Contains(res.SinkError, "supersecret") || strings.Contains(res.FlushError, "supersecret") {
		t.Fatalf("error leaked the query secret: %+v", res)
	}
	retry, err := svc.Submit(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || flushes.Load() != 1 || sinkCalls.Load() != 2 {
		t.Fatalf("posts=%d flushes=%d sinks=%d, want 1, 1, 2", posts.Load(), flushes.Load(), sinkCalls.Load())
	}
	if !retry.Stored || retry.SinkError == "" {
		t.Fatalf("retry = %+v", retry)
	}
}

func TestUnsetSinkMakesNoOutboundRequest(t *testing.T) {
	var calls atomic.Int32
	svc, err := feedback.New(feedback.Options{
		Write: func(context.Context, feedback.Report) error { return nil },
		HTTPClient: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("unexpected request")
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := feedback.ParseFields(validFields("id", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Submit(context.Background(), r)
	if err != nil || res.Sink != "off" || res.SinkError != "" {
		t.Fatalf("result = %+v %v", res, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("outbound calls = %d", calls.Load())
	}
}

func TestNoFlushURLNotesTheBuffer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	svc, err := feedback.New(feedback.Options{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	r, err := feedback.ParseFields(validFields("id", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Submit(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stored || res.Note != feedback.BufferNote || res.FlushError != "" {
		t.Fatalf("result = %+v", res)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func number(v any) int64 {
	switch n := v.(type) {
	case int8:
		return int64(n)
	case int16:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case uint32:
		return int64(n)
	case uint64:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return -1
	}
}
