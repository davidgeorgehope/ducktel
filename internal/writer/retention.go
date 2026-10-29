package writer

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// signalDirs lists the top-level directories retention sweeps — the same
// three signals this package writes to.
var signalDirs = []string{"traces", "logs", "metrics"}

// ParseRetention parses a retention window, accepting Go's normal duration
// units (30m, 720h) plus a bare "Nd" days shorthand — retention windows are
// naturally day-scale, and time.ParseDuration has no day unit of its own. An
// empty string means "no retention" (0, disabled): files accumulate
// indefinitely, which is the safe default for an existing deployment that
// has not explicitly opted in.
func ParseRetention(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}

	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, fmt.Errorf("invalid retention %q: %w", s, err)
		}
		if days < 0 {
			return 0, fmt.Errorf("invalid retention %q: must not be negative", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid retention %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid retention %q: must not be negative", s)
	}
	return d, nil
}

// pruneOldData removes whole date-partition directories (traces|logs|metrics/
// YYYY-MM-DD) older than the retention window. Retention is day-granular by
// design, matching the storage layout: a directory holds exactly one day's
// files, so deleting it whole needs no per-file bookkeeping or compaction —
// "just deletes directories," per the project's own retention notes.
func (w *Writer) pruneOldData() error {
	w.mu.Lock()
	retention := w.retention
	w.mu.Unlock()
	if retention <= 0 {
		return nil
	}

	cutoff := time.Now().UTC().Add(-retention)

	var firstErr error
	for _, signal := range signalDirs {
		signalDir := filepath.Join(w.dataDir, signal)
		entries, err := os.ReadDir(signalDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // this signal has never received any data
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("reading %s: %w", signalDir, err)
			}
			continue
		}

		for _, e := range entries {
			if !e.IsDir() {
				continue // defensive: only date directories are ever expected here
			}
			day, err := time.ParseInLocation("2006-01-02", e.Name(), time.UTC)
			if err != nil {
				continue // not a date-shaped directory — skip, never touch it
			}
			if !day.Before(cutoff) {
				continue
			}

			path := filepath.Join(signalDir, e.Name())
			if err := os.RemoveAll(path); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("removing %s: %w", path, err)
				}
				continue
			}
			log.Printf("retention: removed %s (older than %s)", path, retention)
		}
	}
	return firstErr
}
