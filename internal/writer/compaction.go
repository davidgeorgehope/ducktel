package writer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"
)

// errStopping marks a pass that returned early because the writer is shutting
// down. It is not a failure: the temp file is removed and the sources are left
// for the next run, exactly as after a crash.
var errStopping = errors.New("writer stopping")

// maintenanceStepHook, when non-nil, runs between compaction merges. Tests use
// it to hold a pass open long enough to exercise shutdown and flush
// concurrency; production leaves it nil.
var maintenanceStepHook func()

// stepMaintenance calls the test hook, then checks for a shutdown. The check
// comes after the hook so a pass released after Stop still aborts before its
// next merge.
func stepMaintenance(ctx context.Context) error {
	if maintenanceStepHook != nil {
		maintenanceStepHook()
	}
	if ctx.Err() != nil {
		return errStopping
	}
	return nil
}

// mergedName is the single per-day file compaction produces. It deliberately
// does not match the writer's HH-MM[-n] naming pattern, and the compactor
// recognises its own output by this exact name.
const mergedName = "day.parquet"

// hourlyMargin is how far an hour's start must be behind now before
// compactHours may merge it. writeParquet picks the directory and the HH-MM
// name from the clock when a flush STARTS, and renames when it finishes, so a
// flush started at 10:59:59 can appear as 10-59.parquet after 11:00. Two hours
// of margin means no live write can still land in an eligible hour, even if a
// flush stalls for most of an hour. Like the day margin in compactOldDays,
// this keeps compaction from racing writes that run without w.mu.
const hourlyMargin = 2 * time.Hour

// hourFileName is the compactor's per-hour output: HH.parquet in the day
// directory. It is not a name writeParquet produces. The name also sorts
// chronologically with the per-flush files: "-" is 0x2d and "." is 0x2e, so
// 07-59.parquet < 07.parquet < 08-00.parquet. A day merge can therefore keep
// reading sources in name order and still see each hour's rows in order.
func hourFileName(hour int) string {
	return fmt.Sprintf("%02d.parquet", hour)
}

// isHourFileName reports whether base is an HH.parquet hour file.
func isHourFileName(base string) bool {
	if len(base) != len("00.parquet") || !strings.HasSuffix(base, ".parquet") {
		return false
	}
	if !isTwoDigits(base[:2]) {
		return false
	}
	hour, _ := strconv.Atoi(base[:2])
	return hour <= 23
}

// flushFileHour returns the hour encoded in a per-flush HH-MM[-n].parquet
// name, and false for anything else (hour files, day.parquet, dot files,
// retention leftovers, non-parquet files).
func flushFileHour(base string) (int, bool) {
	rest, ok := strings.CutSuffix(base, ".parquet")
	if !ok || len(rest) < len("00-00") || rest[2] != '-' {
		return 0, false
	}
	if !isTwoDigits(rest[:2]) || !isTwoDigits(rest[3:5]) {
		return 0, false
	}
	if len(rest) > len("00-00") {
		// The collision counter suffix: -n with one or more digits.
		if rest[5] != '-' || len(rest) == 6 {
			return 0, false
		}
		if _, err := strconv.Atoi(rest[6:]); err != nil {
			return 0, false
		}
	}
	hour, _ := strconv.Atoi(rest[:2])
	if hour > 23 {
		return 0, false
	}
	return hour, true
}

func isTwoDigits(s string) bool {
	return len(s) == 2 && s[0] >= '0' && s[0] <= '9' && s[1] >= '0' && s[1] <= '9'
}

// compactHours merges each CLOSED hour of today's and yesterday's directories
// into one HH.parquet per signal. Today and yesterday are the days people
// query most, and a day of per-flush files (thousands of tiny files) is slow
// to scan; one sorted file per closed hour cuts both the file count and the
// bytes read while leaving the still-open hours writable.
//
// now is passed in so tests control the clock. Only the current UTC date and
// the one before it are visited: older days belong to compactOldDays, which
// merges them into day.parquet.
//
// Crash safety mirrors compactDayDir: if HH.parquet already exists, the
// hour's merge completed and any remaining HH-MM[-n].parquet files are
// leftovers of a crash between rename and delete, so they are deleted and
// never merged (merging would double the rows). Otherwise a temp file is
// written, fsync'd and renamed onto HH.parquet before sources are removed; a
// crash before the rename leaves a .tmp-* file that sweepOrphanedTemps
// eventually removes.
func (w *Writer) compactHours(ctx context.Context, now time.Time) error {
	now = now.UTC()
	days := []string{
		now.Format("2006-01-02"),
		now.AddDate(0, 0, -1).Format("2006-01-02"),
	}

	var firstErr error
	for _, signal := range signalDirs {
		signalDir := filepath.Join(w.dataDir, signal)
		for _, day := range days {
			if ctx.Err() != nil {
				return errStopping
			}
			if err := compactHourDir(ctx, signalDir, day, signal, now); err != nil {
				if errors.Is(err, errStopping) {
					return errStopping
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("compacting hours of %s/%s: %w", signal, day, err)
				}
			}
		}
	}
	return firstErr
}

// compactHourDir merges the eligible hours of one day directory for one
// signal. The caller only reaches this for today and yesterday, and
// compactHourDir itself enforces the per-hour margin before touching a file.
func compactHourDir(ctx context.Context, signalDir, day, signal string, now time.Time) error {
	if ctx.Err() != nil {
		return errStopping
	}
	dayDir := filepath.Join(signalDir, day)
	dayStart, err := time.ParseInLocation("2006-01-02", day, time.UTC)
	if err != nil {
		return nil // not a date-shaped name; never touch it
	}

	entries, err := os.ReadDir(dayDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // this signal has no data for that day
		}
		return fmt.Errorf("reading %s: %w", dayDir, err)
	}
	if err := sweepOrphanedTemps(dayDir); err != nil {
		return err
	}

	// Group per-flush files by hour, and remember which hours already have
	// their merged file. A name that is neither an hour file nor a per-flush
	// file (day.parquet, dot files, anything else) is never touched.
	byHour := make(map[int][]string)
	mergedHours := make(map[int]bool)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if strings.HasPrefix(base, ".") || base == mergedName {
			continue
		}
		if isHourFileName(base) {
			hour, _ := strconv.Atoi(base[:2])
			mergedHours[hour] = true
			continue
		}
		hour, ok := flushFileHour(base)
		if !ok {
			continue
		}
		byHour[hour] = append(byHour[hour], filepath.Join(dayDir, base))
	}

	hours := make([]int, 0, len(byHour)+len(mergedHours))
	for hour := range byHour {
		hours = append(hours, hour)
	}
	for hour := range mergedHours {
		if _, ok := byHour[hour]; !ok {
			hours = append(hours, hour)
		}
	}
	sort.Ints(hours)

	var firstErr error
	for _, hour := range hours {
		if ctx.Err() != nil {
			return errStopping
		}
		// Eligible: the hour's start plus the margin is at or before now.
		hourStart := dayStart.Add(time.Duration(hour) * time.Hour)
		if hourStart.Add(hourlyMargin).After(now) {
			continue // the hour may still be receiving writes
		}

		sources := byHour[hour]
		sort.Strings(sources) // name order is chronological within the hour
		merged := filepath.Join(dayDir, hourFileName(hour))

		if mergedHours[hour] {
			// The previous merge's rename completed; leftovers are deleted,
			// never merged. See the crash discussion on compactHours.
			for _, f := range sources {
				if err := os.Remove(f); err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("removing leftover %s: %w", f, err)
					}
					continue
				}
				log.Printf("compaction: removed leftover %s", f)
			}
			continue
		}
		if len(sources) == 0 {
			continue
		}

		if err := writeMergedWith(ctx, dayDir, merged, func(tmp *os.File) error {
			return mergeHourSignal(ctx, tmp, signal, sources)
		}); err != nil {
			if errors.Is(err, errStopping) {
				return errStopping
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("merging hour %02d: %w", hour, err)
			}
			continue
		}
		// Sources are removed only after the merged file is in place. This loop
		// is not interrupted on shutdown: HH.parquet next to some of its sources
		// would count those rows twice until the next pass.
		for _, f := range sources {
			if err := os.Remove(f); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("removing original %s: %w", f, err)
				}
			}
		}
		log.Printf("compaction: merged %d file(s) into %s", len(sources), merged)
		if err := stepMaintenance(ctx); err != nil {
			return err
		}
	}
	return firstErr
}

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
func (w *Writer) compactOldDays(ctx context.Context) error {
	// Eligible: strictly older than yesterday. AddDate(-1) gives yesterday's
	// date, so `< cutoff` means today-2 and earlier.
	cutoff := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	var firstErr error
	for _, signal := range signalDirs {
		if ctx.Err() != nil {
			return errStopping
		}
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
			if ctx.Err() != nil {
				return errStopping
			}
			if !d.IsDir() {
				continue
			}
			if _, err := time.ParseInLocation("2006-01-02", d.Name(), time.UTC); err != nil {
				continue // not a date-shaped directory — never touch it
			}
			if d.Name() >= cutoff {
				continue // today or yesterday (or a clock-skewed future day)
			}
			if err := compactDayDir(ctx, signalDir, d.Name(), signal); err != nil {
				if errors.Is(err, errStopping) {
					return errStopping
				}
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
func compactDayDir(ctx context.Context, signalDir, day, signal string) error {
	if ctx.Err() != nil {
		return errStopping
	}
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
		if err := writeMerged(ctx, dayDir, merged, signal, []string{merged}); err != nil {
			return err
		}
		log.Printf("compaction: rewrote %s (%d row groups were too small)", merged, groups)
		return stepMaintenance(ctx)
	}
	if len(originals) == 0 {
		return nil
	}
	sort.Strings(originals) // chronological

	if err := writeMerged(ctx, dayDir, merged, signal, originals); err != nil {
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
	return stepMaintenance(ctx)
}

// mergeFunc writes a merged parquet body to tmp.
type mergeFunc func(tmp *os.File) error

// writeMerged merges sources into a temp file in dayDir and renames it onto
// merged. sources may include merged itself (a rewrite): it is read in full
// before the rename replaces it.
func writeMerged(ctx context.Context, dayDir, merged, signal string, sources []string) error {
	return writeMergedWith(ctx, dayDir, merged, func(tmp *os.File) error {
		return mergeDaySignal(ctx, tmp, signal, sources)
	})
}

// writeMergedWith owns the crash-safe temp-file protocol: write the body,
// fsync, close, then rename. Callers only supply the mergeFunc, which may stop
// returning errStopping. The context is checked before fsync and rename too, so
// a shutdown stops before the expensive commit steps when it can.
func writeMergedWith(ctx context.Context, dayDir, merged string, merge mergeFunc) error {
	tmp, err := os.CreateTemp(dayDir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := merge(tmp); err != nil {
		tmp.Close()
		return err
	}
	if ctx.Err() != nil {
		tmp.Close()
		return errStopping
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if ctx.Err() != nil {
		return errStopping
	}
	if err := os.Rename(tmpPath, merged); err != nil {
		return fmt.Errorf("committing %s: %w", merged, err)
	}
	return nil
}

// mergeDaySignal writes a day merge body for signal. Hour files are cut at
// their boundary so the per-hour sort survives into day.parquet; per-flush
// sources never cause extra cuts.
func mergeDaySignal(ctx context.Context, tmp *os.File, signal string, sources []string) error {
	switch signal {
	case "traces":
		return mergeIntoHourAware[TraceSpan](ctx, tmp, sources)
	case "logs":
		return mergeIntoHourAware[LogRecord](ctx, tmp, sources)
	case "metrics":
		return mergeIntoHourAware[MetricPoint](ctx, tmp, sources)
	default:
		return fmt.Errorf("unknown signal %q", signal)
	}
}

// mergeHourSignal writes one closed hour's merged body for signal, sorting
// rows when they fit the in-memory cap.
func mergeHourSignal(ctx context.Context, tmp *os.File, signal string, sources []string) error {
	switch signal {
	case "traces":
		return mergeHour[TraceSpan](ctx, tmp, sources, traceSpanLess)
	case "logs":
		return mergeHour[LogRecord](ctx, tmp, sources, logRecordLess)
	case "metrics":
		return mergeHour[MetricPoint](ctx, tmp, sources, metricPointLess)
	default:
		return fmt.Errorf("unknown signal %q", signal)
	}
}

// The sort keys match what the query layer filters and joins on: metric
// series, trace id, and log service. Ties keep source order via
// sort.SliceStable.
func metricPointLess(a, b MetricPoint) bool {
	if a.MetricName != b.MetricName {
		return a.MetricName < b.MetricName
	}
	return a.Timestamp < b.Timestamp
}

func traceSpanLess(a, b TraceSpan) bool {
	if a.TraceID != b.TraceID {
		return a.TraceID < b.TraceID
	}
	return a.StartTime < b.StartTime
}

func logRecordLess(a, b LogRecord) bool {
	if a.ServiceName != b.ServiceName {
		return a.ServiceName < b.ServiceName
	}
	return a.Timestamp < b.Timestamp
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
// compactor's memory (the receiver runs with a small memory limit). It is a
// var so tests can lower it instead of writing 50,000 rows.
var compactRowGroupRows = 50_000

// compactReadBatch is how many rows are read from a source at a time.
const compactReadBatch = 4096

// fragmentedRowsPerGroup: a day.parquet whose row groups average fewer rows
// than this was written by the old compactor, which cut a row group after every
// source file (~2880 groups of a few rows per day). Its footer was megabytes,
// and every query re-read every day's footer, so the query process ran out of
// memory on a tiny dataset. Such files are rewritten in place.
const fragmentedRowsPerGroup = 1000

// mergeWriterOptions are writerOptions plus the row-group cap every merge
// uses, so all compaction output gets zstd and bounded dictionaries too.
func mergeWriterOptions() []parquet.WriterOption {
	return append(writerOptions(), parquet.MaxRowsPerRowGroup(int64(compactRowGroupRows)))
}

// mergeInto appends every row of each source file into the writer. Row groups
// are cut at compactRowGroupRows and sources are streamed in batches, so memory
// stays bounded by one read batch plus one row group's buffer, whatever the
// size of the sources.
func mergeInto[T any](ctx context.Context, tmp *os.File, sources []string) error {
	pw := parquet.NewGenericWriter[T](tmp, mergeWriterOptions()...)
	buf := make([]T, compactReadBatch)
	for _, src := range sources {
		if err := copyRows(ctx, pw, src, buf); err != nil {
			pw.Close()
			return err
		}
	}
	return pw.Close()
}

// hourSortMaxRows caps the rows an hour merge holds in memory to sort them.
// Lower than compactRowGroupRows: a span or log row with large attributes can
// take a few KB, and the receiver runs with a small memory limit. A busier
// hour is merged unsorted, which still turns its files into one.
var hourSortMaxRows = 20_000

// mergeHour merges one closed hour's sources into tmp. Below the row cap the
// rows are read into memory and sorted by less: the resulting hour file is
// what makes the later day merge cheap to scan. Above the cap the sources
// stream in name order exactly like mergeInto, so memory stays bounded by one
// read batch plus one row group's buffer whatever the hour's size.
func mergeHour[T any](ctx context.Context, tmp *os.File, sources []string, less func(a, b T) bool) error {
	var total int64
	for _, src := range sources {
		if ctx.Err() != nil {
			return errStopping
		}
		n, err := sourceRowCount(src)
		if err != nil {
			return err
		}
		total += n
	}
	if ctx.Err() != nil {
		return errStopping
	}
	if total > int64(hourSortMaxRows) {
		return mergeInto[T](ctx, tmp, sources)
	}
	return mergeSorted[T](ctx, tmp, sources, less)
}

// sourceRowCount reads only a parquet footer. The compactor sums these before
// an hour merge so it can choose the sorted path without first reading the
// very rows the sort cap is meant to keep out of memory.
func sourceRowCount(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stating %s: %w", path, err)
	}
	pf, err := parquet.OpenFile(f, stat.Size(), parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true))
	if err != nil {
		return 0, fmt.Errorf("opening parquet %s: %w", path, err)
	}
	return pf.NumRows(), nil
}

// mergeSorted reads all sources, sorts the rows with less, and writes them to
// tmp. Callers check the total row count first, so this only runs when the
// rows fit the memory budget.
func mergeSorted[T any](ctx context.Context, tmp *os.File, sources []string, less func(a, b T) bool) error {
	var rows []T
	buf := make([]T, compactReadBatch)
	for _, src := range sources {
		if err := forEachSourceRow(ctx, src, buf, func(batch []T) error {
			rows = append(rows, batch...)
			return nil
		}); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return errStopping
	}
	sort.SliceStable(rows, func(i, j int) bool { return less(rows[i], rows[j]) })
	if ctx.Err() != nil {
		return errStopping
	}

	pw := parquet.NewGenericWriter[T](tmp, mergeWriterOptions()...)
	if _, err := pw.Write(rows); err != nil {
		pw.Close()
		return fmt.Errorf("writing sorted rows: %w", err)
	}
	if err := pw.Close(); err != nil {
		return fmt.Errorf("closing parquet writer: %w", err)
	}
	return nil
}

// mergeIntoHourAware is the day-merge writer. It preserves the per-hour sort
// in the merged file by cutting a row group at the end of each hour file once
// the group already holds fragmentedRowsPerGroup rows. Tiny hours (a day made
// of 10-row hours) therefore still end up in one large group, so isFragmented
// stays false and the file is not rewritten on every pass. Per-flush sources
// deliberately cause no cuts: the old compactor cut a group per source and
// produced ~2880 tiny groups per day, whose footers exhausted the query
// process. Group size is capped at compactRowGroupRows either way.
func mergeIntoHourAware[T any](ctx context.Context, tmp *os.File, sources []string) error {
	pw := parquet.NewGenericWriter[T](tmp, mergeWriterOptions()...)
	buf := make([]T, compactReadBatch)
	rowsInGroup := 0
	for _, src := range sources {
		if err := forEachSourceRow(ctx, src, buf, func(rows []T) error {
			return writeGroupRows(pw, rows, &rowsInGroup)
		}); err != nil {
			pw.Close()
			return err
		}
		if ctx.Err() != nil {
			pw.Close()
			return errStopping
		}
		if isHourFileName(filepath.Base(src)) && rowsInGroup >= fragmentedRowsPerGroup {
			if err := pw.Flush(); err != nil {
				pw.Close()
				return fmt.Errorf("cutting row group after %s: %w", src, err)
			}
			rowsInGroup = 0
		}
	}
	return pw.Close()
}

// writeGroupRows writes rows in chunks that never push the current row group
// past compactRowGroupRows. When the group is full it is ended with Flush, so
// the next chunk starts a new group.
func writeGroupRows[T any](pw *parquet.GenericWriter[T], rows []T, rowsInGroup *int) error {
	for len(rows) > 0 {
		room := compactRowGroupRows - *rowsInGroup
		if room <= 0 {
			if err := pw.Flush(); err != nil {
				return fmt.Errorf("cutting full row group: %w", err)
			}
			*rowsInGroup = 0
			continue
		}
		n := min(len(rows), room)
		if _, err := pw.Write(rows[:n]); err != nil {
			return fmt.Errorf("writing rows: %w", err)
		}
		*rowsInGroup += n
		rows = rows[n:]
	}
	return nil
}

// forEachSourceRow streams src in batches of len(buf) rows and calls fn for
// each non-empty batch. io.EOF is reported together with the final rows and is
// the success case, not an error.
func forEachSourceRow[T any](ctx context.Context, src string, buf []T, fn func([]T) error) error {
	if ctx.Err() != nil {
		return errStopping
	}
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
		// A large hour or day can need many batches; checking each one keeps
		// shutdown responsive without slowing the copy measurably.
		if ctx.Err() != nil {
			return errStopping
		}
		n, err := rd.Read(buf)
		if n > 0 {
			if ctx.Err() != nil {
				return errStopping
			}
			if ferr := fn(buf[:n]); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", src, err)
		}
	}
}

// copyRows streams one source file into pw in batches of len(buf) rows, so a
// large source (a whole day.parquet being rewritten) is never held in memory
// at once.
func copyRows[T any](ctx context.Context, pw *parquet.GenericWriter[T], src string, buf []T) error {
	return forEachSourceRow(ctx, src, buf, func(rows []T) error {
		if _, err := pw.Write(rows); err != nil {
			return fmt.Errorf("appending %s: %w", src, err)
		}
		return nil
	})
}
