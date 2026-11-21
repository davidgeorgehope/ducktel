package receiver

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func TestFeedbackRouteAuthAndQuery(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	r, err := New("localhost", 0, w, "ingest-token", "flush-token", w.Flush, 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.server.Handler)
	t.Cleanup(srv.Close)

	body := []byte(`{"report_id":"fb-1","tenant":"acme","tool_name":"log_search","category":"bug","severity":"medium","summary":"rows hid the error","details":"empty list","proposed_fix":"show the error","example_call":"log_search()"}`)

	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"ingest token", "ingest-token", http.StatusUnauthorized},
		{"wrong token", "other", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := postFeedback(t, srv.URL, tc.token, body); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}

	bad := postFeedback(t, srv.URL, "flush-token", []byte(`{"tenant":"acme","category":"nope"}`))
	if bad != http.StatusBadRequest {
		t.Fatalf("bad category status = %d", bad)
	}
	oversize := postFeedback(t, srv.URL, "flush-token", bytes.Repeat([]byte("a"), maxFeedbackBody+1))
	if oversize != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d", oversize)
	}

	res, status := postFeedbackBody(t, srv.URL, "flush-token", body)
	if status != http.StatusOK || res["report_id"] != "fb-1" || res["status"] != "stored" {
		t.Fatalf("status %d body %v", status, res)
	}
	res2, status2 := postFeedbackBody(t, srv.URL, "flush-token", body)
	if status2 != http.StatusOK || res2["report_id"] != "fb-1" {
		t.Fatalf("retry status %d body %v", status2, res2)
	}

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	core := telemetry.NewCore(engine)
	rows, _, err := core.SearchLogs("acme", "", "ducktel.agent_feedback", "", "", nil, 60, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0]["severity_text"] != "WARN" || number(rows[0]["severity_number"]) != 13 {
		t.Fatalf("severity = %v %v", rows[0]["severity_text"], rows[0]["severity_number"])
	}
	if !strings.Contains(rows[0]["body"].(string), "show the error") {
		t.Fatalf("body = %v", rows[0]["body"])
	}
	other, _, err := core.SearchLogs("globex", "", "", "", "", nil, 60, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("other tenant saw %d rows", len(other))
	}
}

func postFeedback(t *testing.T, base, token string, body []byte) int {
	t.Helper()
	_, status := postFeedbackBody(t, base, token, body)
	return status
}

func postFeedbackBody(t *testing.T, base, token string, body []byte) (map[string]any, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/feedback", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	json.NewDecoder(resp.Body).Decode(&decoded)
	return decoded, resp.StatusCode
}

func TestFlushFailureDoesNotStoreTheReportTwice(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	logs := 0
	consumer := &countLogs{n: &logs, inner: w}
	r, err := New("localhost", 0, consumer, "", "", func() error {
		return errors.New("disk full")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.server.Handler)
	t.Cleanup(srv.Close)

	body := []byte(`{"report_id":"fb-1","tenant":"acme","tool_name":"log_search","category":"bug","severity":"high","summary":"rows hid the error","details":"empty list","proposed_fix":"show the error"}`)
	res, status := postFeedbackBody(t, srv.URL, "", body)
	if status != http.StatusOK || res["flush_error"] != "disk full" || res["status"] != "stored" {
		t.Fatalf("first status %d body %v", status, res)
	}
	res2, status2 := postFeedbackBody(t, srv.URL, "", body)
	if status2 != http.StatusOK || res2["report_id"] != "fb-1" || res2["flush_error"] != "disk full" {
		t.Fatalf("retry status %d body %v", status2, res2)
	}
	if logs != 1 {
		t.Fatalf("appended %d logs, want 1", logs)
	}
}

type countLogs struct {
	n     *int
	inner interface {
		Add([]writer.TraceSpan)
		AddLogs([]writer.LogRecord)
		AddMetrics([]writer.MetricPoint)
	}
}

func (c *countLogs) Add(spans []writer.TraceSpan) { c.inner.Add(spans) }
func (c *countLogs) AddLogs(recs []writer.LogRecord) {
	*c.n += len(recs)
	c.inner.AddLogs(recs)
}
func (c *countLogs) AddMetrics(points []writer.MetricPoint) { c.inner.AddMetrics(points) }

func number(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	default:
		return -1
	}
}
