package writer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
)

func rowGroupCount(t *testing.T, path string) int {
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
	return len(pf.RowGroups())
}

// spansFrom numbers rows from start so order can be checked across files.
func spansFrom(start, n int) []TraceSpan {
	rows := spanN(n, "svc")
	for i := range rows {
		rows[i].StartTime = int64(start + i)
	}
	return rows
}

// writeFragmented writes rows as a day.parquet the way the old compactor did:
// one row group per chunk of chunk rows.
func writeFragmented(t *testing.T, path string, rows []TraceSpan, chunk int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	pw := parquet.NewGenericWriter[TraceSpan](f)
	for i := 0; i < len(rows); i += chunk {
		end := min(i+chunk, len(rows))
		if _, err := pw.Write(rows[i:end]); err != nil {
			t.Fatal(err)
		}
		if err := pw.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertSequential(t *testing.T, rows []TraceSpan, want int) {
	t.Helper()
	if len(rows) != want {
		t.Fatalf("rows = %d, want %d", len(rows), want)
	}
	for i, r := range rows {
		if r.StartTime != int64(i) {
			t.Fatalf("row %d has StartTime %d: order or content lost", i, r.StartTime)
		}
	}
}

// TestCompactMergesIntoFewRowGroups is the regression test for the footer
// blow-up: a day of many small flush files must become one row group, not one
// per source file.
func TestCompactMergesIntoFewRowGroups(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")
	const files, per = 300, 3
	for i := 0; i < files; i++ {
		writeSourceFile(t, filepath.Join(dayDir, fmt.Sprintf("%05d.parquet", i)), spansFrom(i*per, per))
	}

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatal(err)
	}
	merged := filepath.Join(dayDir, mergedName)
	if got := rowGroupCount(t, merged); got != 1 {
		t.Errorf("row groups = %d, want 1", got)
	}
	assertSequential(t, readAll[TraceSpan](t, merged), files*per)
}

// TestCompactCapsRowGroupSize: a large day is cut into compactRowGroupRows
// groups instead of one unbounded group.
func TestCompactCapsRowGroupSize(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")
	total := 2*compactRowGroupRows + 5
	half := total / 2
	writeSourceFile(t, filepath.Join(dayDir, "a.parquet"), spansFrom(0, half))
	writeSourceFile(t, filepath.Join(dayDir, "b.parquet"), spansFrom(half, total-half))

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatal(err)
	}
	merged := filepath.Join(dayDir, mergedName)
	if got := rowGroupCount(t, merged); got != 3 {
		t.Errorf("row groups = %d, want 3", got)
	}
	assertSequential(t, readAll[TraceSpan](t, merged), total)
}

// TestCompactRewritesFragmentedDayFile: day.parquet files written by the old
// compactor are repaired in place, with every row kept in order.
func TestCompactRewritesFragmentedDayFile(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")
	merged := filepath.Join(dayDir, mergedName)
	writeFragmented(t, merged, spansFrom(0, 3000), 3) // 1000 groups of 3 rows
	if got := rowGroupCount(t, merged); got != 1000 {
		t.Fatalf("setup: row groups = %d, want 1000", got)
	}

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatal(err)
	}
	if got := rowGroupCount(t, merged); got != 1 {
		t.Errorf("row groups after rewrite = %d, want 1", got)
	}
	assertSequential(t, readAll[TraceSpan](t, merged), 3000)
	if files := parquetFilesSorted(t, dayDir); len(files) != 1 {
		t.Errorf("files after rewrite = %v, want only day.parquet", files)
	}
	if temps, _ := filepath.Glob(filepath.Join(dayDir, ".tmp-*")); len(temps) != 0 {
		t.Errorf("temp files left behind: %v", temps)
	}

	// Idempotent: a second pass leaves the repaired file alone.
	before, _ := os.Stat(merged)
	if err := w.compactOldDays(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(merged)
	if !after.ModTime().Equal(before.ModTime()) || os.SameFile(before, after) == false {
		t.Error("healthy day.parquet was rewritten again")
	}
}

// TestCompactLeavesHealthyMultiGroupFileAlone: large groups are not
// "fragmented", even when there are several of them.
func TestCompactLeavesHealthyMultiGroupFileAlone(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")
	merged := filepath.Join(dayDir, mergedName)
	writeFragmented(t, merged, spansFrom(0, 4*fragmentedRowsPerGroup), 2*fragmentedRowsPerGroup)

	before, _ := os.Stat(merged)
	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(merged)
	if !os.SameFile(before, after) {
		t.Error("a file with healthy row groups was rewritten")
	}
	if got := rowGroupCount(t, merged); got != 2 {
		t.Errorf("row groups = %d, want 2 (untouched)", got)
	}
}
