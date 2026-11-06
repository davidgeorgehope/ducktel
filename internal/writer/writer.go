package writer

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/parquet-go/parquet-go"
)

type Writer struct {
	dataDir       string
	flushInterval time.Duration
	bufferSize    int

	mu        sync.Mutex
	traces    []TraceSpan
	logs      []LogRecord
	metrics   []MetricPoint
	retention time.Duration // 0 disables; guarded by mu, see SetRetention

	// onError, if set, is called for flush failures that happen on the
	// background ticker or at shutdown. Those paths have no caller to return
	// an error to, so without this a failing writer would look healthy while
	// dropping data.
	onError func(error)

	stopCh chan struct{}
	done   chan struct{}
}

func New(dataDir string, flushInterval time.Duration, bufferSize int) *Writer {
	return &Writer{
		dataDir:       dataDir,
		flushInterval: flushInterval,
		bufferSize:    bufferSize,
		stopCh:        make(chan struct{}),
		done:          make(chan struct{}),
	}
}

// OnError registers a callback for flush errors raised on background paths.
func (w *Writer) OnError(fn func(error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onError = fn
}

// SetRetention enables a periodic sweep that deletes whole date-partition
// directories older than d. Zero (the default) disables it: data
// accumulates indefinitely unless this is called with a positive duration.
// Must be called before Start for the background ticker to pick it up.
func (w *Writer) SetRetention(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.retention = d
}

func (w *Writer) report(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	fn := w.onError
	w.mu.Unlock()
	if fn != nil {
		fn(err)
	}
}

func (w *Writer) DataDir() string {
	return w.dataDir
}

func (w *Writer) Add(spans []TraceSpan) {
	w.mu.Lock()
	w.traces = append(w.traces, spans...)
	shouldFlush := len(w.traces) >= w.bufferSize
	w.mu.Unlock()

	if shouldFlush {
		w.report(w.Flush())
	}
}

func (w *Writer) AddLogs(records []LogRecord) {
	w.mu.Lock()
	w.logs = append(w.logs, records...)
	shouldFlush := len(w.logs) >= w.bufferSize
	w.mu.Unlock()

	if shouldFlush {
		w.report(w.Flush())
	}
}

func (w *Writer) AddMetrics(points []MetricPoint) {
	w.mu.Lock()
	w.metrics = append(w.metrics, points...)
	shouldFlush := len(w.metrics) >= w.bufferSize
	w.mu.Unlock()

	if shouldFlush {
		w.report(w.Flush())
	}
}

// Flush writes all buffered records to disk, one file per signal.
//
// Records are taken off the buffer up front and put back if their write fails,
// so a transient error (full disk, permission blip) retries next time instead
// of discarding them. Successes are logged with counts and bytes so the
// process log answers "is it flushing, and how much?" without extra tooling.
func (w *Writer) Flush() error {
	w.mu.Lock()
	traces := w.traces
	logs := w.logs
	metrics := w.metrics
	w.traces, w.logs, w.metrics = nil, nil, nil
	w.mu.Unlock()

	if len(traces) == 0 && len(logs) == 0 && len(metrics) == 0 {
		// Nothing to do. Not logged: at a 30s interval this would be noise on
		// an idle instance, and the absence of flush lines is itself the signal.
		return nil
	}

	started := time.Now()
	var firstErr error

	// writeParquet is called WITHOUT holding w.mu: it does filesystem I/O and
	// must not block Add(). The requeue on failure re-acquires the lock — so
	// the requeue closure below must NOT take it again (sync.Mutex is not
	// reentrant; nesting the lock deadlocks).
	flushOne := func(signal string, n int, write func() (string, error), requeueInto func(int) int) {
		path, err := write()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			dropped := requeueInto(n)
			w.noteDropped(signal, dropped)
			log.Printf("flush: %s FAILED (%d record(s) requeued): %v", signal, n, err)
			return
		}
		log.Printf("flush: %s wrote %d record(s) to %s (%s)",
			signal, n, path, time.Since(started).Round(time.Millisecond))
	}

	if len(traces) > 0 {
		flushOne("traces", len(traces), func() (string, error) {
			return writeParquet(w.dataDir, "traces", traces)
		}, func(int) int {
			w.mu.Lock()
			defer w.mu.Unlock()
			return requeue(&w.traces, traces)
		})
	}
	if len(logs) > 0 {
		flushOne("logs", len(logs), func() (string, error) {
			return writeParquet(w.dataDir, "logs", logs)
		}, func(int) int {
			w.mu.Lock()
			defer w.mu.Unlock()
			return requeue(&w.logs, logs)
		})
	}
	if len(metrics) > 0 {
		flushOne("metrics", len(metrics), func() (string, error) {
			return writeParquet(w.dataDir, "metrics", metrics)
		}, func(int) int {
			w.mu.Lock()
			defer w.mu.Unlock()
			return requeue(&w.metrics, metrics)
		})
	}

	return firstErr
}

// noteDropped reports records discarded because the retry buffer was full.
// Dropping is deliberate backpressure, but it must never be silent.
func (w *Writer) noteDropped(signal string, dropped int) {
	if dropped > 0 {
		w.report(fmt.Errorf("dropped %d %s record(s): flush buffer full", dropped, signal))
	}
}

// maxBuffered bounds how many records a signal may hold while flushes keep
// failing. Retrying forever would grow the buffer without limit and OOM a
// long-running serve process, so past this point the oldest records are
// dropped and the loss is reported rather than hidden.
const maxBuffered = 500_000

// requeue puts failed records back at the head of a signal's buffer, trimming
// from the front once the buffer exceeds maxBuffered.
func requeue[T any](buf *[]T, records []T) (dropped int) {
	merged := append(records, *buf...)
	if len(merged) > maxBuffered {
		dropped = len(merged) - maxBuffered
		merged = merged[dropped:]
	}
	*buf = merged
	return dropped
}

// writeParquet serializes records to a new Parquet file. The data is written to
// a temporary file in the same directory, fsync'd, and only then renamed into
// place. Rename is atomic within a filesystem, so a crash or an error mid-write
// can never leave a partial .parquet for the query glob to read — a truncated
// file is silently reported as zero rows by DuckDB rather than raising, which
// would make the loss invisible.
func writeParquet[T any](dataDir, signal string, records []T) (string, error) {
	now := time.Now().UTC()
	dir := filepath.Join(dataDir, signal, now.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s dir: %w", signal, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if we bail before the rename succeeds.
	defer os.Remove(tmpPath)

	pw := parquet.NewGenericWriter[T](tmp)
	if _, err := pw.Write(records); err != nil {
		tmp.Close()
		return "", fmt.Errorf("writing %s: %w", signal, err)
	}
	if err := pw.Close(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("closing parquet writer: %w", err)
	}
	// Flush the file's contents before the rename makes it visible.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("syncing %s: %w", signal, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing %s: %w", signal, err)
	}

	path, err := nextPath(dir, now)
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("committing %s: %w", signal, err)
	}
	return path, nil
}

// nextPath picks the first unused HH-MM[-n].parquet name in dir.
func nextPath(dir string, now time.Time) (string, error) {
	base := now.Format("15-04")
	path := filepath.Join(dir, base+".parquet")
	for i := 1; ; i++ {
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			return path, nil
		}
		if err != nil {
			return "", fmt.Errorf("checking %s: %w", path, err)
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.parquet", base, i))
	}
}

// maintenanceInterval is fixed regardless of flush-interval: retention and
// compaction are both day-granular, so checking hourly is more than enough.
const maintenanceInterval = time.Hour

// maintenance runs the day-granular housekeeping: retention pruning (a no-op
// while retention is disabled) and compaction of past days' per-flush files.
// Both are crash-safe and cheap no-ops when there is nothing to do.
func (w *Writer) maintenance() {
	if err := w.pruneOldData(); err != nil {
		w.report(err)
	}
	if err := w.compactOldDays(); err != nil {
		w.report(err)
	}
}

func (w *Writer) Start() {
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(w.flushInterval)
		defer ticker.Stop()
		maintTicker := time.NewTicker(maintenanceInterval)
		defer maintTicker.Stop()

		// Run once at startup too, so a long-stopped process catches up on
		// backlog instead of waiting a full hour.
		w.maintenance()

		for {
			select {
			case <-ticker.C:
				w.report(w.Flush())
			case <-maintTicker.C:
				w.maintenance()
			case <-w.stopCh:
				w.report(w.Flush())
				return
			}
		}
	}()
}

// Stop flushes remaining buffered records and shuts down the background
// flusher. It returns the error from that final flush, if any.
func (w *Writer) Stop() error {
	close(w.stopCh)
	<-w.done
	// Flush once more: records requeued by a failed final flush are still
	// buffered, and the background goroutine has already exited.
	return w.Flush()
}
