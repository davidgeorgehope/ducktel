package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// newCompareServer seeds ten slow (cache=miss) and ten fast (cache=hit) spans
// and returns the session plus the same Core, so the tool's answer can be
// checked against the core answer for identical options.
func newCompareServer(t *testing.T, configure func(*Server)) (*sdkmcp.ClientSession, *telemetry.Core) {
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

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "ducktel-test", Version: "test"}, nil)
	ds := NewServer(engine, "", "")
	if configure != nil {
		configure(ds)
	}
	ds.Register(srv)

	clientT, serverT := sdkmcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session, core
}

func TestCompareSpansToolMatchesCore(t *testing.T) {
	s, core := newCompareServer(t, nil)

	args := map[string]any{
		"tenant": "acme", "service_name": "api", "span_name": "op",
		"since_minutes": 60, "min_ms": 50, "max_ms": 200, "limit": 15,
	}
	body, isErr := call(t, s, "compare_spans", args)
	if isErr {
		t.Fatalf("compare_spans: %s", body)
	}
	var got telemetry.SpanComparison
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("compare_spans is not JSON: %v\n%s", err, body)
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
		t.Errorf("tool result\ngot:  %+v\nwant: %+v", got, *want)
	}

	if got.SlowN != 10 || got.RestN != 10 {
		t.Errorf("slow_n/rest_n = %d/%d, want 10/10", got.SlowN, got.RestN)
	}
	if len(got.Rows) == 0 || got.Rows[0].Key != "cache" || got.Rows[0].Value != "miss" {
		t.Fatalf("first row = %+v, want cache=miss", got.Rows)
	}
	if got.Rows[0].SlowPct != 100 || got.Rows[0].RestPct != 0 {
		t.Errorf("cache=miss = %v/%v, want 100/0", got.Rows[0].SlowPct, got.Rows[0].RestPct)
	}
}

func TestCompareSpansToolInvalidInput(t *testing.T) {
	s, _ := newCompareServer(t, nil)

	body, isErr := call(t, s, "compare_spans", map[string]any{
		"tenant": "acme", "min_ms": 200, "max_ms": 50,
	})
	if !isErr {
		t.Errorf("min_ms above max_ms should be a tool error, got: %s", body)
	}

	body, isErr = call(t, s, "compare_spans", map[string]any{
		"tenant": "acme", "since_minutes": 10, "until_minutes": 10,
	})
	if !isErr {
		t.Errorf("until_minutes at since_minutes should be a tool error, got: %s", body)
	}
}

func TestCompareSpansToolDashboardLink(t *testing.T) {
	s, _ := newCompareServer(t, func(ds *Server) {
		ds.SetDashboardURL("http://dash.example")
	})

	body, isErr := call(t, s, "compare_spans", map[string]any{
		"tenant": "acme", "service_name": "api", "since_minutes": 60, "min_ms": 50,
	})
	if isErr {
		t.Fatalf("compare_spans: %s", body)
	}
	if !strings.Contains(body, "dashboard_url: http://dash.example#compare?") {
		t.Errorf("missing compare dashboard link: %s", body)
	}
	for _, want := range []string{"tenant=acme", "service_name=api", "since_minutes=60", "min_ms=50"} {
		if !strings.Contains(body, want) {
			t.Errorf("link is missing %q: %s", want, body)
		}
	}
}

func TestCompareSpansToolIsRegistered(t *testing.T) {
	s, _ := newCompareServer(t, nil)

	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "compare_spans" {
			if tool.Description == "" {
				t.Error("compare_spans has no description")
			}
			return
		}
	}
	t.Error("compare_spans is not registered")
}
