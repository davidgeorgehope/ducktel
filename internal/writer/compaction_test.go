package writer

import (
	"context"
	"fmt"
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
	if err := w.compactOldDays(context.Background()); err != nil {
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
	if err := w.compactOldDays(context.Background()); err != nil {
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
	if err := w.compactOldDays(context.Background()); err != nil {
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
	if err := w.compactOldDays(context.Background()); err != nil {
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
	if err := w.compactOldDays(context.Background()); err != nil {
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
	if err := w.compactOldDays(context.Background()); err != nil {
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
	if err := w.compactOldDays(context.Background()); err != nil {
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
	if err := w.compactOldDays(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.compactOldDays(context.Background()); err != nil {
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

// hourTestNow is a fixed clock so hour eligibility is deterministic.
var hourTestNow = time.Date(2025, 6, 15, 12, 30, 0, 0, time.UTC)

func hourDayDir(t *testing.T, dataDir, signal string, day time.Time) string {
	t.Helper()
	dir := filepath.Join(dataDir, signal, day.UTC().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHourFileNames(t *testing.T) {
	names := []string{"08-00.parquet", "07.parquet", "07-59.parquet"}
	sort.Strings(names)
	want := []string{"07-59.parquet", "07.parquet", "08-00.parquet"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("name order = %v, want %v (hour files must sort chronologically)", names, want)
		}
	}

	if !isHourFileName("07.parquet") || !isHourFileName("23.parquet") {
		t.Error("valid hour files not recognised")
	}
	for _, bad := range []string{"7.parquet", "07-59.parquet", "day.parquet", "24.parquet", "07-59-1.parquet", ".07.parquet"} {
		if isHourFileName(bad) {
			t.Errorf("%q wrongly recognised as an hour file", bad)
		}
	}

	for name, want := range map[string]int{
		"07-59.parquet":   7,
		"07-59-3.parquet": 7,
		"23-00.parquet":   23,
	} {
		hour, ok := flushFileHour(name)
		if !ok || hour != want {
			t.Errorf("flushFileHour(%q) = %d, %v; want %d, true", name, hour, ok, want)
		}
	}
	for _, bad := range []string{"07.parquet", "day.parquet", "00000.parquet", "7-59.parquet", "07-5.parquet", "07-59-.parquet"} {
		if _, ok := flushFileHour(bad); ok {
			t.Errorf("%q wrongly recognised as a per-flush file", bad)
		}
	}
}

func TestCompactHoursMergesMetricsSorted(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := hourDayDir(t, dataDir, "metrics", hourTestNow)
	writeSourceFile(t, filepath.Join(dayDir, "09-00.parquet"), []MetricPoint{
		{MetricName: "b", Timestamp: 2},
		{MetricName: "a", Timestamp: 3},
	})
	writeSourceFile(t, filepath.Join(dayDir, "09-01.parquet"), []MetricPoint{
		{MetricName: "a", Timestamp: 1},
		{MetricName: "b", Timestamp: 1},
	})

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactHours(context.Background(), hourTestNow); err != nil {
		t.Fatalf("compactHours: %v", err)
	}

	if files := parquetFilesSorted(t, dayDir); len(files) != 1 || files[0] != "09.parquet" {
		t.Fatalf("files = %v, want [09.parquet]", files)
	}
	rows := readAll[MetricPoint](t, filepath.Join(dayDir, "09.parquet"))
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	want := [][2]any{{"a", int64(1)}, {"a", int64(3)}, {"b", int64(1)}, {"b", int64(2)}}
	for i, r := range rows {
		if r.MetricName != want[i][0] || r.Timestamp != want[i][1] {
			t.Fatalf("row %d = (%s, %d), want (%v, %v): not sorted by (MetricName, Timestamp)",
				i, r.MetricName, r.Timestamp, want[i][0], want[i][1])
		}
	}
}

func TestCompactHoursMergesTracesSorted(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := hourDayDir(t, dataDir, "traces", hourTestNow)
	writeSourceFile(t, filepath.Join(dayDir, "09-00.parquet"), []TraceSpan{
		{TraceID: "b", StartTime: 2},
		{TraceID: "a", StartTime: 3},
	})
	writeSourceFile(t, filepath.Join(dayDir, "09-01.parquet"), []TraceSpan{
		{TraceID: "a", StartTime: 1},
		{TraceID: "b", StartTime: 1},
	})

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactHours(context.Background(), hourTestNow); err != nil {
		t.Fatalf("compactHours: %v", err)
	}

	if files := parquetFilesSorted(t, dayDir); len(files) != 1 || files[0] != "09.parquet" {
		t.Fatalf("files = %v, want [09.parquet]", files)
	}
	rows := readAll[TraceSpan](t, filepath.Join(dayDir, "09.parquet"))
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	want := [][2]any{{"a", int64(1)}, {"a", int64(3)}, {"b", int64(1)}, {"b", int64(2)}}
	for i, r := range rows {
		if r.TraceID != want[i][0] || r.StartTime != want[i][1] {
			t.Fatalf("row %d = (%s, %d), want (%v, %v): not sorted by (TraceID, StartTime)",
				i, r.TraceID, r.StartTime, want[i][0], want[i][1])
		}
	}
}

func TestCompactHoursMergesLogsSorted(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := hourDayDir(t, dataDir, "logs", hourTestNow)
	writeSourceFile(t, filepath.Join(dayDir, "09-00.parquet"), []LogRecord{
		{ServiceName: "b", Timestamp: 2},
		{ServiceName: "a", Timestamp: 3},
	})
	writeSourceFile(t, filepath.Join(dayDir, "09-01.parquet"), []LogRecord{
		{ServiceName: "a", Timestamp: 1},
		{ServiceName: "b", Timestamp: 1},
	})

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactHours(context.Background(), hourTestNow); err != nil {
		t.Fatalf("compactHours: %v", err)
	}

	if files := parquetFilesSorted(t, dayDir); len(files) != 1 || files[0] != "09.parquet" {
		t.Fatalf("files = %v, want [09.parquet]", files)
	}
	rows := readAll[LogRecord](t, filepath.Join(dayDir, "09.parquet"))
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	want := [][2]any{{"a", int64(1)}, {"a", int64(3)}, {"b", int64(1)}, {"b", int64(2)}}
	for i, r := range rows {
		if r.ServiceName != want[i][0] || r.Timestamp != want[i][1] {
			t.Fatalf("row %d = (%s, %d), want (%v, %v): not sorted by (ServiceName, Timestamp)",
				i, r.ServiceName, r.Timestamp, want[i][0], want[i][1])
		}
	}
}

// TestCompactHoursSkipsOpenHours pins the two-hour margin: the current hour
// and the one before it cannot be merged yet.
func TestCompactHoursSkipsOpenHours(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := hourDayDir(t, dataDir, "traces", hourTestNow)
	writeSourceFile(t, filepath.Join(dayDir, "12-00.parquet"), spanN(1, "current"))
	writeSourceFile(t, filepath.Join(dayDir, "11-00.parquet"), spanN(1, "previous"))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactHours(context.Background(), hourTestNow); err != nil {
		t.Fatalf("compactHours: %v", err)
	}

	files := parquetFilesSorted(t, dayDir)
	if len(files) != 2 || files[0] != "11-00.parquet" || files[1] != "12-00.parquet" {
		t.Errorf("open hours were touched: %v", files)
	}
}

// TestCompactHoursYesterdayAndOlder covers both sides of the day window:
// yesterday's closed hours are merged, a two-day-old directory is left for
// compactOldDays.
func TestCompactHoursYesterdayAndOlder(t *testing.T) {
	dataDir := t.TempDir()
	yesterday := hourDayDir(t, dataDir, "traces", hourTestNow.AddDate(0, 0, -1))
	writeSourceFile(t, filepath.Join(yesterday, "23-10.parquet"), spanN(2, "late"))
	older := hourDayDir(t, dataDir, "traces", hourTestNow.AddDate(0, 0, -2))
	writeSourceFile(t, filepath.Join(older, "10-00.parquet"), spanN(2, "old"))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactHours(context.Background(), hourTestNow); err != nil {
		t.Fatalf("compactHours: %v", err)
	}

	if files := parquetFilesSorted(t, yesterday); len(files) != 1 || files[0] != "23.parquet" {
		t.Errorf("yesterday's closed hour not merged: %v", files)
	}
	if rows := readAll[TraceSpan](t, filepath.Join(yesterday, "23.parquet")); len(rows) != 2 {
		t.Errorf("yesterday's merged hour has %d rows, want 2", len(rows))
	}
	if files := parquetFilesSorted(t, older); len(files) != 1 || files[0] != "10-00.parquet" {
		t.Errorf("a two-day-old directory was touched by compactHours: %v", files)
	}
}

// TestCompactHoursLeftoversDeleted pins the same crash-recovery rule as the
// day compactor: an existing HH.parquet means the merge happened, so the
// remaining per-flush files of that hour are deleted, not merged.
func TestCompactHoursLeftoversDeleted(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := hourDayDir(t, dataDir, "traces", hourTestNow)
	merged := filepath.Join(dayDir, "05.parquet")
	writeSourceFile(t, merged, spanN(2, "merged"))
	writeSourceFile(t, filepath.Join(dayDir, "05-05.parquet"), spanN(3, "leftover"))
	writeSourceFile(t, filepath.Join(dayDir, "05-05-1.parquet"), spanN(4, "leftover-2"))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactHours(context.Background(), hourTestNow); err != nil {
		t.Fatalf("compactHours: %v", err)
	}

	files := parquetFilesSorted(t, dayDir)
	if len(files) != 1 || files[0] != "05.parquet" {
		t.Fatalf("files = %v, want only [05.parquet]", files)
	}
	if rows := readAll[TraceSpan](t, merged); len(rows) != 2 {
		t.Errorf("merged hour has %d rows, want 2: leftovers were merged instead of deleted", len(rows))
	}
}

// TestCompactHoursStreamsAboveSortCap: above the cap the hour merges in name
// order without sorting, keeping memory bounded. The cap is lowered so the
// test does not need 20,000 rows.
func TestCompactHoursStreamsAboveSortCap(t *testing.T) {
	oldCap := hourSortMaxRows
	hourSortMaxRows = 4
	defer func() { hourSortMaxRows = oldCap }()

	dataDir := t.TempDir()
	dayDir := hourDayDir(t, dataDir, "traces", hourTestNow)
	writeSourceFile(t, filepath.Join(dayDir, "09-00.parquet"), spansFrom(30, 2))
	writeSourceFile(t, filepath.Join(dayDir, "09-01.parquet"), spansFrom(20, 2))
	writeSourceFile(t, filepath.Join(dayDir, "09-02.parquet"), spansFrom(10, 2))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactHours(context.Background(), hourTestNow); err != nil {
		t.Fatalf("compactHours: %v", err)
	}

	rows := readAll[TraceSpan](t, filepath.Join(dayDir, "09.parquet"))
	want := []int64{30, 31, 20, 21, 10, 11}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for i, r := range rows {
		if r.StartTime != want[i] {
			t.Fatalf("row %d has StartTime %d, want %d: above the cap rows must stream in name order",
				i, r.StartTime, want[i])
		}
	}
}

func TestCompactHoursIsIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := hourDayDir(t, dataDir, "metrics", hourTestNow)
	writeSourceFile(t, filepath.Join(dayDir, "09-00.parquet"), []MetricPoint{
		{MetricName: "b", Timestamp: 2},
		{MetricName: "a", Timestamp: 1},
	})

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactHours(context.Background(), hourTestNow); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(dayDir, "09.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.compactHours(context.Background(), hourTestNow); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(dayDir, "09.parquet"))
	if err != nil {
		t.Fatal(err)
	}

	if files := parquetFilesSorted(t, dayDir); len(files) != 1 || files[0] != "09.parquet" {
		t.Fatalf("files = %v, want [09.parquet]", files)
	}
	if !os.SameFile(before, after) {
		t.Error("a second compactHours rewrote the merged hour file")
	}
	if rows := readAll[MetricPoint](t, filepath.Join(dayDir, "09.parquet")); len(rows) != 2 {
		t.Errorf("idempotent run changed row count to %d", len(rows))
	}
}

// TestCompactDayCutsAtHourBoundary: a day merged from hour files keeps one
// row group per hour (when each hour is large enough to be worth a group).
func TestCompactDayCutsAtHourBoundary(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")
	writeSourceFile(t, filepath.Join(dayDir, "10.parquet"), spansFrom(0, 1500))
	writeSourceFile(t, filepath.Join(dayDir, "11.parquet"), spansFrom(1500, 1500))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(context.Background()); err != nil {
		t.Fatal(err)
	}

	merged := filepath.Join(dayDir, mergedName)
	if got := rowGroupCount(t, merged); got != 2 {
		t.Errorf("row groups = %d, want 2 (one per hour file)", got)
	}
	assertSequential(t, readAll[TraceSpan](t, merged), 3000)
}

// TestCompactDayTinyHoursStayOneGroup: a day of tiny hours must not leave
// fragmented row groups, or isFragmented would rewrite it on every pass.
func TestCompactDayTinyHoursStayOneGroup(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")
	for i := 0; i < 20; i++ {
		writeSourceFile(t, filepath.Join(dayDir, fmt.Sprintf("%02d.parquet", i)), spansFrom(i*10, 10))
	}

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(context.Background()); err != nil {
		t.Fatal(err)
	}

	merged := filepath.Join(dayDir, mergedName)
	if got := rowGroupCount(t, merged); got != 1 {
		t.Errorf("row groups = %d, want 1", got)
	}
	assertSequential(t, readAll[TraceSpan](t, merged), 200)
}

// TestCompactDayPerFlushOnlyCapsAtSize: per-flush sources must not cause a
// row-group cut each. The cap is lowered to keep the test small; the point is
// that 6 sources do not produce 6 groups.
func TestCompactDayPerFlushOnlyCapsAtSize(t *testing.T) {
	oldCap := compactRowGroupRows
	compactRowGroupRows = 10
	defer func() { compactRowGroupRows = oldCap }()

	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")
	for i := 0; i < 6; i++ {
		writeSourceFile(t, filepath.Join(dayDir, fmt.Sprintf("10-0%d.parquet", i)), spansFrom(i*4, 4))
	}

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(context.Background()); err != nil {
		t.Fatal(err)
	}

	merged := filepath.Join(dayDir, mergedName)
	if got := rowGroupCount(t, merged); got != 3 {
		t.Errorf("row groups = %d, want 3 (groups capped at the row limit, not cut per source)", got)
	}
	assertSequential(t, readAll[TraceSpan](t, merged), 24)
}

// TestCompactHoursSortedKeepsEveryRowAcrossBatches: the sorted path collects
// rows from several read batches (compactReadBatch) of several files. Every
// row must arrive intact, so a reused read buffer cannot leak one batch's
// values into another's rows.
func TestCompactHoursSortedKeepsEveryRowAcrossBatches(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := hourDayDir(t, dataDir, "metrics", hourTestNow)
	want := map[string]int{}
	for f := 0; f < 3; f++ {
		rows := make([]MetricPoint, 5000)
		for i := range rows {
			name := fmt.Sprintf("m%02d", (i*7+f)%23)
			attrs := fmt.Sprintf(`{"file":%d,"i":%d}`, f, i)
			rows[i] = MetricPoint{MetricName: name, Timestamp: int64(i), Attributes: attrs}
			want[name+"|"+attrs]++
		}
		writeSourceFile(t, filepath.Join(dayDir, fmt.Sprintf("09-%02d.parquet", f)), rows)
	}
	if err := New(dataDir, time.Hour, 1000).compactHours(context.Background(), hourTestNow); err != nil {
		t.Fatalf("compactHours: %v", err)
	}
	rows := readAll[MetricPoint](t, filepath.Join(dayDir, "09.parquet"))
	if len(rows) != 15000 {
		t.Fatalf("rows = %d, want 15000", len(rows))
	}
	for i, r := range rows {
		if i > 0 && metricPointLess(r, rows[i-1]) {
			t.Fatalf("row %d out of order", i)
		}
		k := r.MetricName + "|" + r.Attributes
		if want[k] == 0 {
			t.Fatalf("row %d (%s) was not in any source, or appears twice", i, k)
		}
		want[k]--
	}
}
