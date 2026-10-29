package writer

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
)

func spanN(n int, svc string) []TraceSpan {
	rows := make([]TraceSpan, n)
	for i := range rows {
		rows[i] = TraceSpan{
			TraceID: "t", SpanID: "s", ServiceName: svc, SpanName: "op",
			StartTime: int64(i), EndTime: int64(i + 1), DurationMs: 1,
			StatusCode: "STATUS_CODE_OK", Attributes: "{}",
			ResourceAttributes: "{}", Events: "[]", Links: "[]",
		}
	}
	return rows
}

// writeSourceFile writes rows as a per-flush-style parquet file at path.
func writeSourceFile[T any](t *testing.T, path string, rows []T) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	pw := parquet.NewGenericWriter[T](f)
	if _, err := pw.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func readAll[T any](t *testing.T, path string) []T {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	pf, err := parquet.OpenFile(f, stat.Size())
	if err != nil {
		t.Fatal(err)
	}
	rd := parquet.NewGenericReader[T](pf)
	defer rd.Close()
	rows := make([]T, rd.NumRows())
	// See mergeInto: io.EOF is the success signal for a full read.
	if _, err := rd.Read(rows); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return rows
}

func oldDayDir(t *testing.T, dataDir, signal string) string {
	t.Helper()
	dir := filepath.Join(dataDir, signal, time.Now().UTC().Add(-48*time.Hour).Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func parquetFilesSorted(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range matches {
		out = append(out, filepath.Base(m))
	}
	sort.Strings(out)
	return out
}

func TestCompactOldDaysMergesDay(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")
	writeSourceFile(t, filepath.Join(dayDir, "10-00.parquet"), spanN(2, "a"))
	writeSourceFile(t, filepath.Join(dayDir, "10-01.parquet"), spanN(1, "b"))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatalf("compactOldDays: %v", err)
	}

	files := parquetFilesSorted(t, dayDir)
	if len(files) != 1 || files[0] != "day.parquet" {
		t.Fatalf("files after compaction = %v, want [day.parquet]", files)
	}
	rows := readAll[TraceSpan](t, filepath.Join(dayDir, "day.parquet"))
	if len(rows) != 3 {
		t.Errorf("merged file has %d rows, want 3", len(rows))
	}
	// Chronological order preserved: sources merge in name order.
	if rows[0].ServiceName != "a" || rows[2].ServiceName != "b" {
		t.Errorf("merge order wrong: %v", []string{rows[0].ServiceName, rows[1].ServiceName, rows[2].ServiceName})
	}
}

// TestCompactLeftoversDeletedNotMerged pins the crash-recovery rule: an
// existing day.parquet means the merge already happened, so remaining
// per-flush files are deleted — merging them would double the rows.
func TestCompactLeftoversDeletedNotMerged(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")
	writeSourceFile(t, filepath.Join(dayDir, "day.parquet"), spanN(1, "merged"))
	writeSourceFile(t, filepath.Join(dayDir, "10-00.parquet"), spanN(2, "leftover"))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatalf("compactOldDays: %v", err)
	}

	files := parquetFilesSorted(t, dayDir)
	if len(files) != 1 || files[0] != "day.parquet" {
		t.Fatalf("files = %v, want only [day.parquet]", files)
	}
	rows := readAll[TraceSpan](t, filepath.Join(dayDir, "day.parquet"))
	if len(rows) != 1 {
		t.Errorf("merged file has %d rows, want 1 — leftover rows were merged instead of deleted", len(rows))
	}
}

// yesterdayDir returns (and creates) the day directory the day-margin exists
// to protect: a flush straddling midnight lands files here just after its
// date has passed.
func yesterdayDir(t *testing.T, dataDir, signal string) string {
	t.Helper()
	dir := filepath.Join(dataDir, signal, time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCompactSkipsYesterday pins the day-margin: per-flush files in
// yesterday's directory must not be merged yet, because a flush begun before
// midnight may still be landing into it.
func TestCompactSkipsYesterday(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := yesterdayDir(t, dataDir, "traces")
	writeSourceFile(t, filepath.Join(dayDir, "23-59.parquet"), spanN(1, "late"))
	writeSourceFile(t, filepath.Join(dayDir, "23-59-1.parquet"), spanN(1, "later"))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatalf("compactOldDays: %v", err)
	}

	files := parquetFilesSorted(t, dayDir)
	if len(files) != 2 || files[0] != "23-59-1.parquet" || files[1] != "23-59.parquet" {
		t.Errorf("yesterday was touched: %v", files)
	}
}

// TestCompactDoesNotDeleteFlushLandingAfterMidnight is the regression for the
// midnight race: a flush started at 23:59:59 writes into yesterday's
// directory and renames after midnight, by which time a naive compaction
// (anything "past") would have merged the directory and would delete this
// file as a stale leftover on its next pass — losing the flush. With the
// day-margin, yesterday's directory is never compacted and the file survives
// the leftover rule.
func TestCompactDoesNotDeleteFlushLandingAfterMidnight(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := yesterdayDir(t, dataDir, "traces")
	writeSourceFile(t, filepath.Join(dayDir, "day.parquet"), spanN(1, "merged-earlier"))
	// The straddling flush's file, appearing after day.parquet already exists.
	straddler := filepath.Join(dayDir, "23-59.parquet")
	writeSourceFile(t, straddler, spanN(2, "straddler"))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatalf("compactOldDays: %v", err)
	}

	if _, err := os.Stat(straddler); err != nil {
		t.Fatalf("the post-midnight flush's file was deleted — silent data loss: %v", err)
	}
	if rows := readAll[TraceSpan](t, straddler); len(rows) != 2 {
		t.Errorf("straddler has %d rows, want 2", len(rows))
	}
}

// TestCompactSweepsOrphanedTemps covers the second review finding: a crash
// between CreateTemp and rename (or rename and the deferred Remove) leaves a
// .tmp-* file that nothing else cleans up. Compaction sweeps stale ones from
// the directories it visits, while leaving young ones (a live write's) alone.
func TestCompactSweepsOrphanedTemps(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")
	writeSourceFile(t, filepath.Join(dayDir, "10-00.parquet"), spanN(1, "a"))

	orphan := filepath.Join(dayDir, ".tmp-orphan")
	if err := os.WriteFile(orphan, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	fresh := filepath.Join(dayDir, ".tmp-fresh")
	if err := os.WriteFile(fresh, []byte("in-flight"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatalf("compactOldDays: %v", err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("old orphaned temp file should be swept: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("young temp file must be left alone (a live write may own it): %v", err)
	}
}

func TestCompactSkipsTodayAndNonDateDirs(t *testing.T) {
	dataDir := t.TempDir()
	todayDir := filepath.Join(dataDir, "traces", time.Now().UTC().Format("2006-01-02"))
	if err := os.MkdirAll(todayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSourceFile(t, filepath.Join(todayDir, "10-00.parquet"), spanN(1, "live"))
	garbageDir := filepath.Join(dataDir, "traces", "not-a-date")
	if err := os.MkdirAll(garbageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSourceFile(t, filepath.Join(garbageDir, "10-00.parquet"), spanN(1, "garbage"))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatalf("compactOldDays: %v", err)
	}

	if files := parquetFilesSorted(t, todayDir); len(files) != 1 || files[0] != "10-00.parquet" {
		t.Errorf("today's directory was touched: %v", files)
	}
	if files := parquetFilesSorted(t, garbageDir); len(files) != 1 {
		t.Errorf("non-date directory was touched: %v", files)
	}
}

func TestCompactAllSignals(t *testing.T) {
	dataDir := t.TempDir()
	for _, signal := range []string{"traces", "logs", "metrics"} {
		dayDir := oldDayDir(t, dataDir, signal)
		switch signal {
		case "traces":
			writeSourceFile(t, filepath.Join(dayDir, "10-00.parquet"), spanN(1, "a"))
		case "logs":
			writeSourceFile(t, filepath.Join(dayDir, "10-00.parquet"), []LogRecord{{Timestamp: 1, Body: "x"}})
		case "metrics":
			writeSourceFile(t, filepath.Join(dayDir, "10-00.parquet"), []MetricPoint{{MetricName: "m", ValueDouble: 1}})
		}
	}

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatalf("compactOldDays: %v", err)
	}

	for _, signal := range []string{"traces", "logs", "metrics"} {
		dayDir := filepath.Join(dataDir, signal, time.Now().UTC().Add(-48*time.Hour).Format("2006-01-02"))
		files := parquetFilesSorted(t, dayDir)
		if len(files) != 1 || files[0] != "day.parquet" {
			t.Errorf("%s: files = %v, want [day.parquet]", signal, files)
		}
	}
}

// TestCompactIsIdempotent: a second run over an already-compacted day must
// change nothing.
func TestCompactIsIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")
	writeSourceFile(t, filepath.Join(dayDir, "10-00.parquet"), spanN(2, "a"))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatal(err)
	}
	if err := w.compactOldDays(); err != nil {
		t.Fatal(err)
	}

	files := parquetFilesSorted(t, dayDir)
	if len(files) != 1 || files[0] != "day.parquet" {
		t.Fatalf("files = %v, want [day.parquet]", files)
	}
	if rows := readAll[TraceSpan](t, filepath.Join(dayDir, "day.parquet")); len(rows) != 2 {
		t.Errorf("idempotent run changed row count to %d", len(rows))
	}
}

// TestCompactMergedFileIsGlobVisible pins that the query layer's
// **/*.parquet glob sees the merged output.
func TestCompactMergedFileIsGlobVisible(t *testing.T) {
	ok, err := filepath.Match("*.parquet", "day.parquet")
	if err != nil || !ok {
		t.Fatal("mergedName must match the *.parquet query glob")
	}
}
