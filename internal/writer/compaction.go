package writer

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"
)

// mergedName is the single per-day file compaction produces. It deliberately
// does not match the writer's HH-MM[-n] naming pattern, and the compactor
// recognises its own output by this exact name.
const mergedName = "day.parquet"

// compactOldDays merges each old day's per-flush parquet files into one
// day.parquet per signal. Sources merge in name order, which is
// chronological (HH-MM[-n]), so the merged file's row-group statistics make
// timestamp filters prune well.
//
// Only days at least a full calendar day in the past are touched — strictly
// before yesterday (UTC). Yesterday itself is deliberately out of scope:
// writeParquet picks its directory from the wall clock when a flush STARTS,
// and flushes run from HTTP handlers and the buffer threshold as well as the
// writer's own loop, so a flush begun at 23:59:59 lands its file in
// yesterday's directory moments after midnight. Compacting on "any day that
// has ended" made that file a racing write — it appeared after the merge and
// the next pass deleted it as a stale leftover, silently losing a flush.
// The day-margin makes such a file at least a day old before its directory
// is eligible, so no live write can still be in flight. (A lock around flush
// writes was the alternative; writeParquet deliberately runs without w.mu so
// ingest never blocks behind disk I/O, and the margin preserves that.)
//
// Crash safety: if day.parquet already exists, the previous merge's rename
// completed and any remaining per-flush files are leftovers of a crash
// between rename and delete — they are deleted, never merged (merging them
// would double the rows). Otherwise the sources merge into a temp file that
// is fsync'd and renamed into place before the originals are removed; a
// crash before the rename leaves a stray .tmp-* file, swept once old enough
// by sweepOrphanedTemps.
func (w *Writer) compactOldDays() error {
	// Eligible: strictly older than yesterday. AddDate(-1) gives yesterday's
	// date, so `< cutoff` means today-2 and earlier.
	cutoff := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	var firstErr error
	for _, signal := range signalDirs {
		signalDir := filepath.Join(w.dataDir, signal)
		days, err := os.ReadDir(signalDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("reading %s: %w", signalDir, err)
			}
			continue
		}
		for _, d := range days {
			if !d.IsDir() {
				continue
			}
			if _, err := time.ParseInLocation("2006-01-02", d.Name(), time.UTC); err != nil {
				continue // not a date-shaped directory — never touch it
			}
			if d.Name() >= cutoff {
				continue // today or yesterday (or a clock-skewed future day)
			}
			if err := compactDayDir(signalDir, d.Name(), signal); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("compacting %s/%s: %w", signal, d.Name(), err)
				}
			}
		}
	}
	return firstErr
}

// tempSweepAge is how old a stray .tmp-* file must be before compaction
// removes it. A naming convention writeParquet also uses for its in-flight
// temp file, so the age guard leaves a genuinely live write alone even if the
// day-margin reasoning above were ever wrong.
const tempSweepAge = time.Hour

// sweepOrphanedTemps removes .tmp-* files older than tempSweepAge from dir.
// writeParquet's deferred os.Remove covers failures, but a crash between
// CreateTemp and rename — or between rename and the deferred Remove — leaves
// one behind, and with retention off nothing else would ever clean it up.
func sweepOrphanedTemps(dir string) error {
	temps, err := filepath.Glob(filepath.Join(dir, ".tmp-*"))
	if err != nil {
		return err
	}
	ageCutoff := time.Now().Add(-tempSweepAge)
	for _, tmp := range temps {
		info, err := os.Stat(tmp)
		if err != nil {
			if os.IsNotExist(err) {
				continue // already gone (raced a concurrent sweep)
			}
			return fmt.Errorf("stating %s: %w", tmp, err)
		}
		if info.ModTime().After(ageCutoff) {
			continue // young: a live write may still own it
		}
		if err := os.Remove(tmp); err != nil {
			return fmt.Errorf("removing orphaned temp %s: %w", tmp, err)
		}
		log.Printf("compaction: removed orphaned temp %s", tmp)
	}
	return nil
}

// compactDayDir merges one day directory's per-flush files for one signal.
// The caller only reaches this for days strictly older than yesterday, so no
// live write can still be landing here.
func compactDayDir(signalDir, day, signal string) error {
	dayDir := filepath.Join(signalDir, day)
	merged := filepath.Join(dayDir, mergedName)

	if err := sweepOrphanedTemps(dayDir); err != nil {
		return err
	}

	sources, err := filepath.Glob(filepath.Join(dayDir, "*.parquet"))
	if err != nil {
		return err
	}
	// Exclude the compactor's own output; the rest are per-flush originals.
	var originals []string
	for _, f := range sources {
		if filepath.Base(f) != mergedName {
			originals = append(originals, f)
		}
	}

	if _, err := os.Stat(merged); err == nil {
		// A previous merge completed; leftovers are deleted, never merged.
		removed := 0
		for _, f := range originals {
			if err := os.Remove(f); err != nil {
				return fmt.Errorf("removing leftover %s: %w", f, err)
			}
			removed++
		}
		if removed > 0 {
			log.Printf("compaction: removed %d leftover file(s) in %s", removed, dayDir)
		}
		fragmented, groups, err := isFragmented(merged)
		if err != nil {
			return err
		}
		if !fragmented {
			return nil
		}
		if err := writeMerged(dayDir, merged, signal, []string{merged}); err != nil {
			return err
		}
		log.Printf("compaction: rewrote %s (%d row groups were too small)", merged, groups)
		return nil
	}
	if len(originals) == 0 {
		return nil
	}
	sort.Strings(originals) // chronological

	if err := writeMerged(dayDir, merged, signal, originals); err != nil {
		return err
	}
	// Originals are removed only after the merged file is in place — see the
	// crash discussion on compactOldDays.
	for _, f := range originals {
		if err := os.Remove(f); err != nil {
			return fmt.Errorf("removing original %s: %w", f, err)
		}
	}
	log.Printf("compaction: merged %d file(s) into %s", len(originals), merged)
	return nil
}

// writeMerged merges sources into a temp file in dayDir and renames it onto
// merged. sources may include merged itself (a rewrite): it is read in full
// before the rename replaces it.
func writeMerged(dayDir, merged, signal string, sources []string) error {
	tmp, err := os.CreateTemp(dayDir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	switch signal {
	case "traces":
		err = mergeInto[TraceSpan](tmp, sources)
	case "logs":
		err = mergeInto[LogRecord](tmp, sources)
	case "metrics":
		err = mergeInto[MetricPoint](tmp, sources)
	default:
		err = fmt.Errorf("unknown signal %q", signal)
	}
	if err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, merged); err != nil {
		return fmt.Errorf("committing %s: %w", merged, err)
	}
	return nil
}

// isFragmented reports whether a merged file's row groups are too small on
// average (see fragmentedRowsPerGroup), along with its row group count.
func isFragmented(path string) (bool, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, 0, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return false, 0, err
	}
	pf, err := parquet.OpenFile(f, stat.Size(), parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true))
	if err != nil {
		return false, 0, fmt.Errorf("opening parquet %s: %w", path, err)
	}
	groups := len(pf.RowGroups())
	if groups <= 1 {
		return false, groups, nil
	}
	return pf.NumRows()/int64(groups) < fragmentedRowsPerGroup, groups, nil
}

// compactRowGroupRows caps a merged file's row groups. Readers pay per row
// group (footer metadata, statistics), so groups must be large; the writer
// buffers at most this many rows before cutting one, which bounds the
// compactor's memory (the receiver runs with a small memory limit).
const compactRowGroupRows = 50_000

// compactReadBatch is how many rows are read from a source at a time.
const compactReadBatch = 4096

// fragmentedRowsPerGroup: a day.parquet whose row groups average fewer rows
// than this was written by the old compactor, which cut a row group after every
// source file (~2880 groups of a few rows per day). Its footer was megabytes,
// and every query re-read every day's footer, so the query process ran out of
// memory on a tiny dataset. Such files are rewritten in place.
const fragmentedRowsPerGroup = 1000

// mergeInto appends every row of each source file into the writer. Row groups
// are cut at compactRowGroupRows and sources are streamed in batches, so memory
// stays bounded by one read batch plus one row group's buffer, whatever the
// size of the sources.
func mergeInto[T any](tmp *os.File, sources []string) error {
	pw := parquet.NewGenericWriter[T](tmp, parquet.MaxRowsPerRowGroup(compactRowGroupRows))
	buf := make([]T, compactReadBatch)
	for _, src := range sources {
		if err := copyRows(pw, src, buf); err != nil {
			pw.Close()
			return err
		}
	}
	return pw.Close()
}

// copyRows streams one source file into pw in batches of len(buf) rows, so a
// large source (a whole day.parquet being rewritten) is never held in memory
// at once.
func copyRows[T any](pw *parquet.GenericWriter[T], src string, buf []T) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stating %s: %w", src, err)
	}
	// The page index and bloom filters are only for selective reads; a full
	// copy never uses them, and on a fragmented file they are large.
	pf, err := parquet.OpenFile(f, stat.Size(), parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true))
	if err != nil {
		return fmt.Errorf("opening parquet %s: %w", src, err)
	}
	rd := parquet.NewGenericReader[T](pf)
	defer rd.Close()
	for {
		n, err := rd.Read(buf)
		if n > 0 {
			if _, werr := pw.Write(buf[:n]); werr != nil {
				return fmt.Errorf("appending %s: %w", src, werr)
			}
		}
		// parquet-go reports io.EOF together with the final rows; that is the
		// success case, not an error.
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", src, err)
		}
	}
}
