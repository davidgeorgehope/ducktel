package writer

import (
	"fmt"
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

	mu      sync.Mutex
	traces  []TraceSpan
	logs    []LogRecord
	metrics []MetricPoint

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

func (w *Writer) DataDir() string {
	return w.dataDir
}

func (w *Writer) Add(spans []TraceSpan) {
	w.mu.Lock()
	w.traces = append(w.traces, spans...)
	shouldFlush := len(w.traces) >= w.bufferSize
	w.mu.Unlock()

	if shouldFlush {
		w.Flush()
	}
}

func (w *Writer) AddLogs(records []LogRecord) {
	w.mu.Lock()
	w.logs = append(w.logs, records...)
	shouldFlush := len(w.logs) >= w.bufferSize
	w.mu.Unlock()

	if shouldFlush {
		w.Flush()
	}
}

func (w *Writer) AddMetrics(points []MetricPoint) {
	w.mu.Lock()
	w.metrics = append(w.metrics, points...)
	shouldFlush := len(w.metrics) >= w.bufferSize
	w.mu.Unlock()

	if shouldFlush {
		w.Flush()
	}
}

func (w *Writer) Flush() error {
	w.mu.Lock()
	traces := w.traces
	w.traces = nil
	logs := w.logs
	w.logs = nil
	metrics := w.metrics
	w.metrics = nil
	w.mu.Unlock()

	var firstErr error
	if len(traces) > 0 {
		if err := writeParquet(w.dataDir, "traces", traces); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if len(logs) > 0 {
		if err := writeParquet(w.dataDir, "logs", logs); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if len(metrics) > 0 {
		if err := writeParquet(w.dataDir, "metrics", metrics); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func writeParquet[T any](dataDir, signal string, records []T) error {
	now := time.Now().UTC()
	dir := filepath.Join(dataDir, signal, now.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s dir: %w", signal, err)
	}

	base := now.Format("15-04")
	path := filepath.Join(dir, base+".parquet")
	for i := 1; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.parquet", base, i))
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating parquet file: %w", err)
	}
	defer f.Close()

	pw := parquet.NewGenericWriter[T](f)
	if _, err := pw.Write(records); err != nil {
		return fmt.Errorf("writing %s: %w", signal, err)
	}
	if err := pw.Close(); err != nil {
		return fmt.Errorf("closing parquet writer: %w", err)
	}
	return nil
}

func (w *Writer) Start() {
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(w.flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.Flush()
			case <-w.stopCh:
				w.Flush()
				return
			}
		}
	}()
}

func (w *Writer) Stop() {
	close(w.stopCh)
	<-w.done
}
