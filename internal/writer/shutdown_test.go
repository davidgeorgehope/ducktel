package writer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// countParquetRows sums the row counts of every parquet file under root. Row
// counts are read from footers only, so a large backlog stays cheap to check.
func countParquetRows(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".parquet") {
			return nil
		}
		n, err := sourceRowCount(path)
		if err != nil {
			return err
		}
		total += n
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return total
		}
		t.Fatalf("counting rows under %s: %v", root, err)
	}
	return total
}

// tempFilesUnder returns the in-flight compaction files under root. None may
// survive a cancelled pass.
func tempFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasPrefix(info.Name(), ".tmp-") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for temp files: %v", root, err)
	}
	return out
}

// writeHourBacklog writes per-flush-style files for closed hours of day and
// returns how many rows were written.
func writeHourBacklog(t *testing.T, dataDir string, day time.Time, hours []int, filesPerHour, rowsPerFile int) int64 {
	t.Helper()
	dir := hourDayDir(t, dataDir, "traces", day)
	var rows int64
	for _, hour := range hours {
		for i := 0; i < filesPerHour; i++ {
			path := filepath.Join(dir, fmt.Sprintf("%02d-%02d.parquet", hour, i))
			writeSourceFile(t, path, spanN(rowsPerFile, "backlog"))
			rows += int64(rowsPerFile)
		}
	}
	return rows
}

// eligibleTodayHours lists the hours of now's UTC date that compactHours may
// merge: the hour's start must be at least hourlyMargin behind now.
func eligibleTodayHours(now time.Time) []int {
	now = now.UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	lastEligible := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.UTC).Add(-hourlyMargin)
	var hours []int
	for hour := 0; hour < 24; hour++ {
		if dayStart.Add(time.Duration(hour) * time.Hour).After(lastEligible) {
			break
		}
		hours = append(hours, hour)
	}
	return hours
}

// hourAndSourceFiles counts merged HH.parquet files and per-flush sources
// under root, which is how the tests tell an interrupted pass from a finished
// one.
func hourAndSourceFiles(t *testing.T, root string) (hourFiles, sourceFiles int) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		base := info.Name()
		if isHourFileName(base) {
			hourFiles++
			return nil
		}
		if _, ok := flushFileHour(base); ok {
			sourceFiles++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return hourFiles, sourceFiles
}

// blockMaintenanceAfterFirstMerge installs a hook that holds the first
// maintenance pass open after its first merge. The returned release function
// unblocks it and is safe to call more than once; the returned firstMerge
// channel closes once the pass is blocked. The caller must clear the hook with
// clearMaintenanceHook after all writer goroutines have stopped.
func blockMaintenanceAfterFirstMerge(t *testing.T) (firstMerge <-chan struct{}, release func()) {
	t.Helper()
	started := make(chan struct{})
	unblock := make(chan struct{})
	var hookOnce, releaseOnce sync.Once
	maintenanceStepHook = func() {
		hookOnce.Do(func() {
			close(started)
			<-unblock
		})
	}
	return started, func() {
		releaseOnce.Do(func() { close(unblock) })
	}
}

func clearMaintenanceHook() { maintenanceStepHook = nil }

// waitForStopCancellation waits until Stop has cancelled the writer context,
// which is what interrupts the blocked pass. It makes the shutdown tests
// independent of how long the scheduler takes to run the Stop goroutine.
func waitForStopCancellation(t *testing.T, w *Writer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for w.ctx.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("Stop did not cancel the maintenance context")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStopDuringMaintenance pins that SIGTERM does not wait for a whole
// compaction pass. The pass is held open after one merge so the interruption is
// deterministic; Stop must return promptly, leave the remaining hours' sources
// in place, and a later pass must still see every row exactly once.
func TestStopDuringMaintenance(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()

	hours := []int{0, 1, 2, 3}
	wantRows := writeHourBacklog(t, dataDir, now.AddDate(0, 0, -1), hours, 6, 3)

	todayHours := eligibleTodayHours(now)
	if len(todayHours) > 3 {
		todayHours = todayHours[:3]
	}
	wantRows += writeHourBacklog(t, dataDir, now, todayHours, 6, 3)
	eligibleHours := len(hours) + len(todayHours)

	firstMerge, release := blockMaintenanceAfterFirstMerge(t)
	defer release()
	defer clearMaintenanceHook()

	w := New(dataDir, time.Hour, 1000)
	w.Start()

	select {
	case <-firstMerge:
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance never merged an hour")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- w.Stop() }()

	waitForStopCancellation(t, w)
	// The pass is blocked after one merge, so Stop must still be waiting.
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before the blocked pass was released: %v", err)
	default:
	}

	release()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s: it waited for the whole compaction pass")
	}

	if temps := tempFilesUnder(t, dataDir); len(temps) > 0 {
		t.Errorf("cancelled pass left temp file(s): %v", temps)
	}
	hourFiles, sourceFiles := hourAndSourceFiles(t, filepath.Join(dataDir, "traces"))
	if sourceFiles == 0 {
		t.Error("no per-flush sources remain: the pass finished instead of being interrupted")
	}
	if hourFiles >= eligibleHours {
		t.Errorf("merged %d of %d eligible hours before Stop returned; the pass was not interrupted",
			hourFiles, eligibleHours)
	}
	if got := countParquetRows(t, filepath.Join(dataDir, "traces")); got != wantRows {
		t.Errorf("rows after interrupted pass = %d, want %d: a cancelled merge lost or duplicated rows",
			got, wantRows)
	}

	// A later pass (what a restarted process does) must finish the rest
	// without changing the row count. Use the same clock the backlog was
	// built against so a midnight crossing cannot move files out of scope.
	if err := w.compactHours(context.Background(), now); err != nil {
		t.Fatalf("finishing compactHours: %v", err)
	}
	if got := countParquetRows(t, filepath.Join(dataDir, "traces")); got != wantRows {
		t.Errorf("rows after the finishing pass = %d, want %d", got, wantRows)
	}
	if hourFiles, sourceFiles := hourAndSourceFiles(t, filepath.Join(dataDir, "traces")); sourceFiles != 0 || hourFiles != eligibleHours {
		t.Errorf("finishing pass left %d source file(s) and %d hour file(s), want 0 and %d",
			sourceFiles, hourFiles, eligibleHours)
	}
}

// TestFlushDuringMaintenance pins that maintenance no longer blocks the flush
// ticker: while a pass is held open, newly buffered records must still become
// a parquet file within about two flush intervals.
func TestFlushDuringMaintenance(t *testing.T) {
	dataDir := t.TempDir()
	yesterday := hourDayDir(t, dataDir, "traces", time.Now().UTC().AddDate(0, 0, -1))
	writeSourceFile(t, filepath.Join(yesterday, "00-00.parquet"), spanN(5, "backlog"))
	writeSourceFile(t, filepath.Join(yesterday, "00-01.parquet"), spanN(5, "backlog"))

	firstMerge, release := blockMaintenanceAfterFirstMerge(t)
	defer release()
	defer clearMaintenanceHook()

	const flushInterval = 20 * time.Millisecond
	w := New(dataDir, flushInterval, 1000)
	w.Start()

	select {
	case <-firstMerge:
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance never merged an hour")
	}

	w.Add([]TraceSpan{span("during-maintenance")})

	todayDir := filepath.Join(dataDir, "traces", time.Now().UTC().Format("2006-01-02"))
	deadline := time.Now().Add(2*flushInterval + 2*time.Second)
	var flushed string
	for time.Now().Before(deadline) {
		matches, err := filepath.Glob(filepath.Join(todayDir, "*.parquet"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) > 0 {
			flushed = matches[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if flushed == "" {
		t.Fatal("flush did not run while maintenance was blocked")
	}

	rows := readAll[TraceSpan](t, flushed)
	found := false
	for _, r := range rows {
		if r.ServiceName == "during-maintenance" {
			found = true
		}
	}
	if !found {
		t.Errorf("flushed file %s does not contain the record written during maintenance", flushed)
	}

	release()
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestStopFinalFlush pins that records buffered before Stop are on disk when
// Stop returns, even though Stop was called while maintenance was running.
func TestStopFinalFlush(t *testing.T) {
	dataDir := t.TempDir()
	yesterday := hourDayDir(t, dataDir, "traces", time.Now().UTC().AddDate(0, 0, -1))
	writeSourceFile(t, filepath.Join(yesterday, "00-00.parquet"), spanN(4, "backlog"))
	writeSourceFile(t, filepath.Join(yesterday, "00-01.parquet"), spanN(6, "backlog"))
	const backlogRows = int64(10)

	firstMerge, release := blockMaintenanceAfterFirstMerge(t)
	defer release()
	defer clearMaintenanceHook()

	// A long flush interval means only Stop's final flush can write these.
	w := New(dataDir, time.Hour, 1000)
	w.Start()

	select {
	case <-firstMerge:
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance never merged an hour")
	}

	w.Add([]TraceSpan{span("final-flush")})
	w.AddLogs([]LogRecord{{Timestamp: 1, ServiceName: "final-flush", Body: "x"}})
	w.AddMetrics([]MetricPoint{{MetricName: "final-flush", ValueDouble: 1}})

	stopDone := make(chan error, 1)
	go func() { stopDone <- w.Stop() }()

	waitForStopCancellation(t, w)
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before the blocked pass was released: %v", err)
	default:
	}

	release()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s")
	}

	if got := countParquetRows(t, filepath.Join(dataDir, "traces")); got != backlogRows+1 {
		t.Errorf("traces rows after Stop = %d, want %d (buffered record not flushed)", got, backlogRows+1)
	}
	if got := countParquetRows(t, filepath.Join(dataDir, "logs")); got != 1 {
		t.Errorf("logs rows after Stop = %d, want 1", got)
	}
	if got := countParquetRows(t, filepath.Join(dataDir, "metrics")); got != 1 {
		t.Errorf("metrics rows after Stop = %d, want 1", got)
	}
	if temps := tempFilesUnder(t, dataDir); len(temps) > 0 {
		t.Errorf("Stop left temp file(s): %v", temps)
	}
}
