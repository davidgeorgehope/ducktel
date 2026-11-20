package writer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func span(svc string) TraceSpan {
	now := time.Now()
	return TraceSpan{
		TraceID: "t", SpanID: "s", ServiceName: svc, SpanName: "op",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 1,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: "{}", Events: "[]", Links: "[]",
	}
}

func parquetFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

func TestFlushWritesParquetUnderSignalDir(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 1000)
	w.Add([]TraceSpan{span("a")})
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	files := parquetFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	rel, _ := filepath.Rel(dir, files[0])
	if !strings.HasPrefix(rel, "traces") || !strings.HasSuffix(rel, ".parquet") {
		t.Errorf("unexpected path layout: %s", rel)
	}
	// Date partitioning: traces/YYYY-MM-DD/HH-MM.parquet
	if parts := strings.Split(rel, string(filepath.Separator)); len(parts) != 3 {
		t.Errorf("expected traces/<date>/<file>, got %s", rel)
	}
}

// TestFlushEmptyIsNoop verifies flushing nothing neither writes a file nor errors.
func TestFlushEmptyIsNoop(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 1000)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if files := parquetFiles(t, dir); len(files) != 0 {
		t.Errorf("empty flush wrote %v", files)
	}
}

// TestBufferThresholdTriggersFlush verifies hitting bufferSize writes to disk
// without an explicit flush.
func TestBufferThresholdTriggersFlush(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 2) // threshold 2

	w.Add([]TraceSpan{span("a")})
	if files := parquetFiles(t, dir); len(files) != 0 {
		t.Errorf("flushed below threshold: %v", files)
	}

	w.Add([]TraceSpan{span("b")})
	if files := parquetFiles(t, dir); len(files) != 1 {
		t.Errorf("threshold not honoured, files=%v", files)
	}
}

// TestRepeatedFlushesGetDistinctFiles verifies the collision counter, so a
// second flush in the same minute does not overwrite the first.
func TestRepeatedFlushesGetDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 1000)
	for i := 0; i < 3; i++ {
		w.Add([]TraceSpan{span("a")})
		if err := w.Flush(); err != nil {
			t.Fatalf("flush %d: %v", i, err)
		}
	}
	if files := parquetFiles(t, dir); len(files) != 3 {
		t.Errorf("expected 3 distinct files, got %d: %v", len(files), files)
	}
}

func TestAllThreeSignals(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 1000)
	w.Add([]TraceSpan{span("a")})
	w.AddLogs([]LogRecord{{Timestamp: time.Now().UnixMicro(), ServiceName: "a", SeverityText: "INFO", Body: "b"}})
	w.AddMetrics([]MetricPoint{{MetricName: "m", MetricType: "gauge", ValueDouble: 1}})

	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if files := parquetFiles(t, dir); len(files) != 3 {
		t.Errorf("expected one file per signal, got %v", files)
	}
}

// TestStopFlushesRemaining verifies a shutdown flush persists buffered records.
func TestStopFlushesRemaining(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 1000)
	w.Start()
	w.Add([]TraceSpan{span("a")})
	w.Stop()
	if files := parquetFiles(t, dir); len(files) != 1 {
		t.Errorf("Stop did not flush: %v", files)
	}
}

// TestConcurrentFlushKeepsEverySpan: two flushes in the same minute must not
// pick one HH-MM name. writeParquet chooses the name without the buffer lock,
// and os.Rename replaces a file that is already there, so the first batch
// disappears while Flush still returns nil.
func TestConcurrentFlushKeepsEverySpan(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 1)
	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			s := span("svc")
			s.SpanID = fmt.Sprintf("s-%d", i)
			s.TraceID = s.SpanID
			w.Add([]TraceSpan{s})
		}(i)
	}
	wg.Wait()

	got := map[string]int{}
	for _, f := range parquetFiles(t, dir) {
		if strings.HasPrefix(filepath.Base(f), ".") {
			t.Errorf("temp file visible: %s", f)
			continue
		}
		for _, row := range readAll[TraceSpan](t, f) {
			got[row.SpanID]++
		}
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("s-%d", i)
		if got[id] != 1 {
			t.Errorf("span %s count = %d, want 1", id, got[id])
		}
	}
}

// TestSecondFlushWaitsForThePublish: a Flush that finds an empty buffer
// must not return while another Flush still holds that batch unpublished.
func TestSecondFlushWaitsForThePublish(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 1000)
	w.Add([]TraceSpan{span("held")})

	release := make(chan struct{})
	started := make(chan struct{})
	commitTestHook = func() {
		close(started)
		<-release
	}
	t.Cleanup(func() { commitTestHook = nil })

	firstDone := make(chan error, 1)
	go func() { firstDone <- w.Flush() }()
	<-started

	secondDone := make(chan error, 1)
	go func() { secondDone <- w.Flush() }()
	select {
	case err := <-secondDone:
		t.Fatalf("second Flush returned (%v) before the first publish", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Flush did not return")
	}
	rows := readAll[TraceSpan](t, soleTraceFile(t, dir))
	if len(rows) != 1 || rows[0].ServiceName != "held" {
		t.Fatalf("rows = %+v", rows)
	}
}

func soleTraceFile(t *testing.T, dir string) string {
	t.Helper()
	var found string
	for _, f := range parquetFiles(t, dir) {
		if strings.HasPrefix(filepath.Base(f), ".") {
			continue
		}
		found = f
	}
	if found == "" {
		t.Fatal("no parquet file")
	}
	return found
}

// TestConcurrentAddIsRaceFree exercises the mutex under -race.
func TestConcurrentAddIsRaceFree(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 1000)
	w.Start()
	defer w.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				w.Add([]TraceSpan{span("a")})
			}
		}()
	}
	wg.Wait()
}

// TestTempFilesAreNotLeftBehind verifies the atomic-write path cleans up.
func TestTempFilesAreNotLeftBehind(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 1000)
	w.Add([]TraceSpan{span("a")})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	for _, f := range parquetFiles(t, dir) {
		if strings.HasPrefix(filepath.Base(f), ".") {
			t.Errorf("temp file left behind: %s", f)
		}
	}
}

// TestTempNameIsNotGlobVisible guards a subtle failure: the query layer globs
// *.parquet, so an in-flight temp file must not match that pattern or readers
// can observe a partially written file.
func TestTempNameIsNotGlobVisible(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, time.Hour, 1000)
	w.Add([]TraceSpan{span("a")})

	// Capture the temp name the writer would use.
	tmp, err := os.CreateTemp(filepath.Join(dir), ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(tmp.Name())
	tmp.Close()
	os.Remove(tmp.Name())

	if strings.HasSuffix(name, ".parquet") {
		t.Errorf("temp file name %q would match the *.parquet query glob", name)
	}
}
