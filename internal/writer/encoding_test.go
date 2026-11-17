package writer

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"
)

// openMetadata reads a parquet file's footer, which carries the per-column
// chunk metadata the tests below assert on (compression codec and encodings).
func openMetadata(t *testing.T, path string) *format.FileMetaData {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		t.Fatalf("stating %s: %v", path, err)
	}
	pf, err := parquet.OpenFile(f, stat.Size(), parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true))
	if err != nil {
		t.Fatalf("opening parquet %s: %v", path, err)
	}
	md := *pf.Metadata()
	return &md
}

// chunkMeta returns the column chunk metadata of every row group for the named
// top-level column. The schemas here are flat, so the column path is the field
// name alone.
func chunkMeta(t *testing.T, md *format.FileMetaData, column string) []format.ColumnMetaData {
	t.Helper()
	var out []format.ColumnMetaData
	for _, rg := range md.RowGroups {
		for _, c := range rg.Columns {
			if len(c.MetaData.PathInSchema) == 1 && c.MetaData.PathInSchema[0] == column {
				out = append(out, c.MetaData)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no column chunk named %q in file metadata", column)
	}
	return out
}

// isDictionaryEncoded reports whether a column chunk used dictionary pages.
func isDictionaryEncoded(c format.ColumnMetaData) bool {
	for _, e := range c.Encoding {
		if e == format.RLEDictionary || e == format.PlainDictionary {
			return true
		}
	}
	return false
}

// assertAllZstd checks that every column chunk of every row group is ZSTD
// compressed.
func assertAllZstd(t *testing.T, path string) {
	t.Helper()
	md := openMetadata(t, path)
	if len(md.RowGroups) == 0 {
		t.Fatalf("%s has no row groups", path)
	}
	for i, rg := range md.RowGroups {
		for _, c := range rg.Columns {
			if got := c.MetaData.Codec; got != format.Zstd {
				t.Errorf("%s row group %d column %v: codec = %s, want ZSTD",
					path, i, c.MetaData.PathInSchema, got)
			}
		}
	}
}

// TestWriteParquetUsesZstdCompression covers the per-flush writer: a file from
// writeParquet (here through Flush) must be zstd compressed in every column.
func TestWriteParquetUsesZstdCompression(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 1000)
	w.Add(spanN(100, "checkout"))
	w.AddLogs([]LogRecord{{Timestamp: 1, ServiceName: "checkout", Body: "hello"}})
	w.AddMetrics([]MetricPoint{{MetricName: "requests", MetricType: "sum", ValueDouble: 1}})
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	files := parquetFiles(t, dir)
	if len(files) != 3 {
		t.Fatalf("expected one file per signal, got %d: %v", len(files), files)
	}
	for _, f := range files {
		assertAllZstd(t, f)
	}
}

// TestCompactionUsesZstdCompression covers every compaction writer: both the
// hour merge (mergeSorted/mergeInto) and the day merge (mergeIntoHourAware)
// must produce zstd compressed column chunks.
func TestCompactionUsesZstdCompression(t *testing.T) {
	// Separate data dirs: an old day dir would also be picked up by the day
	// compactor, so the hour merge gets its own tree.
	dayDataDir := t.TempDir()
	dayDir := oldDayDir(t, dayDataDir, "traces")
	writeSourceFile(t, filepath.Join(dayDir, "10-00.parquet"), spanN(5, "a"))
	writeSourceFile(t, filepath.Join(dayDir, "10-01.parquet"), spanN(5, "b"))

	hourDataDir := t.TempDir()
	hourDir := hourDayDir(t, hourDataDir, "metrics", hourTestNow)
	writeSourceFile(t, filepath.Join(hourDir, "09-00.parquet"), []MetricPoint{
		{MetricName: "b", Timestamp: 2},
		{MetricName: "a", Timestamp: 1},
	})

	if err := New(dayDataDir, time.Hour, 1000).compactOldDays(); err != nil {
		t.Fatalf("compactOldDays: %v", err)
	}
	if err := New(hourDataDir, time.Hour, 1000).compactHours(hourTestNow); err != nil {
		t.Fatalf("compactHours: %v", err)
	}

	assertAllZstd(t, filepath.Join(dayDir, mergedName))
	assertAllZstd(t, filepath.Join(hourDir, "09.parquet"))
}

// TestDictionaryEncodingPerColumn pins the dict tags: repeated columns
// (service_name, attributes) are dictionary encoded, near-unique ones
// (trace_id, exemplars) are not.
func TestDictionaryEncodingPerColumn(t *testing.T) {
	dir := t.TempDir()

	spans := make([]TraceSpan, 200)
	for i := range spans {
		spans[i] = span("checkout")
		spans[i].TraceID = fmt.Sprintf("trace-%04d", i)
	}
	tracesPath, err := writeParquet(dir, "traces", spans)
	if err != nil {
		t.Fatalf("writeParquet traces: %v", err)
	}
	md := openMetadata(t, tracesPath)
	for _, c := range chunkMeta(t, md, "service_name") {
		if !isDictionaryEncoded(c) {
			t.Errorf("service_name encodings = %v, want dictionary encoding", c.Encoding)
		}
	}
	for _, c := range chunkMeta(t, md, "trace_id") {
		if isDictionaryEncoded(c) {
			t.Errorf("trace_id encodings = %v, want no dictionary encoding", c.Encoding)
		}
	}

	points := make([]MetricPoint, 200)
	for i := range points {
		points[i] = MetricPoint{
			MetricName: "requests",
			Attributes: fmt.Sprintf(`{"route":"/v1/items","status":200,"n":%d}`, i),
			Exemplars:  fmt.Sprintf(`{"trace_id":"trace-%04d"}`, i),
			Timestamp:  int64(i),
		}
	}
	metricsPath, err := writeParquet(dir, "metrics", points)
	if err != nil {
		t.Fatalf("writeParquet metrics: %v", err)
	}
	md = openMetadata(t, metricsPath)
	for _, c := range chunkMeta(t, md, "attributes") {
		if !isDictionaryEncoded(c) {
			t.Errorf("attributes encodings = %v, want dictionary encoding", c.Encoding)
		}
	}
	for _, c := range chunkMeta(t, md, "exemplars") {
		if isDictionaryEncoded(c) {
			t.Errorf("exemplars encodings = %v, want no dictionary encoding", c.Encoding)
		}
	}
}

// TestRoundTripAllSignals writes every signal through writeParquet and reads it
// back: compression and dictionary encoding must not change a single value.
func TestRoundTripAllSignals(t *testing.T) {
	dir := t.TempDir()

	traces := []TraceSpan{
		{
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7",
			ParentSpanID: "0000000000000000", TraceState: "vendor=1",
			ServiceName: "checkout", SpanName: "POST /cart", SpanKind: "SPAN_KIND_SERVER",
			StartTime: 1700000000000000, EndTime: 1700000000001500, DurationMs: 1.5,
			StatusCode: "STATUS_CODE_OK", StatusMessage: "",
			Attributes: `{"http.status":200,"route":"/cart"}`, ResourceAttributes: `{"service.version":"1.2.3"}`,
			ScopeName: "otelhttp", ScopeVersion: "0.1.0", Events: "[]", Links: "[]",
		},
		{
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b8",
			ParentSpanID: "00f067aa0ba902b7", TraceState: "",
			ServiceName: "payments", SpanName: "charge", SpanKind: "SPAN_KIND_CLIENT",
			StartTime: 1700000000000500, EndTime: 1700000000001400, DurationMs: 0.9,
			StatusCode: "STATUS_CODE_ERROR", StatusMessage: "card declined",
			Attributes: `{"error":true}`, ResourceAttributes: `{"service.version":"1.2.3"}`,
			ScopeName: "otelhttp", ScopeVersion: "0.1.0",
			Events: `[{"name":"retry"}]`, Links: "[]",
		},
	}
	tracesPath, err := writeParquet(dir, "traces", traces)
	if err != nil {
		t.Fatalf("writeParquet traces: %v", err)
	}
	if got := readAll[TraceSpan](t, tracesPath); !reflect.DeepEqual(got, traces) {
		t.Errorf("traces round trip mismatch:\n got %+v\nwant %+v", got, traces)
	}

	logs := []LogRecord{
		{
			Timestamp: 1700000000000000, ObservedTimestamp: 1700000000000100,
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7",
			SeverityNumber: 9, SeverityText: "INFO", Body: "cart created",
			Attributes: `{"cart.id":"c-1"}`, ResourceAttributes: `{"service.version":"1.2.3"}`,
			ServiceName: "checkout", ScopeName: "logger", ScopeVersion: "0.1.0",
			Flags: 1, EventName: "cart.created",
		},
		{
			Timestamp: 1700000000001000, ObservedTimestamp: 1700000000001100,
			SeverityNumber: 17, SeverityText: "ERROR", Body: "payment failed",
			Attributes: `{"error":"declined"}`, ServiceName: "payments",
			ScopeName: "logger", ScopeVersion: "0.1.0", EventName: "",
		},
	}
	logsPath, err := writeParquet(dir, "logs", logs)
	if err != nil {
		t.Fatalf("writeParquet logs: %v", err)
	}
	if got := readAll[LogRecord](t, logsPath); !reflect.DeepEqual(got, logs) {
		t.Errorf("logs round trip mismatch:\n got %+v\nwant %+v", got, logs)
	}

	metrics := []MetricPoint{
		{
			MetricName: "http.server.duration", MetricDescription: "request duration",
			MetricUnit: "ms", MetricType: "histogram",
			Timestamp: 1700000000000000, StartTimestamp: 1699999999000000,
			ValueDouble: 12.5, ValueInt: 0, Count: 42, Sum: 525.5, Min: 1, Max: 30,
			BucketCounts: "[1,2,3]", ExplicitBounds: "[5,10,25]",
			QuantileValues: "[{\"q\":0.5,\"v\":12}]",
			Attributes:     `{"route":"/cart"}`, ResourceAttributes: `{"service.version":"1.2.3"}`,
			ServiceName: "checkout", ScopeName: "otelhttp", ScopeVersion: "0.1.0",
			Exemplars: `[{"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}]`,
			Flags:     0, IsMonotonic: true, AggregationTemporality: "AGGREGATION_TEMPORALITY_CUMULATIVE",
		},
		{
			MetricName: "queue.depth", MetricDescription: "", MetricUnit: "1",
			MetricType: "gauge", Timestamp: 1700000000002000,
			ValueDouble: 0, ValueInt: 7, Attributes: "{}", ResourceAttributes: "{}",
			ServiceName: "worker", AggregationTemporality: "AGGREGATION_TEMPORALITY_UNSPECIFIED",
		},
	}
	metricsPath, err := writeParquet(dir, "metrics", metrics)
	if err != nil {
		t.Fatalf("writeParquet metrics: %v", err)
	}
	if got := readAll[MetricPoint](t, metricsPath); !reflect.DeepEqual(got, metrics) {
		t.Errorf("metrics round trip mismatch:\n got %+v\nwant %+v", got, metrics)
	}
}

// writeOldStyleFile writes rows the way the writer did before this change:
// default options, so PLAIN and uncompressed.
func writeOldStyleFile[T any](t *testing.T, path string, rows []T) {
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

// writeNewStyleFile writes rows with the options writeParquet and compaction
// now use, at a caller-chosen path.
func writeNewStyleFile[T any](t *testing.T, path string, rows []T) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	pw := parquet.NewGenericWriter[T](f, writerOptions()...)
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

// TestCompactMergesOldAndNewStyleFiles: files on disk written before this
// change (PLAIN, uncompressed) must still merge together with new ones into a
// single file holding every row, and the merged output must be the new style.
func TestCompactMergesOldAndNewStyleFiles(t *testing.T) {
	dataDir := t.TempDir()
	dayDir := oldDayDir(t, dataDir, "traces")

	oldRows := spansFrom(0, 3)
	for i := range oldRows {
		oldRows[i].ServiceName = "legacy"
	}
	newRows := spansFrom(3, 4)
	for i := range newRows {
		newRows[i].ServiceName = "modern"
	}
	oldPath := filepath.Join(dayDir, "10-00.parquet")
	newPath := filepath.Join(dayDir, "10-01.parquet")
	writeOldStyleFile(t, oldPath, oldRows)
	writeNewStyleFile(t, newPath, newRows)

	// Sanity check the two styles really differ: the old file is PLAIN and
	// uncompressed, so the merge test is not accidentally comparing likes.
	if md := openMetadata(t, oldPath); md.RowGroups[0].Columns[0].MetaData.Codec == format.Zstd {
		t.Fatal("setup: old-style file unexpectedly zstd compressed")
	}
	assertAllZstd(t, newPath)

	w := New(dataDir, time.Hour, 1000)
	if err := w.compactOldDays(); err != nil {
		t.Fatalf("compactOldDays: %v", err)
	}

	files := parquetFilesSorted(t, dayDir)
	if len(files) != 1 || files[0] != mergedName {
		t.Fatalf("files after compaction = %v, want [%s]", files, mergedName)
	}
	merged := filepath.Join(dayDir, mergedName)
	rows := readAll[TraceSpan](t, merged)
	assertSequential(t, rows, len(oldRows)+len(newRows))

	services := map[string]int{}
	for _, r := range rows {
		services[r.ServiceName]++
	}
	if services["legacy"] != len(oldRows) || services["modern"] != len(newRows) {
		t.Errorf("merged services = %v, want %d legacy and %d modern rows",
			services, len(oldRows), len(newRows))
	}
	assertAllZstd(t, merged)
}

// TestDictionaryMaxBytesFallsBackToPlain pins the DictionaryMaxBytes guard: a
// column whose dictionary exceeds 1 MiB within a row group switches to PLAIN
// pages for the rest of that group instead of growing without bound, while a
// low-cardinality column in the same file stays dictionary encoded.
func TestDictionaryMaxBytesFallsBackToPlain(t *testing.T) {
	// Enough unique, page-sized values per row group to push the dictionary
	// past 1 MiB, and more rows than one column buffer holds so that a page is
	// written (and the limit checked) before the group ends.
	const rows = 40_000
	points := make([]MetricPoint, rows)
	for i := range points {
		// Unique values that do not compress to a tiny dictionary entry.
		points[i] = MetricPoint{
			MetricName: "gauge",
			Attributes: fmt.Sprintf("%096d", i),
			Timestamp:  int64(i),
		}
	}

	path, err := writeParquet(t.TempDir(), "metrics", points)
	if err != nil {
		t.Fatalf("writeParquet: %v", err)
	}

	md := openMetadata(t, path)
	sawPlain := false
	for _, c := range chunkMeta(t, md, "attributes") {
		for _, e := range c.Encoding {
			if e == format.Plain {
				sawPlain = true
			}
		}
	}
	if !sawPlain {
		t.Error("attributes column never fell back to PLAIN: DictionaryMaxBytes is not taking effect")
	}
	for _, c := range chunkMeta(t, md, "metric_name") {
		if !isDictionaryEncoded(c) {
			t.Errorf("metric_name encodings = %v, want dictionary encoding (low cardinality must keep its dictionary)", c.Encoding)
		}
	}
	if got := readAll[MetricPoint](t, path); !reflect.DeepEqual(got, points) {
		t.Errorf("round trip mismatch after dictionary fallback (%d rows in, %d out)", rows, len(got))
	}
}
