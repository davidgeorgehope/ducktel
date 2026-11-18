package query

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/writer"
	"github.com/parquet-go/parquet-go"
)

func writeParquetRows[T any](t *testing.T, path string, rows []T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	pw := parquet.NewGenericWriter[T](f)
	if _, err := pw.Write(rows); err != nil {
		pw.Close()
		f.Close()
		t.Fatalf("writing %s: %v", path, err)
	}
	if err := pw.Close(); err != nil {
		f.Close()
		t.Fatalf("closing parquet writer for %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing %s: %v", path, err)
	}
}

func countRows(t *testing.T, e *Engine, since time.Time, sqlStr string) int64 {
	t.Helper()
	var (
		rows []map[string]interface{}
		err  error
	)
	if since.IsZero() {
		rows, _, err = e.Query(sqlStr)
	} else {
		rows, _, err = e.QuerySince(since, sqlStr)
	}
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("query returned %d rows, want 1 aggregate row", len(rows))
	}
	return rows[0]["c"].(int64)
}

// TestQuerySincePrunesOldDayFiles writes the same recent timestamp into an old
// day directory and into today's directory. The row in the old directory is
// indistinguishable by timestamp, so only file selection can exclude it: the
// bounded query must count today's row alone, the unbounded query both.
func TestQuerySincePrunesOldDayFiles(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	// A row well inside the one-hour query window.
	recent := now.Add(-30 * time.Minute).UnixMicro()
	point := writer.MetricPoint{
		MetricName:         "pruning.check",
		MetricType:         "gauge",
		Timestamp:          recent,
		ValueDouble:        1,
		ResourceAttributes: `{}`,
	}

	w := writer.New(dataDir, 0, 1000)
	w.AddMetrics([]writer.MetricPoint{point})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding today's file: %v", err)
	}

	oldPath := filepath.Join(dataDir, "metrics",
		now.AddDate(0, 0, -5).Format("2006-01-02"), "10-00.parquet")
	writeParquetRows(t, oldPath, []writer.MetricPoint{point})

	e, err := Open(dataDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	const q = "SELECT count(*) AS c FROM metrics"
	if got := countRows(t, e, now.Add(-time.Hour), q); got != 1 {
		t.Errorf("QuerySince counted %d row(s), want 1 (only today's file)", got)
	}
	if got := countRows(t, e, time.Time{}, q); got != 2 {
		t.Errorf("Query counted %d row(s), want 2 (today and old day)", got)
	}
}

// metricWithoutExemplars is a deliberately older/smaller schema: it has only
// the columns the query reads, so the metrics view cannot get exemplars from
// the file itself.
type metricWithoutExemplars struct {
	MetricName string `parquet:"metric_name"`
	Timestamp  int64  `parquet:"timestamp"`
}

// TestViewsExposeColumnsMissingFromFiles pins the column safety of the view:
// whichever files are selected, the view must expose every column a query may
// name. Without the UNION ALL BY NAME placeholder, selecting exemplars here
// fails with "column not found" rather than returning NULL.
func TestViewsExposeColumnsMissingFromFiles(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "metrics",
		time.Now().UTC().Format("2006-01-02"), "day.parquet")
	writeParquetRows(t, path, []metricWithoutExemplars{{
		MetricName: "no.exemplars",
		Timestamp:  time.Now().UTC().Add(-time.Minute).UnixMicro(),
	}})

	e, err := Open(dataDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	const q = "SELECT metric_name, exemplars FROM metrics"
	for _, tc := range []struct {
		name  string
		since time.Time
	}{
		{name: "unbounded Query", since: time.Time{}},
		{name: "bounded QuerySince", since: time.Now().UTC().Add(-time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				rows []map[string]interface{}
				err  error
			)
			if tc.since.IsZero() {
				rows, _, err = e.Query(q)
			} else {
				rows, _, err = e.QuerySince(tc.since, q)
			}
			if err != nil {
				t.Fatalf("selecting a column only the placeholder supplies: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			if got := rows[0]["metric_name"]; got != "no.exemplars" {
				t.Errorf("metric_name = %v, want no.exemplars", got)
			}
			if got := rows[0]["exemplars"]; got != nil {
				t.Errorf("exemplars = %v, want NULL", got)
			}
		})
	}
}
