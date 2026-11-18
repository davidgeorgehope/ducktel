package query

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// cachePoint is one metric row for the view-cache tests.
func cachePoint(name string, ts time.Time) writer.MetricPoint {
	return writer.MetricPoint{
		MetricName:         name,
		MetricType:         "gauge",
		Timestamp:          ts.UnixMicro(),
		ValueDouble:        1,
		ResourceAttributes: `{}`,
	}
}

// TestViewCacheSeesFileWrittenAfterOpen: remembering the file list must not
// stop a file written after Open from being seen by the next query.
func TestViewCacheSeesFileWrittenAfterOpen(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	const q = "SELECT count(*) AS c FROM metrics"
	if got := countRows(t, e, time.Time{}, q); got != 0 {
		t.Fatalf("empty store count = %d, want 0", got)
	}

	now := time.Now().UTC()
	path := filepath.Join(dir, "metrics", now.Format("2006-01-02"), "12-00.parquet")
	writeParquetRows(t, path, []writer.MetricPoint{cachePoint("late.file", now)})

	if got := countRows(t, e, time.Time{}, q); got != 1 {
		t.Errorf("count after a file was written = %d, want 1 (the new file is invisible)", got)
	}
}

// TestViewCacheSeesFileReplacedInPlace: compaction rewrites day.parquet and
// HH.parquet under the same name. A path-only comparison would keep the old
// view; size and mtime must make the new contents visible.
func TestViewCacheSeesFileReplacedInPlace(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	path := filepath.Join(dir, "metrics", now.Format("2006-01-02"), "day.parquet")
	writeParquetRows(t, path, []writer.MetricPoint{cachePoint("before.compaction", now)})

	e, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	const q = "SELECT count(*) AS c FROM metrics"
	if got := countRows(t, e, time.Time{}, q); got != 1 {
		t.Fatalf("count before replacement = %d, want 1", got)
	}

	// Rewrite the same path with more rows, then force a distinct mtime so the
	// test does not depend on filesystem timestamp resolution.
	writeParquetRows(t, path, []writer.MetricPoint{
		cachePoint("after.compaction.a", now),
		cachePoint("after.compaction.b", now),
	})
	if err := os.Chtimes(path, now, now.Add(time.Minute)); err != nil {
		t.Fatalf("setting mtime on %s: %v", path, err)
	}

	if got := countRows(t, e, time.Time{}, q); got != 2 {
		t.Errorf("count after replacing the file = %d, want 2 (stale view kept the old rows)", got)
	}
}

// TestViewCacheSurvivesFileRemoval: compaction deletes per-flush files. The
// next query must rebuild the view from what is left, not fail on the gone
// paths.
func TestViewCacheSurvivesFileRemoval(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	dayDir := filepath.Join(dir, "metrics", now.Format("2006-01-02"))
	first := filepath.Join(dayDir, "10-00.parquet")
	second := filepath.Join(dayDir, "10-00-1.parquet")
	writeParquetRows(t, first, []writer.MetricPoint{cachePoint("flush.one", now)})
	writeParquetRows(t, second, []writer.MetricPoint{cachePoint("flush.two", now)})

	e, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	const q = "SELECT count(*) AS c FROM metrics"
	if got := countRows(t, e, time.Time{}, q); got != 2 {
		t.Fatalf("count before removal = %d, want 2", got)
	}

	if err := os.Remove(first); err != nil {
		t.Fatalf("removing %s: %v", first, err)
	}
	if got := countRows(t, e, time.Time{}, q); got != 1 {
		t.Errorf("count after removing one file = %d, want 1", got)
	}

	if err := os.Remove(second); err != nil {
		t.Fatalf("removing %s: %v", second, err)
	}
	if got := countRows(t, e, time.Time{}, q); got != 0 {
		t.Errorf("count after removing every file = %d, want 0", got)
	}
}

// TestViewCacheSkipsUnchangedRebuild pins the optimization itself: with an
// unchanged file set, consecutive queries must not run CREATE OR REPLACE VIEW.
func TestViewCacheSkipsUnchangedRebuild(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	path := filepath.Join(dir, "metrics", now.Format("2006-01-02"), "day.parquet")
	writeParquetRows(t, path, []writer.MetricPoint{cachePoint("cached.metric", now)})

	e, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	if e.viewBuilds == 0 {
		t.Fatal("Open built no views; the counter is not wired up")
	}
	before := e.viewBuilds

	const q = "SELECT count(*) AS c FROM metrics"
	if got := countRows(t, e, time.Time{}, q); got != 1 {
		t.Fatalf("first query count = %d, want 1", got)
	}
	if e.viewBuilds != before {
		t.Errorf("first query rebuilt views (%d -> %d) for an unchanged file set", before, e.viewBuilds)
	}

	before = e.viewBuilds
	if got := countRows(t, e, time.Time{}, q); got != 1 {
		t.Fatalf("second query count = %d, want 1", got)
	}
	if e.viewBuilds != before {
		t.Errorf("second query rebuilt views (%d -> %d) for an unchanged file set", before, e.viewBuilds)
	}
}
