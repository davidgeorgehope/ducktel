package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func newSQLCore(t *testing.T) *Core {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		seedSpan("api", "acme", "trace-acme", `{}`),
		seedSpan("worker", "acme", "trace-acme-2", `{}`),
		seedSpan("globex-svc", "globex", "trace-globex", `{}`),
	})
	now := time.Now().UnixMicro()
	w.AddLogs([]writer.LogRecord{
		{Timestamp: now, Body: "acme-line", ServiceName: "api", Attributes: "{}",
			ResourceAttributes: `{"tenant.id":"acme","service.name":"api"}`},
		{Timestamp: now, Body: "globex-secret", ServiceName: "globex-svc", Attributes: "{}",
			ResourceAttributes: `{"tenant.id":"globex","service.name":"globex-svc"}`},
	})
	w.AddMetrics([]writer.MetricPoint{
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: now, ValueDouble: 0.5,
			ServiceName: "api", ResourceAttributes: `{"tenant.id":"acme"}`},
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: now, ValueDouble: 0.9,
			ServiceName: "globex-svc", ResourceAttributes: `{"tenant.id":"globex"}`},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	return NewCore(engine)
}

func querySQL(t *testing.T, c *Core, tenant, statement string, limit int) ([]map[string]any, []string) {
	t.Helper()
	rows, cols, err := c.SQLQuery(context.Background(), tenant, statement, limit)
	if err != nil {
		t.Fatalf("SQLQuery(%q) tenant %q: %v", statement, tenant, err)
	}
	return rows, cols
}

func wantSQLReject(t *testing.T, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsInvalidInput(err) {
		t.Fatalf("got %v, want invalid input", err)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("error %q does not contain %q", err.Error(), reason)
	}
	if !strings.Contains(err.Error(), "list_attribute_keys") {
		t.Fatalf("error %q missing hint", err.Error())
	}
}

func rowsHave(rows []map[string]any, needle string) bool {
	for _, row := range rows {
		for _, v := range row {
			s, ok := v.(string)
			if ok && strings.Contains(s, needle) {
				return true
			}
		}
	}
	return false
}

func numberValue(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case uint64:
		return float64(n)
	default:
		t.Fatalf("value %v (%T) is not a number", v, v)
		return 0
	}
}

func TestSQLQueryTenantIsolation(t *testing.T) {
	c := newSQLCore(t)

	spans, _ := querySQL(t, c, "acme", "SELECT * FROM spans", 0)
	if len(spans) != 2 {
		t.Fatalf("acme spans = %d, want 2", len(spans))
	}
	if rowsHave(spans, "globex") {
		t.Fatalf("acme spans include globex: %v", spans)
	}

	logs, _ := querySQL(t, c, "acme", "SELECT body, service_name FROM logs", 0)
	if len(logs) != 1 || logs[0]["body"] != "acme-line" {
		t.Fatalf("acme logs = %v", logs)
	}
	if rowsHave(logs, "globex") {
		t.Fatalf("acme logs include globex: %v", logs)
	}

	metrics, _ := querySQL(t, c, "acme", "SELECT service_name, value_double FROM metrics", 0)
	if len(metrics) != 1 || metrics[0]["service_name"] != "api" {
		t.Fatalf("acme metrics = %v", metrics)
	}
	if rowsHave(metrics, "globex") {
		t.Fatalf("acme metrics include globex: %v", metrics)
	}

	globex, _ := querySQL(t, c, "globex", "SELECT service_name FROM spans", 0)
	if len(globex) != 1 || globex[0]["service_name"] != "globex-svc" {
		t.Fatalf("globex spans after acme = %v", globex)
	}

	logsView, _, err := c.engine.Query("SELECT body FROM logs")
	if err != nil {
		t.Fatalf("logs view after sql_query: %v", err)
	}
	if len(logsView) != 2 {
		t.Fatalf("logs view rows = %d, want 2", len(logsView))
	}
	metricsView, _, err := c.engine.Query("SELECT service_name FROM metrics")
	if err != nil {
		t.Fatalf("metrics view after sql_query: %v", err)
	}
	if len(metricsView) != 2 {
		t.Fatalf("metrics view rows = %d, want 2", len(metricsView))
	}
}

func TestSQLQueryHidesBaseViews(t *testing.T) {
	c := newSQLCore(t)
	statements := []string{
		"SELECT * FROM traces",
		"SELECT * FROM main.traces",
		"SELECT * FROM _ducktel_logs",
		"SELECT service_name FROM spans WHERE 1 = 0 UNION ALL SELECT service_name FROM traces",
		"WITH spans AS (SELECT * FROM traces) SELECT * FROM spans",
		"WITH RECURSIVE c AS (SELECT * FROM traces UNION ALL SELECT * FROM c WHERE false) SELECT * FROM c",
	}
	for _, statement := range statements {
		t.Run(statement, func(t *testing.T) {
			rows, _, err := c.SQLQuery(context.Background(), "acme", statement, 0)
			wantSQLReject(t, err, "only spans, logs, and metrics are visible")
			if rowsHave(rows, "globex") {
				t.Fatalf("rows include globex: %v", rows)
			}
		})
	}
}

func TestSQLQueryTenantLiteralDoesNotInject(t *testing.T) {
	c := newSQLCore(t)
	rows, _, err := c.SQLQuery(context.Background(), "acme' OR '1'='1", "SELECT * FROM spans", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("injected tenant returned %d rows", len(rows))
	}
	if rowsHave(rows, "globex") {
		t.Fatalf("injected tenant returned globex: %v", rows)
	}
}

func TestSQLQueryRejectsWrites(t *testing.T) {
	c := newSQLCore(t)
	statements := []string{
		"INSERT INTO spans VALUES (1)",
		"UPDATE spans SET span_name = 'x'",
		"DELETE FROM spans",
		"CREATE TABLE t(i INT)",
		"DROP VIEW traces",
		"PRAGMA version",
		"ATTACH 'x.db' AS x",
		"COPY (SELECT 1) TO '/tmp/ducktel-sql-query-copy.csv'",
		"INSTALL json",
		"LOAD json",
		"SELECT * FROM spans /*",
		"SELECT 1 */ ; DROP VIEW traces; /*",
	}
	for _, statement := range statements {
		t.Run(statement, func(t *testing.T) {
			rows, _, err := c.SQLQuery(context.Background(), "acme", statement, 0)
			wantSQLReject(t, err, "only a single SELECT or WITH is allowed")
			if len(rows) != 0 {
				t.Fatalf("rejected statement returned rows: %v", rows)
			}
		})
	}
	rows, _ := querySQL(t, c, "acme", "SELECT count(*) AS n FROM spans", 0)
	if got := numberValue(t, rows[0]["n"]); got != 2 {
		t.Fatalf("count after write attempts = %v, want 2", got)
	}
	rows, _ = querySQL(t, c, "acme", "SELECT count(*) AS n FROM spans -- trailing", 0)
	if got := numberValue(t, rows[0]["n"]); got != 2 {
		t.Fatalf("count after line comment = %v, want 2", got)
	}
}

func TestSQLQueryRejectsFileReads(t *testing.T) {
	c := newSQLCore(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out.csv")
	parquet := filepath.Join(dir, "x.parquet")
	statements := []string{
		"SELECT * FROM read_parquet('" + parquet + "')",
		"SELECT * FROM read_csv('" + out + "')",
		"SELECT read_blob('" + out + "')",
		"SELECT * FROM glob('" + dir + "/*')",
		"COPY (SELECT 1) TO '" + out + "'",
	}
	for _, statement := range statements {
		t.Run(statement, func(t *testing.T) {
			_, _, err := c.SQLQuery(context.Background(), "acme", statement, 0)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !IsInvalidInput(err) {
				t.Fatalf("got %v, want invalid input", err)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Fatalf("file exists after %s: %v", statement, statErr)
			}
		})
	}
}

func TestSQLQueryMultiStatementDoesNotDrop(t *testing.T) {
	c := newSQLCore(t)
	_, _, err := c.SQLQuery(context.Background(), "acme", "SELECT 1; DROP VIEW traces", 0)
	wantSQLReject(t, err, "only a single SELECT or WITH is allowed")
	rows, _ := querySQL(t, c, "acme", "SELECT count(*) AS n FROM spans", 0)
	if got := numberValue(t, rows[0]["n"]); got != 2 {
		t.Fatalf("count after drop attempt = %v, want 2", got)
	}
}

func TestSQLQueryLimits(t *testing.T) {
	c := newSQLCore(t)
	rows, _ := querySQL(t, c, "acme", "SELECT * FROM range(500)", 0)
	if len(rows) != 200 {
		t.Fatalf("omitted limit returned %d rows, want 200", len(rows))
	}
	rows, _ = querySQL(t, c, "acme", "SELECT * FROM range(500)", -3)
	if len(rows) != 200 {
		t.Fatalf("negative limit returned %d rows, want 200", len(rows))
	}
	rows, _ = querySQL(t, c, "acme", "SELECT * FROM range(500)", 2)
	if len(rows) != 2 {
		t.Fatalf("limit 2 returned %d rows", len(rows))
	}
	rows, _, err := c.SQLQuery(context.Background(), "acme", "SELECT * FROM range(500)", 1001)
	if len(rows) != 0 {
		t.Fatalf("limit 1001 returned %d rows", len(rows))
	}
	if !IsInvalidInput(err) {
		t.Fatalf("limit 1001: got %v, want invalid input", err)
	}
}

func TestSQLQueryTimeout(t *testing.T) {
	c := newSQLCore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := c.SQLQuery(ctx, "acme", "SELECT sum(r) FROM range(100000000000) r", 0)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("sum range returned after %s", time.Since(start))
	}
	if err == nil {
		t.Fatal("expected an error")
	}

	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, _, err = c.SQLQuery(ctx, "acme", "SELECT count(*) FROM range(100000000000)", 0)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("count range returned after %s", elapsed)
	}
	if err == nil {
		t.Fatal("expected the range scan to stop")
	}
}

func TestSQLQueryCountFilter(t *testing.T) {
	c := newSQLCore(t)
	rows, cols := querySQL(t, c, "acme", "SELECT count(*) AS n FROM spans", 0)
	if len(cols) != 1 || cols[0] != "n" {
		t.Fatalf("cols = %v", cols)
	}
	if got := numberValue(t, rows[0]["n"]); got != 2 {
		t.Fatalf("acme count = %v, want 2", got)
	}
	rows, cols = querySQL(t, c, "acme", "SELECT span_name, service_name FROM spans WHERE service_name = 'worker'", 0)
	if len(rows) != 1 || rows[0]["service_name"] != "worker" {
		t.Fatalf("filtered spans = %v", rows)
	}
	if cols[0] != "span_name" || cols[1] != "service_name" {
		t.Fatalf("column order = %v", cols)
	}
}

func TestSQLQueryRequiresTenant(t *testing.T) {
	c := newSQLCore(t)
	_, _, err := c.SQLQuery(context.Background(), "", "SELECT 1", 0)
	if !IsInvalidInput(err) {
		t.Fatalf("missing tenant: %v", err)
	}
	_, _, err = c.SQLQuery(context.Background(), "   ", "SELECT 1", 0)
	if !IsInvalidInput(err) {
		t.Fatalf("blank tenant: %v", err)
	}
}

func TestSQLQueryBinderErrorIncludesHint(t *testing.T) {
	c := newSQLCore(t)
	_, _, err := c.SQLQuery(context.Background(), "acme", "SELECT no_such_column FROM spans", 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	if IsInvalidInput(err) {
		t.Fatalf("binder error classified as invalid input: %v", err)
	}
	for _, phrase := range []string{"list_attribute_keys", "spans", "logs", "metrics"} {
		if !strings.Contains(err.Error(), phrase) {
			t.Errorf("error %q missing %q", err.Error(), phrase)
		}
	}
}

func TestSQLQueryDescriptionListsColumns(t *testing.T) {
	c := newSQLCore(t)
	desc := c.SQLQueryDescription()
	for _, phrase := range []string{
		"spans", "logs", "metrics",
		"traces is not a view in this tool",
		"Unix microseconds",
		"SELECT",
	} {
		if !strings.Contains(desc, phrase) {
			t.Errorf("description missing %q", phrase)
		}
	}
	for _, view := range []string{"traces", "logs", "metrics"} {
		cols, err := c.engine.Describe(view)
		if err != nil {
			t.Fatalf("describe %s: %v", view, err)
		}
		if len(cols) == 0 {
			t.Fatalf("describe %s returned no columns", view)
		}
		for _, col := range cols {
			if !strings.Contains(desc, col.Name) {
				t.Errorf("description missing %s column %q", view, col.Name)
			}
		}
	}
}
