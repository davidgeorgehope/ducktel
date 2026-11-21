package query

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/marcboeker/go-duckdb"
)

type ColumnInfo struct {
	Name string
	Type string
}

// views is the allowlist of queryable signal views.
var views = map[string]bool{
	"traces":  true,
	"logs":    true,
	"metrics": true,
}

func validView(name string) bool {
	return views[name]
}

type Engine struct {
	db      *sql.DB
	dataDir string
	// mu serializes the refresh-then-query sequence. Views are re-derived per
	// query so files written after Open stay visible; without serialization,
	// concurrent requests interleave CREATE OR REPLACE VIEW with queries and
	// with each other, and one losing that race used to swap the signal view
	// for the empty fallback — rows:[] at HTTP 200 for data that exists.
	mu sync.Mutex

	// builtFrom remembers the exact file list (path, size, mtime) each view
	// was last created from. Re-running CREATE OR REPLACE VIEW over hundreds
	// of files was a large share of every query; an unchanged list means the
	// existing view already reads the same files. Guarded by mu.
	builtFrom map[string][]fileStamp
	// viewBuilds counts executed CREATE OR REPLACE VIEW statements. It is
	// unexported and only read by tests, to prove an unchanged file set does
	// not rebuild.
	viewBuilds int
}

// fileStamp identifies one Parquet file as the view should see it. Size and
// mtime catch an in-place rewrite under the same name, which a path-only
// comparison would miss (day.parquet and HH.parquet are compacted in place).
type fileStamp struct {
	path  string
	size  int64
	mtime int64
}

// cgroupMemoryFiles are the paths read to detect a container memory limit —
// cgroup v2 first, v1 second. A var so tests can point it at fixture files.
var cgroupMemoryFiles = []string{
	"/sys/fs/cgroup/memory.max",                   // cgroup v2: "max" or bytes
	"/sys/fs/cgroup/memory/memory.limit_in_bytes", // cgroup v1: bytes
}

// CgroupMemoryLimit is cgroupMemoryLimit for other packages (serve sizes the
// Go heap limit from it).
func CgroupMemoryLimit() int64 { return cgroupMemoryLimit() }

// cgroupMemoryLimit returns the cgroup memory limit in bytes, or 0 when there
// is none (bare-metal dev machine, or the file says "max").
func cgroupMemoryLimit() int64 {
	for _, p := range cgroupMemoryFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		if n > 1<<60 {
			// cgroup v1's "unlimited" sentinel (≈ 2^63); not a real limit.
			continue
		}
		return n
	}
	return 0
}

// memoryLimitFraction is how much of the detected cgroup limit DuckDB's
// buffer manager may use. The rest stays free for the Go heap and page cache,
// so a wide query spills to DuckDB's temp directory instead of pushing the
// whole process over the limit and getting OOM-killed.
const memoryLimitFraction = 70

func Open(dataDir string) (*Engine, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("opening duckdb: %w", err)
	}

	// Every query that needs an order says ORDER BY itself, so DuckDB does not
	// have to keep the input row order through a blocking operator. That order
	// is a large share of the memory a wide aggregation uses; dropping it lets
	// the engine release batches as soon as they are consumed. See the ORDER BY
	// audit in internal/telemetry.
	if _, err := db.Exec("SET preserve_insertion_order = false;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("disabling duckdb preserve_insertion_order: %w", err)
	}

	if limit := cgroupMemoryLimit(); limit > 0 {
		// DuckDB sizes its buffer manager to ~80% of what it detects; inside
		// a cgroup-limited container that left almost nothing for the rest of
		// the process and wide scans OOM-killed the mcp container (measured:
		// a 7-day metric scan pinned 4Gi). Capping below the limit makes the
		// engine spill to disk rather than die. Division first, so an extreme
		// cgroup value cannot overflow int64.
		// DuckDB's memory_limit pragma wants a unit suffix; whole MiB, rounded
		// down so the cap can only undershoot, never overshoot.
		capped := limit / 100 * memoryLimitFraction / (1 << 20)
		if _, err := db.Exec(fmt.Sprintf("PRAGMA memory_limit='%dMiB';", capped)); err != nil {
			db.Close()
			return nil, fmt.Errorf("setting duckdb memory_limit: %w", err)
		}
	}

	e := &Engine{db: db, dataDir: dataDir, builtFrom: make(map[string][]fileStamp)}
	if err := e.CreateViews(); err != nil {
		db.Close()
		return nil, err
	}
	return e, nil
}

// CreateViews (re)derives the signal views from every Parquet file currently on
// disk. DuckDB resolves a read_parquet file list when the view is created, so
// views must be re-derived to observe files written afterwards. See refresh in
// Query: without it an engine opened before any data existed kept a placeholder
// `WHERE false` view and returned zero rows forever.
func (e *Engine) CreateViews() error {
	return e.createViews(time.Time{})
}

// createViews (re)derives the signal views from the Parquet files a query with
// the given lower bound can read. A zero since means every file; otherwise
// selectFiles drops day directories and early hours that cannot contain a row
// at or after since.
func (e *Engine) createViews(since time.Time) error {
	views := []struct {
		name      string
		signal    string
		emptyCols string
	}{
		{
			name:   "traces",
			signal: "traces",
			emptyCols: `'' as trace_id, '' as span_id, '' as parent_span_id,
				'' as trace_state,
				'' as service_name, '' as span_name, '' as span_kind,
				CAST(0 AS BIGINT) as start_time, CAST(0 AS BIGINT) as end_time,
				CAST(0.0 AS DOUBLE) as duration_ms,
				'' as status_code, '' as status_message, '' as attributes,
				'' as resource_attributes, '' as scope_name, '' as scope_version,
				'' as events, '' as links`,
		},
		{
			name:   "logs",
			signal: "logs",
			emptyCols: `CAST(0 AS BIGINT) as timestamp, CAST(0 AS BIGINT) as observed_timestamp,
				'' as trace_id, '' as span_id,
				CAST(0 AS INTEGER) as severity_number, '' as severity_text,
				'' as body, '' as attributes, '' as resource_attributes,
				'' as service_name, '' as scope_name, '' as scope_version,
				CAST(0 AS UINTEGER) as flags, '' as event_name`,
		},
		{
			name:   "metrics",
			signal: "metrics",
			emptyCols: `'' as metric_name, '' as metric_description, '' as metric_unit,
				'' as metric_type,
				CAST(0 AS BIGINT) as timestamp, CAST(0 AS BIGINT) as start_timestamp,
				CAST(0.0 AS DOUBLE) as value_double, CAST(0 AS BIGINT) as value_int,
				CAST(0 AS UBIGINT) as count, CAST(0.0 AS DOUBLE) as sum,
				CAST(0.0 AS DOUBLE) as min, CAST(0.0 AS DOUBLE) as max,
				'' as bucket_counts, '' as explicit_bounds, '' as quantile_values,
				'' as attributes, '' as resource_attributes,
				'' as service_name, '' as scope_name, '' as scope_version,
				'' as exemplars, CAST(0 AS UINTEGER) as flags,
				false as is_monotonic, '' as aggregation_temporality`,
		},
	}

	for _, v := range views {
		files, err := selectFiles(filepath.Join(e.dataDir, v.signal), since)
		if err != nil {
			return fmt.Errorf("selecting %s files: %w", v.name, err)
		}
		stamps, err := stampFiles(files)
		if err != nil {
			return fmt.Errorf("stamping %s files: %w", v.name, err)
		}
		if e.viewUnchanged(v.name, stamps) {
			continue
		}
		if len(stamps) == 0 {
			// No file can contain a matching row (or the signal has no data
			// yet), so the empty placeholder is the correct view.
			empty := fmt.Sprintf(`CREATE OR REPLACE VIEW %s AS SELECT %s WHERE false`, v.name, v.emptyCols)
			if _, err := e.db.Exec(empty); err != nil {
				return fmt.Errorf("creating empty %s view: %w", v.name, err)
			}
			e.rememberView(v.name, stamps)
			continue
		}

		// The placeholder is appended even when files exist, and pruned and
		// unbounded views both get it, so the view always exposes the full
		// column set. A pruned set can lack a column an older or newer file
		// has; without the placeholder a query on that column would fail
		// with "column not found" depending on which files were selected.
		q := fmt.Sprintf(
			`CREATE OR REPLACE VIEW %s AS SELECT * FROM read_parquet([%s], union_by_name=true) UNION ALL BY NAME SELECT %s WHERE false`,
			v.name, quotePaths(stampPaths(stamps)), v.emptyCols)
		if _, err := e.db.Exec(q); err != nil {
			// A failure while files exist must NOT be swallowed: silently
			// replacing a working view with the empty one made every later
			// query return a well-formed empty result for data that exists —
			// wrong answers indistinguishable from "no data" (observed live
			// as a trace lookup alternating rows:[] / rows:[span] while a
			// concurrent query was re-deriving the views).
			return fmt.Errorf("creating %s view from %d file(s): %w", v.name, len(stamps), err)
		}
		e.rememberView(v.name, stamps)
	}
	return nil
}

// stampFiles captures the identity of each selected file. A file that vanished
// between listing and stat cannot contribute rows any more, so it is dropped
// rather than failing the query: compaction deletes per-flush files from under
// a running engine.
func stampFiles(files []string) ([]fileStamp, error) {
	stamps := make([]fileStamp, 0, len(files))
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stating %s: %w", path, err)
		}
		stamps = append(stamps, fileStamp{path: path, size: info.Size(), mtime: info.ModTime().UnixNano()})
	}
	return stamps, nil
}

// stampPaths returns the files still present in stamps, in the same order.
func stampPaths(stamps []fileStamp) []string {
	paths := make([]string, len(stamps))
	for i, s := range stamps {
		paths[i] = s.path
	}
	return paths
}

// viewUnchanged reports whether name was last created from exactly stamps.
// The comparison is on the file list, not on since: two bounds that select the
// same files need the same view.
func (e *Engine) viewUnchanged(name string, stamps []fileStamp) bool {
	prev, ok := e.builtFrom[name]
	if !ok || len(prev) != len(stamps) {
		return false
	}
	for i := range prev {
		if prev[i] != stamps[i] {
			return false
		}
	}
	return true
}

// rememberView records the file list a view was just created from and counts
// the rebuild for tests.
func (e *Engine) rememberView(name string, stamps []fileStamp) {
	if e.builtFrom == nil {
		e.builtFrom = make(map[string][]fileStamp)
	}
	e.builtFrom[name] = stamps
	e.viewBuilds++
}

// quotePaths renders paths as a DuckDB list of SQL string literals. Paths are
// not identifiers and cannot be bound as placeholders in read_parquet's table
// argument, so only the string literal escaping applies: a single quote is
// doubled.
func quoteSQLString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func quotePaths(paths []string) string {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = quoteSQLString(p)
	}
	return strings.Join(quoted, ", ")
}

// selectFiles lists the Parquet files under signalDir that can hold a row at
// or after since. A zero since lists every parquet file, matching what the old
// `<signal>/**/*.parquet` glob read.
//
// A time-bounded query is served from a subset. Writer names encode the UTC
// hour a flush started in, and rows are buffered before that flush, so a row
// with timestamp >= since can be a little older than since in wall-clock terms.
// Sender clocks may also be ahead of ours. b = since - 1h is generous slack for
// both: a row at or after since lands in a file named at or after b.
//
// Under that bound: day directories before b's date are skipped; b's own date
// keeps day.parquet, its hour files and per-flush files at or after b's hour,
// and any unrecognised parquet name; later dates are kept whole. Dot files
// (.tmp-* in-flight writes) are never selected. Directories whose names are
// not dates are outside the compaction layout, so their files are always kept.
func selectFiles(signalDir string, since time.Time) ([]string, error) {
	var files []string

	var b, bDay time.Time
	if !since.IsZero() {
		b = since.UTC().Add(-time.Hour)
		bDay = time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	}

	err := filepath.WalkDir(signalDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if since.IsZero() || path == signalDir {
				return nil
			}
			rel, err := filepath.Rel(signalDir, path)
			if err != nil {
				return err
			}
			parts := strings.Split(rel, string(os.PathSeparator))
			day, err := time.ParseInLocation("2006-01-02", parts[0], time.UTC)
			if err == nil && day.Before(bDay) {
				// An old day directory: nothing in it can be selected, so do
				// not even list its files.
				return fs.SkipDir
			}
			return nil
		}
		base := d.Name()
		if strings.HasPrefix(base, ".") || !strings.HasSuffix(base, ".parquet") {
			return nil
		}
		if since.IsZero() {
			files = append(files, path)
			return nil
		}

		rel, err := filepath.Rel(signalDir, path)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		if len(parts) < 2 {
			// Directly in the signal directory: no date information, keep it.
			files = append(files, path)
			return nil
		}
		day, err := time.ParseInLocation("2006-01-02", parts[0], time.UTC)
		if err != nil {
			// Not a date-shaped directory: outside the retention/compaction
			// layout, so the glob would have included it. Keep it.
			files = append(files, path)
			return nil
		}
		switch {
		case day.Before(bDay):
			return nil
		case day.After(bDay):
			files = append(files, path)
			return nil
		}
		if len(parts) > 2 {
			// An unknown nested directory inside a date directory: keep it
			// rather than guess at a layout the writer never creates.
			files = append(files, path)
			return nil
		}
		if keepInDayDir(base, b.Hour()) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing parquet files under %s: %w", signalDir, err)
	}
	files, err = dropSuperseded(files)
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// dropSuperseded hides a per-flush or hour file once a newer merge of it
// exists. Compaction renames the merged file into place before it deletes
// the sources, and a crash in that gap leaves both. A reader that opens both
// counts those rows twice. A file strictly newer than the merge is not inside
// it (a flush that landed afterwards) and stays visible. Equal timestamps stay
// visible too: hiding one would drop rows when the clock cannot order them.
func dropSuperseded(files []string) ([]string, error) {
	byDir := make(map[string][]string)
	var dirs []string
	for _, f := range files {
		dir := filepath.Dir(f)
		if _, ok := byDir[dir]; !ok {
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], f)
	}

	out := make([]string, 0, len(files))
	for _, dir := range dirs {
		kept, err := dropSupersededDir(dir, byDir[dir])
		if err != nil {
			return nil, err
		}
		out = append(out, kept...)
	}
	return out, nil
}

func dropSupersededDir(dir string, files []string) ([]string, error) {
	if _, err := time.ParseInLocation("2006-01-02", filepath.Base(dir), time.UTC); err != nil {
		return files, nil
	}

	var dayPath string
	hourPath := make(map[int]string)
	for _, f := range files {
		base := filepath.Base(f)
		if base == "day.parquet" {
			dayPath = f
			continue
		}
		if hour, ok := hourFileHour(base); ok {
			hourPath[hour] = f
		}
	}
	if dayPath == "" && len(hourPath) == 0 {
		return files, nil
	}

	var dayTime time.Time
	var err error
	if dayPath != "" {
		dayTime, err = fileMtime(dayPath)
		if err != nil {
			return nil, err
		}
	}
	hourTime := make(map[int]time.Time, len(hourPath))
	for hour, path := range hourPath {
		ts, err := fileMtime(path)
		if err != nil {
			return nil, err
		}
		hourTime[hour] = ts
	}

	kept := make([]string, 0, len(files))
	for _, f := range files {
		base := filepath.Base(f)
		hide, err := superseded(base, f, dayPath, dayTime, hourPath, hourTime)
		if err != nil {
			return nil, err
		}
		if !hide {
			kept = append(kept, f)
		}
	}
	return kept, nil
}

func fileMtime(path string) (time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("stating %s: %w", path, err)
	}
	return info.ModTime(), nil
}

// superseded reports whether base is strictly older than a merge in the same
// directory. A zero dayTime or hourTime means that merge file is absent, and
// olderThan treats a zero time as do not hide.
func superseded(base, path, dayPath string, dayTime time.Time, hourPath map[int]string, hourTime map[int]time.Time) (bool, error) {
	if base == "day.parquet" || dayPath == path {
		return false, nil
	}
	if hour, ok := hourFileHour(base); ok && hourPath[hour] == path {
		return olderThan(path, dayTime)
	}
	hour, ok := flushFileHour(base)
	if !ok {
		return false, nil
	}
	if hourPath[hour] != "" {
		hide, err := olderThan(path, hourTime[hour])
		if err != nil || hide {
			return hide, err
		}
	}
	return olderThan(path, dayTime)
}

func olderThan(path string, merge time.Time) (bool, error) {
	if merge.IsZero() {
		return false, nil
	}
	ts, err := fileMtime(path)
	if err != nil {
		return false, err
	}
	if ts.IsZero() {
		return false, nil
	}
	return ts.Before(merge), nil
}

// keepInDayDir reports whether base, a file directly inside b's day
// directory, may hold a row at or after the bound hour. day.parquet is a merge
// of the whole day and unknown names are kept: a name this code does not
// recognise must never cause data to be dropped.
func keepInDayDir(base string, boundHour int) bool {
	if base == "day.parquet" {
		return true
	}
	if hour, ok := hourFileHour(base); ok {
		return hour >= boundHour
	}
	if hour, ok := flushFileHour(base); ok {
		return hour >= boundHour
	}
	return true
}

// hourFileHour returns the hour encoded in an HH.parquet merged-hour name.
func hourFileHour(base string) (int, bool) {
	if len(base) != len("00.parquet") || !strings.HasSuffix(base, ".parquet") || !isTwoDigits(base[:2]) {
		return 0, false
	}
	hour, _ := strconv.Atoi(base[:2])
	if hour > 23 {
		return 0, false
	}
	return hour, true
}

// flushFileHour returns the hour encoded in a per-flush HH-MM[-n].parquet
// name, and false for anything else (hour files, day.parquet, unknown names).
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

// Query executes SQL with optional bound parameters. Values passed in args are
// sent to DuckDB as placeholders (?) rather than interpolated into the SQL
// string, which prevents SQL injection from caller-supplied values. It is the
// unbounded form: every Parquet file is visible.
func (e *Engine) Query(sqlStr string, args ...interface{}) ([]map[string]interface{}, []string, error) {
	return e.query(time.Time{}, sqlStr, args...)
}

// QuerySince executes SQL like Query, but derives the signal views from only
// the Parquet files that can hold a row at or after since. An unbounded or
// zero since falls back to Query's behaviour. The caller's SQL still carries
// its own time predicate; this only narrows which files are opened.
func (e *Engine) QuerySince(since time.Time, sqlStr string, args ...any) ([]map[string]interface{}, []string, error) {
	return e.query(since, sqlStr, args...)
}

// query is the shared implementation of Query and QuerySince.
func (e *Engine) query(since time.Time, sqlStr string, args ...any) ([]map[string]interface{}, []string, error) {
	// The refresh and the query are one critical section: two requests
	// interleaving their CREATE OR REPLACE VIEW statements used to leave one
	// of them querying a view mid-replacement, and its lost race surfaced as
	// silent empty results. See the mu field.
	e.mu.Lock()
	defer e.mu.Unlock()

	// Re-derive the views so the query sees Parquet files written since the
	// engine was opened. Cheap: the file list is resolved at view creation.
	if err := e.createViews(since); err != nil {
		return nil, nil, fmt.Errorf("refreshing views: %w", err)
	}

	rows, err := e.db.Query(sqlStr, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("executing query: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// SerializeSQL returns DuckDB's JSON plan for statement. The text is bound
// as a value, so the driver does not execute it.
func (e *Engine) SerializeSQL(ctx context.Context, statement string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var raw string
	err := e.db.QueryRowContext(ctx,
		"SELECT CAST(json_serialize_sql(CAST(? AS VARCHAR)) AS VARCHAR)",
		statement).Scan(&raw)
	if err != nil {
		return "", err
	}
	return raw, nil
}

// QueryTenant runs statement against temp views that contain one tenant's
// rows. limit is the SQL LIMIT integer. The caller has already decided that
// statement is one read-only query. e.mu is held until the transaction rolls
// back, so the temp views and the read share one connection and no other
// query replaces the signal views mid-flight.
func (e *Engine) QueryTenant(ctx context.Context, tenant, statement string, limit int) ([]map[string]any, []string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := e.createViews(time.Time{}); err != nil {
		return nil, nil, fmt.Errorf("refreshing views: %w", err)
	}
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("opening connection: %w", err)
	}
	defer conn.Close()

	// A temp view named logs whose query also reads logs rebinds to itself.
	// Rename every signal view for this transaction, then point the temp view
	// at that name. traces is renamed too, so the statement has no unfiltered
	// relation. Rollback restores the signal views and drops the temp views.
	// The DROP names the temp schema, so a leftover cannot drop a signal view.
	dropTenantViews(context.Background(), conn)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() {
		tx.Rollback()
		dropTenantViews(context.Background(), conn)
	}()

	for _, name := range []struct{ from, to string }{
		{"traces", "_ducktel_traces"},
		{"logs", "_ducktel_logs"},
		{"metrics", "_ducktel_metrics"},
	} {
		q := fmt.Sprintf("ALTER VIEW %s RENAME TO %s", name.from, name.to)
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return nil, nil, fmt.Errorf("renaming %s: %w", name.from, err)
		}
	}

	pred := "json_extract_string(resource_attributes, '$.\"tenant.id\"') = " + quoteSQLString(tenant)
	tenantViews := []struct{ name, source string }{
		{"spans", "_ducktel_traces"},
		{"logs", "_ducktel_logs"},
		{"metrics", "_ducktel_metrics"},
	}
	for _, v := range tenantViews {
		q := fmt.Sprintf("CREATE TEMP VIEW %s AS SELECT * FROM %s WHERE %s", v.name, v.source, pred)
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return nil, nil, fmt.Errorf("creating tenant view %s: %w", v.name, err)
		}
	}

	// The closing paren is on its own line so a `--` comment cannot swallow it.
	wrapped := fmt.Sprintf("SELECT * FROM (\n%s\n) AS _q LIMIT %d", statement, limit)
	rows, err := tx.QueryContext(ctx, wrapped)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

func dropTenantViews(ctx context.Context, conn *sql.Conn) {
	for _, name := range []string{"temp.spans", "temp.logs", "temp.metrics"} {
		_, _ = conn.ExecContext(ctx, "DROP VIEW IF EXISTS "+name)
	}
}

func scanRows(rows *sql.Rows) ([]map[string]any, []string, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, fmt.Errorf("getting columns: %w", err)
	}

	var results []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		valuePtrs := make([]any, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, nil, fmt.Errorf("scanning row: %w", err)
		}
		row := make(map[string]any, len(columns))
		for i, col := range columns {
			val := values[i]
			if b, ok := val.([]byte); ok {
				val = string(b)
			}
			row[col] = val
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating rows: %w", err)
	}
	return results, columns, nil
}

// Describe returns column info for a known view. The view name is validated
// against an allowlist because identifiers cannot be bound as parameters.
func (e *Engine) Describe(view string) ([]ColumnInfo, error) {
	if !validView(view) {
		return nil, fmt.Errorf("unknown view %q (expected traces, logs, or metrics)", view)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.CreateViews(); err != nil {
		return nil, fmt.Errorf("refreshing views: %w", err)
	}
	rows, err := e.db.Query(fmt.Sprintf("DESCRIBE %s", view))
	if err != nil {
		return nil, fmt.Errorf("describing %s: %w", view, err)
	}
	defer rows.Close()

	var cols []ColumnInfo
	for rows.Next() {
		var name, typ string
		var null, key, def, extra sql.NullString
		if err := rows.Scan(&name, &typ, &null, &key, &def, &extra); err != nil {
			return nil, fmt.Errorf("scanning column info: %w", err)
		}
		cols = append(cols, ColumnInfo{Name: name, Type: typ})
	}
	return cols, rows.Err()
}

// Schema describes the traces view (kept for backward compatibility).
func (e *Engine) Schema() ([]ColumnInfo, error) {
	return e.Describe("traces")
}

func (e *Engine) Close() error {
	return e.db.Close()
}
