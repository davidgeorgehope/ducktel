package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func jsonPrefix(body string) string {
	for _, marker := range []string{"Queries only return", "dashboard_url:"} {
		if i := strings.Index(body, marker); i >= 0 {
			return strings.TrimSpace(body[:i])
		}
	}
	return body
}

func listSavedNames(t *testing.T, s *sdkmcp.ClientSession, tenant string) []string {
	t.Helper()
	body, isErr := call(t, s, "list_saved_queries", map[string]any{"tenant": tenant})
	if isErr {
		t.Fatalf("list_saved_queries: %s", body)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i], _ = r["name"].(string)
	}
	return names
}

func TestSavedQueryRoundTrip(t *testing.T) {
	s := newTestServer(t)
	spanArgs := map[string]any{"tenant": "acme", "service_name": "worker"}

	body, isErr := call(t, s, "save_query", map[string]any{
		"tenant": "acme", "name": "workers", "tool": "span_search", "arguments": spanArgs,
	})
	if isErr {
		t.Fatalf("save_query: %s", body)
	}

	names := listSavedNames(t, s, "acme")
	if len(names) != 1 || names[0] != "workers" {
		t.Fatalf("names = %v", names)
	}

	directBody, isErr := call(t, s, "span_search", spanArgs)
	if isErr {
		t.Fatalf("span_search: %s", directBody)
	}
	runBody, isErr := call(t, s, "run_saved_query", map[string]any{"tenant": "acme", "name": "workers"})
	if isErr {
		t.Fatalf("run_saved_query: %s", runBody)
	}
	if jsonPrefix(runBody) != jsonPrefix(directBody) {
		t.Errorf("run_saved_query body %q != direct %q", jsonPrefix(runBody), jsonPrefix(directBody))
	}
	if strings.Contains(body, "\n") || strings.Contains(body, " ") {
		t.Errorf("save_query JSON is not compact: %s", body)
	}
}

func TestSaveQueryRejectsInvalidInput(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"bad tool", map[string]any{
			"tenant": "acme", "name": "bad-tool", "tool": "not_a_tool",
			"arguments": map[string]any{"tenant": "acme"},
		}},
		{"unknown field", map[string]any{
			"tenant": "acme", "name": "bad-field", "tool": "span_search",
			"arguments": map[string]any{"tenant": "acme", "nope": 1},
		}},
		{"bad window", map[string]any{
			"tenant": "acme", "name": "bad-window", "tool": "span_search",
			"arguments": map[string]any{"tenant": "acme", "since_minutes": 10, "until_minutes": 20},
		}},
		{"bad aggregation", map[string]any{
			"tenant": "acme", "name": "bad-agg", "tool": "metric_query",
			"arguments": map[string]any{"tenant": "acme", "metric_name": "http.server.duration", "aggregation": "nope"},
		}},
		{"missing trace_id", map[string]any{
			"tenant": "acme", "name": "bad-trace", "tool": "trace_lookup",
			"arguments": map[string]any{"tenant": "acme"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := listSavedNames(t, s, "acme")
			_, isErr := call(t, s, "save_query", tc.args)
			if !isErr {
				t.Fatal("expected save error")
			}
			after := listSavedNames(t, s, "acme")
			if len(after) != len(before) {
				t.Fatalf("store grew: before %v after %v", before, after)
			}
		})
	}
}

func TestSaveQueryRejectsTenantMismatch(t *testing.T) {
	s := newTestServer(t)
	before := listSavedNames(t, s, "acme")
	_, isErr := call(t, s, "save_query", map[string]any{
		"tenant": "acme", "name": "mismatch", "tool": "span_search",
		"arguments": map[string]any{"tenant": "other", "service_name": "worker"},
	})
	if !isErr {
		t.Fatal("expected mismatch error")
	}
	if len(listSavedNames(t, s, "acme")) != len(before) {
		t.Fatal("store changed")
	}
}

func TestSavedQueryTenantIsolation(t *testing.T) {
	s := newTestServer(t)
	_, isErr := call(t, s, "save_query", map[string]any{
		"tenant": "acme", "name": "secret", "tool": "span_search",
		"arguments": map[string]any{"tenant": "acme", "service_name": "worker"},
	})
	if isErr {
		t.Fatal(isErr)
	}

	listB, isErr := call(t, s, "list_saved_queries", map[string]any{"tenant": "other"})
	if isErr {
		t.Fatal(listB)
	}
	if listB != "[]" {
		t.Errorf("tenant B list = %s, want []", listB)
	}

	_, isErr = call(t, s, "run_saved_query", map[string]any{"tenant": "other", "name": "secret"})
	if !isErr {
		t.Fatal("expected run error for wrong tenant")
	}

	body, isErr := call(t, s, "run_saved_query", map[string]any{"tenant": "acme", "name": "missing"})
	if !isErr {
		t.Fatal("expected unknown name error")
	}
	if strings.Contains(body, "other") {
		t.Errorf("leaked other tenant: %s", body)
	}
	if !strings.Contains(body, "secret") {
		t.Errorf("should list acme names: %s", body)
	}
}

func TestRunSavedQueryUnknownTenantEmpty(t *testing.T) {
	s := newTestServer(t)
	body, isErr := call(t, s, "run_saved_query", map[string]any{"tenant": "lonely", "name": "x"})
	if !isErr {
		t.Fatal("expected error")
	}
	if !strings.Contains(body, "no saved queries") {
		t.Errorf("body = %s", body)
	}
}

func TestSavedQueryWindowOverride(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	stale := seedSpan("api", "acme", "trace-old")
	stale.StartTime = old.UnixMicro()
	stale.EndTime = old.UnixMicro()
	s := newTestServerWithSpans(t, nil, []writer.TraceSpan{stale})

	_, isErr := call(t, s, "save_query", map[string]any{
		"tenant": "acme", "name": "spans", "tool": "span_search",
		"arguments": map[string]any{"tenant": "acme", "since_minutes": 180},
	})
	if isErr {
		t.Fatal("save")
	}

	before, _ := call(t, s, "list_saved_queries", map[string]any{"tenant": "acme"})
	runNarrow, isErr := call(t, s, "run_saved_query", map[string]any{
		"tenant": "acme", "name": "spans", "since_minutes": 30,
	})
	if isErr {
		t.Fatalf("run narrow: %s", runNarrow)
	}
	runStored, isErr := call(t, s, "run_saved_query", map[string]any{"tenant": "acme", "name": "spans"})
	if isErr {
		t.Fatalf("run stored window: %s", runStored)
	}
	if countRows(t, runNarrow) >= countRows(t, runStored) {
		t.Errorf("narrow=%d rows stored-window=%d rows, want narrow < stored", countRows(t, runNarrow), countRows(t, runStored))
	}
	after, _ := call(t, s, "list_saved_queries", map[string]any{"tenant": "acme"})
	if before != after {
		t.Error("stored arguments changed after override run")
	}
}

func TestSaveQueryFillsOmittedTenant(t *testing.T) {
	s := newTestServer(t)
	body, isErr := call(t, s, "save_query", map[string]any{
		"tenant": "acme", "name": "workers", "tool": "span_search",
		"arguments": map[string]any{"service_name": "worker"},
	})
	if isErr {
		t.Fatalf("save: %s", body)
	}
	listed, isErr := call(t, s, "list_saved_queries", map[string]any{"tenant": "acme"})
	if isErr {
		t.Fatal(listed)
	}
	var rows []struct {
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(listed), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Arguments["tenant"] != "acme" {
		t.Fatalf("stored arguments = %#v", rows)
	}
}

func TestRunSavedQueryRejectsUntilOnAttributeKeys(t *testing.T) {
	s := newTestServer(t)
	_, isErr := call(t, s, "save_query", map[string]any{
		"tenant": "acme", "name": "keys", "tool": "list_attribute_keys",
		"arguments": map[string]any{"tenant": "acme"},
	})
	if isErr {
		t.Fatal("save")
	}
	body, isErr := call(t, s, "run_saved_query", map[string]any{
		"tenant": "acme", "name": "keys", "until_minutes": 5,
	})
	if !isErr || !strings.Contains(body, "until_minutes") {
		t.Fatalf("until override: err=%v body=%s", isErr, body)
	}
	body, isErr = call(t, s, "run_saved_query", map[string]any{
		"tenant": "acme", "name": "keys", "since_minutes": 120,
	})
	if isErr {
		t.Fatalf("since override: %s", body)
	}
}

func TestRunSavedQueryRejectsOverrideOnTraceLookup(t *testing.T) {
	s := newTestServer(t)
	_, isErr := call(t, s, "save_query", map[string]any{
		"tenant": "acme", "name": "t", "tool": "trace_lookup",
		"arguments": map[string]any{"tenant": "acme", "trace_id": "trace-acme-1"},
	})
	if isErr {
		t.Fatal("save")
	}
	_, isErr = call(t, s, "run_saved_query", map[string]any{
		"tenant": "acme", "name": "t", "since_minutes": 5,
	})
	if !isErr {
		t.Fatal("expected override error")
	}
}

func TestSaveQueryUpsertOneRecord(t *testing.T) {
	s := newTestServer(t)
	for _, desc := range []string{"first", "second"} {
		_, isErr := call(t, s, "save_query", map[string]any{
			"tenant": "acme", "name": "q", "tool": "list_services",
			"arguments": map[string]any{"tenant": "acme"}, "description": desc,
		})
		if isErr {
			t.Fatal("save failed")
		}
	}
	body, _ := call(t, s, "list_saved_queries", map[string]any{"tenant": "acme"})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want one record, got %d", len(rows))
	}
	if rows[0]["description"] != "second" {
		t.Errorf("description = %v", rows[0]["description"])
	}
}

func TestSaveQueryRunsSQLQuery(t *testing.T) {
	s := newTestServer(t)
	body, isErr := call(t, s, "save_query", map[string]any{
		"tenant": "acme",
		"name":   "span-count",
		"tool":   "sql_query",
		"arguments": map[string]any{
			"tenant": "acme",
			"sql":    "SELECT count(*) AS n FROM spans",
		},
	})
	if isErr {
		t.Fatalf("save sql_query: %s", body)
	}
	body, isErr = call(t, s, "run_saved_query", map[string]any{
		"tenant": "acme",
		"name":   "span-count",
	})
	if isErr {
		t.Fatalf("run sql_query: %s", body)
	}
	if !strings.Contains(body, `"n":`) {
		t.Fatalf("run sql_query body = %s", body)
	}
	since := 5
	_, isErr = call(t, s, "run_saved_query", map[string]any{
		"tenant":        "acme",
		"name":          "span-count",
		"since_minutes": since,
	})
	if !isErr {
		t.Fatal("sql_query has no time window, so since_minutes must be rejected")
	}
}

func TestSaveQueryRejectsNonSavableTools(t *testing.T) {
	s := newTestServer(t)
	for _, tool := range []string{"flush_buffer", "list_tenants", "save_query", "list_saved_queries", "run_saved_query", "submit_feedback"} {
		_, isErr := call(t, s, "save_query", map[string]any{
			"tenant": "acme", "name": "x", "tool": tool,
			"arguments": map[string]any{"tenant": "acme"},
		})
		if !isErr {
			t.Fatalf("%s should be rejected", tool)
		}
	}
}

func TestSavedToolsAlwaysRegistered(t *testing.T) {
	t.Run("without_flush", func(t *testing.T) {
		assertSavedToolsRegistered(t, newTestServer(t))
	})
	t.Run("with_flush", func(t *testing.T) {
		dir := t.TempDir()
		engine, err := query.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer engine.Close()
		srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "ducktel-test", Version: "test"}, SDKOptions())
		NewServer(engine, dir, "http://localhost/flush", "tok").Register(srv)
		clientT, serverT := sdkmcp.NewInMemoryTransports()
		ctx := context.Background()
		if _, err := srv.Connect(ctx, serverT, nil); err != nil {
			t.Fatal(err)
		}
		client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil)
		session, err := client.Connect(ctx, clientT, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		assertSavedToolsRegistered(t, session)
	})
}

func assertSavedToolsRegistered(t *testing.T, s *sdkmcp.ClientSession) {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, name := range []string{"list_saved_queries", "save_query", "run_saved_query"} {
		if !got[name] {
			t.Errorf("missing %s", name)
		}
	}
}
