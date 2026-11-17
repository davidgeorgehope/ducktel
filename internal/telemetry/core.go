package telemetry

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
)

// Core runs ducktel's generic, tenant-scoped telemetry queries. It is the one
// implementation of "what is a valid tenant, what does a span search's WHERE
// clause look like, what aggregations are allowed" — shared by every consumer
// so a fix or a tightened check cannot drift between two copies.
type Core struct {
	engine *query.Engine
	// tenantAttr is the resource attribute holding the tenant id.
	tenantAttr string

	// cache holds recent List* discovery results; see discover.
	cacheMu sync.Mutex
	cache   map[string]cachedResult
}

// NewCore wires a query engine into a Core. An engine of nil is valid for
// tests that only exercise validation, not queries.
func NewCore(engine *query.Engine) *Core {
	return &Core{engine: engine, tenantAttr: "tenant.id"}
}

// AttrFilter is one key/value attribute match, ANDed with any others.
type AttrFilter struct {
	Key   string
	Value string
}

// errInvalidInput classifies an error as a caller input problem (missing or
// malformed parameter) rather than a query engine failure, so an HTTP layer
// can map it to 400 instead of 500. Check with IsInvalidInput; the sentinel
// itself is unexported so its text never leaks into a returned error message.
var errInvalidInput = errors.New("invalid input")

type inputError struct{ msg string }

func (e *inputError) Error() string { return e.msg }
func (e *inputError) Unwrap() error { return errInvalidInput }
func invalidInput(format string, a ...any) error {
	return &inputError{msg: fmt.Sprintf(format, a...)}
}

// IsInvalidInput reports whether err represents a caller input problem, as
// opposed to a query engine failure.
func IsInvalidInput(err error) bool {
	return errors.Is(err, errInvalidInput)
}

func validateTenant(tenant string) error {
	if strings.TrimSpace(tenant) == "" {
		return invalidInput("tenant is required: every query is scoped to one tenant")
	}
	return nil
}

// tenantPredicate returns the mandatory isolation predicate. It is appended to
// every query as a non-optional condition.
func (c *Core) tenantPredicate() string {
	return fmt.Sprintf("json_extract_string(resource_attributes, '%s') = ?",
		strings.ReplaceAll(jsonPath(c.tenantAttr), "'", "''"))
}

// aggregationExpr maps a caller-supplied aggregation name to SQL. The value is
// chosen from a fixed set, never interpolated from input.
func aggregationExpr(name string) (string, error) {
	switch strings.ToLower(name) {
	case "avg", "mean":
		return "avg(value_double)", nil
	case "sum":
		return "sum(value_double)", nil
	case "min":
		return "min(value_double)", nil
	case "max":
		return "max(value_double)", nil
	case "count":
		return "count(*)", nil
	case "p50":
		return "approx_quantile(value_double, 0.5)", nil
	case "p95":
		return "approx_quantile(value_double, 0.95)", nil
	case "p99":
		return "approx_quantile(value_double, 0.99)", nil
	case "buckets":
		return "", invalidInput("aggregation buckets needs an explicit-bucket histogram; this metric is not one")
	}
	return "", invalidInput("aggregation %q is not supported; use one of: avg, sum, min, max, count, p50, p95, p99", name)
}

// LookupTrace retrieves every span belonging to one trace, ordered by start
// time, scoped to tenant.
func (c *Core) LookupTrace(traceID, tenant string, limit int) ([]map[string]any, []string, error) {
	if traceID == "" {
		return nil, nil, invalidInput("trace_id is required")
	}
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}
	if limit <= 0 {
		limit = 1000
	}

	q := `SELECT trace_id, span_id, parent_span_id, service_name, span_name, span_kind,
	             start_time, end_time, duration_ms, status_code, status_message, attributes
	      FROM traces
	      WHERE trace_id = ?
	        AND ` + c.tenantPredicate() + `
	      ORDER BY start_time
	      LIMIT ?`

	return c.engine.Query(q, traceID, tenant, limit)
}

// SearchSpans finds spans matching attribute filters within a time range,
// scoped to tenant.
func (c *Core) SearchSpans(tenant, serviceName string, filters []AttrFilter, sinceMinutes, limit int) ([]map[string]any, []string, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}

	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	since := sinceMinutes
	if since <= 0 {
		since = 60
	}
	cutoff := time.Now().Add(-time.Duration(since) * time.Minute).UnixMicro()

	conds := []string{"start_time >= ?", c.tenantPredicate()}
	params := []any{cutoff, tenant}

	if serviceName != "" {
		conds = append(conds, "service_name = ?")
		params = append(params, serviceName)
	}
	for _, f := range filters {
		if f.Key == "" {
			return nil, nil, invalidInput("attribute filter key must not be empty")
		}
		// Search span attributes AND resource attributes: a caller should not
		// need to know which one a key lives in. `service.name`, for example,
		// arrives as a resource attribute (and is additionally promoted to the
		// service_name column) but is absent from span attributes — filtering
		// only attributes silently matches nothing for the most common key.
		conds = append(conds,
			"(json_extract_string(attributes, ?) = ? OR json_extract_string(resource_attributes, ?) = ?)")
		params = append(params, jsonPath(f.Key), f.Value, jsonPath(f.Key), f.Value)
	}

	params = append(params, limit)
	q := `SELECT trace_id, span_id, service_name, span_name, span_kind,
	             start_time, end_time, duration_ms, status_code, attributes
	      FROM traces
	      WHERE ` + strings.Join(conds, " AND ") + `
	      ORDER BY start_time DESC
	      LIMIT ?`

	return c.engine.Query(q, params...)
}

// SearchLogs finds log records matching filters, scoped to tenant — the
// log-signal counterpart of SearchSpans, with the same filter semantics: an
// exact service name, a case-insensitive exact severity (matched against
// severity_text, which senders set alongside severity_number), a
// case-insensitive body substring, and attribute filters matched against
// record attributes OR resource attributes. Results are newest first.
//
// traceID, when non-empty, restricts to records carrying that trace id. A
// trace id is a precise key, so — symmetric with LookupTrace — it defaults
// to the whole history rather than the 60-minute window; pass an explicit
// sinceMinutes to bound it anyway. This is what lets a trace's logs be
// found however old the trace is.
func (c *Core) SearchLogs(tenant, traceID, serviceName, severity, search string, filters []AttrFilter, sinceMinutes, limit int) ([]map[string]any, []string, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}

	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	conds := []string{c.tenantPredicate()}
	params := []any{tenant}

	// Time window: default 60 minutes, except for a trace-scoped lookup with
	// no explicit window — see the doc comment.
	since := sinceMinutes
	if since <= 0 && traceID == "" {
		since = 60
	}
	if since > 0 {
		conds = append(conds, "timestamp >= ?")
		params = append(params, time.Now().Add(-time.Duration(since)*time.Minute).UnixMicro())
	}

	if traceID != "" {
		conds = append(conds, "trace_id = ?")
		params = append(params, traceID)
	}
	if serviceName != "" {
		conds = append(conds, "service_name = ?")
		params = append(params, serviceName)
	}
	if severity != "" {
		conds = append(conds, "UPPER(severity_text) = ?")
		params = append(params, strings.ToUpper(severity))
	}
	if search != "" {
		conds = append(conds, "body ILIKE ?")
		params = append(params, "%"+search+"%")
	}
	for _, f := range filters {
		if f.Key == "" {
			return nil, nil, invalidInput("attribute filter key must not be empty")
		}
		// Dual-scope like SearchSpans: the caller should not need to know
		// whether a key lives in the record's own attributes or its resource
		// attributes.
		conds = append(conds,
			"(json_extract_string(attributes, ?) = ? OR json_extract_string(resource_attributes, ?) = ?)")
		params = append(params, jsonPath(f.Key), f.Value, jsonPath(f.Key), f.Value)
	}

	params = append(params, limit)
	q := `SELECT timestamp, service_name, severity_text, severity_number,
	             body, trace_id, span_id, attributes
	      FROM logs
	      WHERE ` + strings.Join(conds, " AND ") + `
	      ORDER BY timestamp DESC
	      LIMIT ?`

	return c.engine.Query(q, params...)
}

// metricGroupKeys splits a group_by string into individual keys: comma-
// separated, whitespace-trimmed, empty parts dropped. An empty group_by means
// no grouping at all.
func metricGroupKeys(groupBy string) []string {
	var keys []string
	for _, part := range strings.Split(groupBy, ",") {
		if k := strings.TrimSpace(part); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// attrKeyCharset bounds what an attr.<key> group key may contain. Because the
// validated key is later embedded in a single-quoted JSON-path string literal
// (the "strictly quoted" option — identifiers cannot be bound as parameters
// and SELECT-list placeholders would scramble param ordering), the charset
// deliberately excludes both quote characters so nothing can break out.
const attrKeyCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-"

func validAttrKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if !strings.ContainsRune(attrKeyCharset, r) {
			return false
		}
	}
	return true
}

// metricGroupExpr resolves one group_by key to (SQL expression, result alias).
// A plain column name must be on the knownColumns allowlist; a data-point
// attribute key is written attr.<key> (or attributes.<key>) and extracts
// json_extract_string(attributes, '$."<key>"'), aliased back to the key so
// the result rows carry e.g. "attr.kind". The attribute form is how
// metric_query reaches data-point attributes like kind/backend/status, which
// live in the metrics.attributes JSON column rather than as first-class
// columns.
func metricGroupExpr(key string) (expr, alias string, ok bool) {
	for _, prefix := range []string{"attr.", "attributes."} {
		if rest, found := strings.CutPrefix(key, prefix); found {
			if !validAttrKey(rest) {
				return "", "", false
			}
			// Safe to embed: rest passed validAttrKey, so it contains no quote
			// characters and jsonPath's own escaping is a no-op here.
			return fmt.Sprintf("json_extract_string(attributes, '%s')", jsonPath(rest)), key, true
		}
	}
	col, ok := quoteIdent(key)
	if !ok {
		return "", "", false
	}
	return col, col, true
}

// QueryMetric aggregates a metric over a time range, optionally grouped by
// columns and/or data-point attribute keys, scoped to tenant.
//
// groupBy is comma-separated; each key is either a plain column (the
// knownColumns allowlist) or attr.<key> / attributes.<key> for a data-point
// attribute. filters match on data-point attributes OR resource attributes,
// exactly like span_search.
//
// value_double is the normalised numeric column: it is populated for both
// double and int points (see the receiver's fillNumberDataPoint). count on a
// gauge counts exported points, not events: a gauge re-exported every
// interval is counted once per export.
//
// Histogram and exponential histogram metrics (decided by the most recent
// point) are aggregated from count/sum/bucket_counts instead: count and sum
// of observations, avg = sum/count, min/max as reported by the points, and
// p50/p95/p99 interpolated from merged buckets, and buckets (one row per
// bucket: le, value) — the last two for explicit-bucket histograms only. Cumulative series are turned into increments first; see
// histogram.go.
func (c *Core) QueryMetric(tenant, metricName, aggregation string, sinceMinutes int, groupBy string, filters []AttrFilter) ([]map[string]any, []string, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}
	if metricName == "" {
		return nil, nil, invalidInput("metric_name is required")
	}

	since := sinceMinutes
	if since <= 0 {
		since = 60
	}
	cutoff := time.Now().Add(-time.Duration(since) * time.Minute).UnixMicro()

	selectCols := []string{}
	var groupExprs, groupAliases []string
	groupKeys := metricGroupKeys(groupBy)
	for _, key := range groupKeys {
		expr, alias, ok := metricGroupExpr(key)
		if !ok {
			return nil, nil, invalidInput(
				"group_by %q is not a groupable column; allowed columns: %s — or attr.<key> / attributes.<key> for a data-point attribute (key charset %s)",
				key, knownColumnsList(), attrKeyCharset)
		}
		groupExprs = append(groupExprs, expr)
		groupAliases = append(groupAliases, alias)
		if expr == alias {
			selectCols = append(selectCols, expr)
		} else {
			selectCols = append(selectCols, fmt.Sprintf(`%s AS "%s"`, expr, alias))
		}
	}
	conds := []string{"metric_name = ?", "timestamp >= ?", c.tenantPredicate()}
	params := []any{metricName, cutoff, tenant}

	for _, f := range filters {
		if f.Key == "" {
			return nil, nil, invalidInput("attribute filter key must not be empty")
		}
		// Same dual-scope match as span_search: a caller should not need to
		// know whether a key lives in data-point attributes or resource
		// attributes.
		conds = append(conds,
			"(json_extract_string(attributes, ?) = ? OR json_extract_string(resource_attributes, ?) = ?)")
		params = append(params, jsonPath(f.Key), f.Value, jsonPath(f.Key), f.Value)
	}

	// A histogram keeps its data in count/sum/bucket_counts, not value_double;
	// the most recent point decides which path aggregates this metric.
	typeRows, _, err := c.engine.Query(`SELECT metric_type FROM metrics WHERE `+
		strings.Join(conds, " AND ")+` ORDER BY timestamp DESC LIMIT 1`, params...)
	if err != nil {
		return nil, nil, err
	}
	if len(typeRows) == 1 {
		if mt, _ := typeRows[0]["metric_type"].(string); mt == typeHistogram || mt == typeExpHistogram {
			hconds := strings.Join(append(conds, "metric_type = ?"), " AND ")
			return c.queryHistogram(mt, aggregation, groupExprs, groupAliases, hconds, append(params, mt), cutoff)
		}
	}

	agg, err := aggregationExpr(aggregation)
	if err != nil {
		return nil, nil, err
	}
	selectCols = append(selectCols, fmt.Sprintf("%s AS value", agg))

	q := "SELECT " + strings.Join(selectCols, ", ") + ` FROM metrics
	      WHERE ` + strings.Join(conds, " AND ")

	if len(groupKeys) > 0 {
		// Positional GROUP BY/ORDER BY: the group expressions are the first
		// selectCols, in order, so this refers to them without repeating
		// their (embedded) expressions.
		positions := make([]string, len(groupKeys))
		for i := range positions {
			positions[i] = fmt.Sprintf("%d", i+1)
		}
		q += " GROUP BY " + strings.Join(positions, ", ") + " ORDER BY 1"
	}

	return c.engine.Query(q, params...)
}

// discoveryTTL is how long a List* result is reused. Every page load (and
// every reconnect after an error) asks for the same lists; re-scanning for
// each is pure cost, and a list that is a few minutes stale is harmless.
const discoveryTTL = 5 * time.Minute

type cachedResult struct {
	rows []map[string]any
	cols []string
	at   time.Time
}

// discover runs a bounded discovery query through a short-lived cache keyed
// by key. Results are shared between callers and must be treated read-only.
// Errors are not cached.
func (c *Core) discover(key, q string, args ...any) ([]map[string]any, []string, error) {
	c.cacheMu.Lock()
	if r, ok := c.cache[key]; ok && time.Since(r.at) < discoveryTTL {
		c.cacheMu.Unlock()
		return r.rows, r.cols, nil
	}
	c.cacheMu.Unlock()

	rows, cols, err := c.engine.Query(q, args...)
	if err != nil {
		return nil, nil, err
	}
	c.cacheMu.Lock()
	if c.cache == nil {
		c.cache = make(map[string]cachedResult)
	}
	c.cache[key] = cachedResult{rows: rows, cols: cols, at: time.Now()}
	c.cacheMu.Unlock()
	return rows, cols, nil
}

// ListTenants returns the distinct tenant ids seen in any signal: traces,
// logs or metrics. A tenant that only ships logs or metrics is still a tenant,
// and a traces-only list hid it from the dashboard's picker. No MCP tool
// needs this — an agent caller already knows its own tenant — but a
// human-facing consumer needs a way to discover what exists.
func (c *Core) ListTenants() ([]map[string]any, []string, error) {
	path := strings.ReplaceAll(jsonPath(c.tenantAttr), "'", "''")
	var parts []string
	for _, table := range []string{"traces", "logs", "metrics"} {
		parts = append(parts, fmt.Sprintf(`SELECT json_extract_string(resource_attributes, '%s') AS tenant
	      FROM %s
	      WHERE json_extract_string(resource_attributes, '%s') IS NOT NULL`, path, table, path))
	}
	// UNION (not UNION ALL) also de-duplicates across signals.
	q := strings.Join(parts, "\n	      UNION\n	      ") + "\n	      ORDER BY 1"
	return c.discover("tenants", q)
}

// ListServices returns distinct service names within one tenant.
func (c *Core) ListServices(tenant string) ([]map[string]any, []string, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}
	q := `SELECT DISTINCT service_name FROM traces WHERE ` + c.tenantPredicate() + ` ORDER BY 1`
	return c.discover("services\x00"+tenant, q, tenant)
}

// ListLogServices returns distinct service names within one tenant as seen
// in the LOG signal. Separate from ListServices because the two signals'
// service sets can differ — a service that only ships logs would be missing
// from a traces-derived list, and a consumer's log filter would then
// silently offer the wrong options.
func (c *Core) ListLogServices(tenant string) ([]map[string]any, []string, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}
	q := `SELECT DISTINCT service_name FROM logs WHERE ` + c.tenantPredicate() + ` ORDER BY 1`
	return c.discover("logservices\x00"+tenant, q, tenant)
}

// ListMetricNames returns distinct metric names within one tenant. Like
// ListServices, no MCP tool needs this — an agent caller already knows the
// metric name it wants — but a human-facing consumer has no way to guess an
// exact metric_name string without seeing what's actually been ingested.
func (c *Core) ListMetricNames(tenant string) ([]map[string]any, []string, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}
	q := `SELECT DISTINCT metric_name FROM metrics WHERE ` + c.tenantPredicate() + ` ORDER BY 1`
	return c.discover("metricnames\x00"+tenant, q, tenant)
}
