package telemetry

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// compareSampleCap bounds how many spans per group (slow and rest) the
// feature counting reads. A variable rather than a constant so tests can
// lower it and exercise the sampling path without seeding 20k spans.
var compareSampleCap = 20000

const (
	defaultCompareLimit = 15
	maxCompareLimit     = 50
	// compareMaxKeyValues drops high-cardinality keys (ids, URLs with ids,
	// timestamps): their values are not a shared property of slow spans.
	compareMaxKeyValues = 50
	// compareMinPairCount drops a value that is too rare to be a pattern.
	compareMinPairCount = 5
)

// CompareOpts scopes a CompareSpans call. Slow, when it has a bound, defines
// the slow group directly; otherwise slow means slower than the scope's p95.
type CompareOpts struct {
	ServiceName string
	SpanName    string // optional exact span_name
	Filters     []AttrFilter
	Since       int // minutes, default 60
	Until       int // minutes before now where the window ends; default 0 (now)
	Slow        Range
	Limit       int // rows returned, default 15, cap 50
}

// SpanComparison is the answer to "what do the slow spans have that the rest
// do not": the attribute values whose share differs most between the two
// groups, plus the sizes both shares were computed from.
type SpanComparison struct {
	// ThresholdMs is the derived p95 duration when Slow had no bound.
	ThresholdMs *float64 `json:"threshold_ms,omitempty"`
	// MinMs and MaxMs are the duration bounds actually used for "slow".
	MinMs *float64 `json:"min_ms,omitempty"`
	MaxMs *float64 `json:"max_ms,omitempty"`

	SlowN int64 `json:"slow_n"`
	RestN int64 `json:"rest_n"`
	// Sampled is true when a group was larger than compareSampleCap and its
	// shares came from a random sample.
	Sampled bool `json:"sampled"`

	Rows []CompareRow `json:"rows"`
}

// CompareRow is one attribute value's share in each group.
type CompareRow struct {
	Key     string  `json:"key"` // e.g. "cache", "resource.k8s.pod.name", "span_name"
	Value   string  `json:"value"`
	SlowPct float64 `json:"slow_pct"` // share of slow spans with this value, 0-100, 1 decimal
	RestPct float64 `json:"rest_pct"`
	Diff    float64 `json:"diff"` // slow_pct - rest_pct
}

// CompareSpans compares the slow spans of one scope with the rest of that
// scope and returns the attribute values whose share differs most. The scope
// is the same one span_search takes (service, span name, filters, window);
// the slow definition is either the caller's duration range or the scope's
// p95. Both groups are sampled independently when they exceed the cap, and
// the reported group sizes stay the true counts.
func (c *Core) CompareSpans(tenant string, o CompareOpts) (*SpanComparison, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, err
	}
	if err := o.Slow.validate("slow duration"); err != nil {
		return nil, err
	}

	limit := o.Limit
	if limit <= 0 {
		limit = defaultCompareLimit
	}
	if limit > maxCompareLimit {
		limit = maxCompareLimit
	}

	win := Window{Since: o.Since, Until: o.Until}
	if err := win.validate(); err != nil {
		return nil, err
	}
	cutoffTime, endTime := win.bounds(time.Now())

	// The scope deliberately carries no duration predicate: the duration
	// range decides which side of the comparison a span falls on, not which
	// spans are in the comparison.
	scopeConds, scopeParams, err := c.spanConds(tenant, o.ServiceName, o.Filters, Range{})
	if err != nil {
		return nil, err
	}
	conds := append([]string{"start_time >= ?"}, scopeConds...)
	params := append([]any{cutoffTime.UnixMicro()}, scopeParams...)
	if !endTime.IsZero() {
		conds = append(conds, "start_time < ?")
		params = append(params, endTime.UnixMicro())
	}
	if o.SpanName != "" {
		conds = append(conds, "span_name = ?")
		params = append(params, o.SpanName)
	}
	scopeWhere := strings.Join(conds, " AND ")

	out := &SpanComparison{Rows: []CompareRow{}}

	slowConds, slowParams := o.Slow.conds("duration_ms")
	if len(slowConds) == 0 {
		// No slow range: derive the threshold from the scope itself, computed
		// once so the count and feature queries share the same boundary. An
		// approximate quantile streams; an exact one holds every duration of
		// the scope in memory (tens of millions over a busy day).
		q := `SELECT CAST(approx_quantile(duration_ms, 0.95) AS DOUBLE) AS threshold
		      FROM traces
		      WHERE ` + scopeWhere
		rows, _, err := c.engine.QuerySince(cutoffTime, q, params...)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 || rows[0]["threshold"] == nil {
			return out, nil
		}
		threshold := toFloat(rows[0]["threshold"])
		out.ThresholdMs = &threshold
		out.MinMs = &threshold
		slowConds = []string{"duration_ms >= ?"}
		slowParams = []any{threshold}
	} else {
		out.MinMs = o.Slow.Min
		out.MaxMs = o.Slow.Max
	}
	slowExpr := "(" + strings.Join(slowConds, " AND ") + ")"

	// True group sizes first: sampling must not change them.
	countQ := `SELECT count(*) FILTER (WHERE ` + slowExpr + `) AS slow_n,
	                  count(*) FILTER (WHERE NOT ` + slowExpr + `) AS rest_n
	           FROM traces
	           WHERE ` + scopeWhere
	countArgs := append(append(append([]any{}, slowParams...), slowParams...), params...)
	countRows, _, err := c.engine.QuerySince(cutoffTime, countQ, countArgs...)
	if err != nil {
		return nil, err
	}
	if len(countRows) == 0 {
		return out, nil
	}
	out.SlowN = intVal(countRows[0]["slow_n"])
	out.RestN = intVal(countRows[0]["rest_n"])
	if out.SlowN == 0 || out.RestN == 0 {
		return out, nil
	}

	sampledSlow := out.SlowN
	if sampledSlow > int64(compareSampleCap) {
		sampledSlow = int64(compareSampleCap)
		out.Sampled = true
	}
	sampledRest := out.RestN
	if sampledRest > int64(compareSampleCap) {
		sampledRest = int64(compareSampleCap)
		out.Sampled = true
	}

	// A random sample per group, then one row per feature occurrence;
	// aggregation and the noise filters stay in SQL so only the surviving
	// pairs travel back.
	skipService := ""
	if o.ServiceName != "" {
		skipService = ` AND key <> 'resource.service.name'`
	}
	// Each group is sampled on its own with a reservoir: it streams and keeps
	// only the sample in memory. A row_number() over random() would sort the
	// whole scope, attribute strings included, and a busy service's hour is
	// over a million spans.
	cols := `span_name, status_code, span_kind, attributes, resource_attributes`
	capRows := strconv.Itoa(compareSampleCap)
	featureQ := `WITH sampled AS (
	    SELECT *, true AS slow FROM (
	        SELECT ` + cols + ` FROM traces WHERE ` + scopeWhere + ` AND (` + slowExpr + `)
	    ) USING SAMPLE reservoir(` + capRows + ` ROWS)
	    UNION ALL
	    SELECT *, false AS slow FROM (
	        SELECT ` + cols + ` FROM traces WHERE ` + scopeWhere + ` AND NOT (` + slowExpr + `)
	    ) USING SAMPLE reservoir(` + capRows + ` ROWS)
	), features AS (
	    SELECT slow, 'span_name' AS key, span_name AS value FROM sampled
	    UNION ALL
	    SELECT slow, 'status_code', status_code FROM sampled
	    UNION ALL
	    SELECT slow, 'span_kind', span_kind FROM sampled
	    UNION ALL
	    SELECT slow, k AS key,
	           json_extract_string(attributes, '$."' || replace(k, '"', chr(92) || '"') || '"') AS value
	    FROM sampled, unnest(CASE WHEN json_valid(attributes) THEN json_keys(attributes) ELSE CAST([] AS VARCHAR[]) END) AS t(k)
	    UNION ALL
	    SELECT slow, 'resource.' || k AS key,
	           json_extract_string(resource_attributes, '$."' || replace(k, '"', chr(92) || '"') || '"') AS value
	    FROM sampled, unnest(CASE WHEN json_valid(resource_attributes) THEN json_keys(resource_attributes) ELSE CAST([] AS VARCHAR[]) END) AS t(k)
	)
	SELECT key, value, slow_count, rest_count
	FROM (
	    SELECT key, value,
	           count(*) FILTER (WHERE slow) AS slow_count,
	           count(*) FILTER (WHERE NOT slow) AS rest_count,
	           count(*) OVER (PARTITION BY key) AS key_values
	    FROM features
	    WHERE value IS NOT NULL AND value <> ''
	    GROUP BY key, value
	) agg
	WHERE key_values <= ` + strconv.Itoa(compareMaxKeyValues) +
		` AND slow_count + rest_count >= ` + strconv.Itoa(compareMinPairCount) + skipService

	featureArgs := append(append(append(append([]any{}, params...), slowParams...), params...), slowParams...)
	pairs, _, err := c.engine.QuerySince(cutoffTime, featureQ, featureArgs...)
	if err != nil {
		return nil, err
	}

	for _, r := range pairs {
		slowPct := 100 * float64(intVal(r["slow_count"])) / float64(sampledSlow)
		restPct := 100 * float64(intVal(r["rest_count"])) / float64(sampledRest)
		row := CompareRow{
			Key:     strVal(r["key"]),
			Value:   strVal(r["value"]),
			SlowPct: roundPercent(slowPct),
			RestPct: roundPercent(restPct),
		}
		row.Diff = roundPercent(row.SlowPct - row.RestPct)
		out.Rows = append(out.Rows, row)
	}

	// Biggest difference first. On an equal magnitude, the value more common
	// in the slow group comes first: that is the answer to the question, while
	// its mirror image is the same fact stated backwards. Key and value then
	// make the order stable across calls.
	sort.SliceStable(out.Rows, func(i, j int) bool {
		a, b := math.Abs(out.Rows[i].Diff), math.Abs(out.Rows[j].Diff)
		if a != b {
			return a > b
		}
		if out.Rows[i].Diff != out.Rows[j].Diff {
			return out.Rows[i].Diff > out.Rows[j].Diff
		}
		if out.Rows[i].Key != out.Rows[j].Key {
			return out.Rows[i].Key < out.Rows[j].Key
		}
		return out.Rows[i].Value < out.Rows[j].Value
	})
	if len(out.Rows) > limit {
		out.Rows = out.Rows[:limit]
	}
	return out, nil
}

// roundPercent keeps a share to one decimal, the precision a comparison is
// read at; more digits suggest a certainty sampling does not have.
func roundPercent(v float64) float64 { return math.Round(v*10) / 10 }
