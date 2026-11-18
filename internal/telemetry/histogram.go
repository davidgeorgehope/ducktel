package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Histogram support for QueryMetric.
//
// The standard OTel instrumentations (http.server.duration and friends) emit
// explicit-bucket histograms, usually with CUMULATIVE temporality: every
// export repeats the running totals since the process started. Aggregating
// those needs two things value_double-based gauges don't:
//
//  1. Turn running totals into increments. Within one series (same attributes,
//     same resource, same start_timestamp, same bounds) each point minus the
//     previous point in the window is what happened in between. The first
//     point of a series in the window has no predecessor: it is counted whole
//     only when the series itself started inside the window, otherwise it is
//     the baseline. A count that goes down is a reset and is counted whole.
//     DELTA points are already increments.
//  2. Merge buckets, then interpolate a quantile from them, the same way
//     Prometheus' histogram_quantile does.

const (
	temporalityCumulative = "AGGREGATION_TEMPORALITY_CUMULATIVE"
	typeHistogram         = "histogram"
	typeExpHistogram      = "exponential_histogram"
)

// histogramSeriesKey is the series identity the window used to partition on
// (attributes, resource_attributes, start_timestamp and explicit_bounds),
// hashed into one UBIGINT. Carrying one integer instead of the attribute and
// bounds strings through the window and the later CTEs is what keeps a 6 h
// scan inside a 1 GB container. A hash collision would merge two series;
// with 64 bits that is far less likely than a corrupted Parquet file, and the
// alternative (carrying the strings) does not fit in the container at all.
const histogramSeriesKey = "hash(attributes, resource_attributes, start_timestamp, explicit_bounds)"

// histogramDeltasCTE returns the WITH clause that yields one row per point
// with its increment (dc, ds, db) and the group columns g1..gN. db is NULL
// for exponential histograms, whose buckets are not merged here, and absent
// entirely when withBuckets is false: count/sum/avg do not read bucket_counts
// at all, so its JSON is never parsed. min/max do not use this CTE either
// (they aggregate the points directly).
//
// Delta-temporality points are already increments: they never enter the
// window, they are unioned in as they are. Only cumulative points are lagged,
// and they are partitioned by the single hashed series key. The returned
// params are bound after conds' own params.
func histogramDeltasCTE(groupExprs []string, conds string, withBuckets bool, cutoff time.Time) (string, []any) {
	gdefs, gcols := "", ""
	for i, e := range groupExprs {
		gdefs += fmt.Sprintf("%s AS g%d, ", e, i+1)
		gcols += fmt.Sprintf("g%d, ", i+1)
	}

	head := `WITH pts AS (
	SELECT ` + gdefs + histogramSeriesKey + ` AS k,
	       timestamp AS ts, start_timestamp AS st, aggregation_temporality AS temp,
	       CAST(count AS DOUBLE) AS c`
	// The temporality and cutoff parameters are bound after conds' own params,
	// in the order the placeholders appear below.
	params := []any{temporalityCumulative}

	if !withBuckets {
		q := head + `, sum AS s
	FROM metrics WHERE ` + conds + `
), del AS (
	SELECT ` + gcols + `c AS dc, s AS ds
	FROM pts WHERE temp <> ?
), cum AS (
	SELECT ` + gcols + `k, ts, st, c, s,
	       lag(c) OVER (PARTITION BY k ORDER BY ts) AS pc,
	       lag(s) OVER (PARTITION BY k ORDER BY ts) AS ps
	FROM pts WHERE temp IS NOT DISTINCT FROM ?
), cd AS (
	SELECT ` + gcols + `CASE WHEN pc IS NULL THEN CASE WHEN st >= ? THEN c ELSE 0 END
	            WHEN c < pc THEN c
	            ELSE c - pc END AS dc,
	       CASE WHEN pc IS NULL THEN CASE WHEN st >= ? THEN s ELSE 0 END
	            WHEN c < pc THEN s
	            ELSE s - ps END AS ds
	FROM cum
), d AS (
	SELECT ` + gcols + `dc, ds FROM del
	UNION ALL
	SELECT ` + gcols + `dc, ds FROM cd
)
`
		params = append(params, temporalityCumulative, cutoff.UnixMicro(), cutoff.UnixMicro())
		return q, params
	}

	q := head + `, explicit_bounds AS bounds,
	       from_json(bucket_counts, '["DOUBLE"]') AS b
	FROM metrics WHERE ` + conds + `
), del AS (
	SELECT ` + gcols + `bounds, b AS db
	FROM pts WHERE temp <> ?
), cum AS (
	SELECT ` + gcols + `k, ts, st, c, bounds, b,
	       lag(c) OVER (PARTITION BY k ORDER BY ts) AS pc,
	       lag(b) OVER (PARTITION BY k ORDER BY ts) AS pb
	FROM pts WHERE temp IS NOT DISTINCT FROM ?
), cd AS (
	SELECT ` + gcols + `bounds,
	       CASE WHEN pc IS NULL THEN CASE WHEN st >= ? THEN b END
	            WHEN c < pc THEN b
	            WHEN len(b) = len(pb) THEN list_transform(b, (x, i) -> x - pb[i])
	       END AS db
	FROM cum
), d AS (
	SELECT ` + gcols + `bounds, db FROM del
	UNION ALL
	SELECT ` + gcols + `bounds, db FROM cd
)
`
	params = append(params, temporalityCumulative, cutoff.UnixMicro())
	return q, params
}

// queryHistogram aggregates histogram points. groupExprs/aliases are the
// resolved group_by keys; conds/params the WHERE clause (already including
// the metric type predicate).
func (c *Core) queryHistogram(metricType, aggregation string, groupExprs, aliases []string, conds string, params []any, cutoff time.Time) ([]map[string]any, []string, error) {
	agg := strings.ToLower(aggregation)
	var q float64
	switch agg {
	case "count", "sum", "avg", "mean", "min", "max":
	case "p50", "p95", "p99", "buckets":
		if metricType != typeHistogram {
			return nil, nil, invalidInput("aggregation %q needs explicit bucket boundaries; %s points support count, sum, avg, min, max", aggregation, metricType)
		}
		q = map[string]float64{"p50": 0.5, "p95": 0.95, "p99": 0.99, "buckets": -1}[agg]
	default:
		return nil, nil, invalidInput("aggregation %q is not supported; use one of: avg, sum, min, max, count, p50, p95, p99, buckets, exemplars", aggregation)
	}

	gdefs, gsel, gby := "", "", ""
	gcols := make([]string, len(groupExprs))
	for i, e := range groupExprs {
		gcols[i] = fmt.Sprintf("g%d", i+1)
		gdefs += fmt.Sprintf("%s AS g%d, ", e, i+1)
	}
	if len(gcols) > 0 {
		gsel = strings.Join(gcols, ", ") + ", "
		gby = " GROUP BY " + strings.Join(gcols, ", ") + " ORDER BY " + strings.Join(gcols, ", ")
	}

	cols := append(append([]string{}, aliases...), "value")
	rename := func(rows []map[string]any) []map[string]any {
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			m := make(map[string]any, len(cols))
			for i, a := range aliases {
				m[a] = r[gcols[i]]
			}
			m["value"] = r["value"]
			out = append(out, m)
		}
		return out
	}

	if agg == "min" || agg == "max" {
		// min/max read the values the points report, not an increment, so they
		// do not need the window (or the bucket JSON) at all. Aggregating the
		// points directly is both cheaper and exactly the old result: the old
		// d carried every point through unchanged for this filter.
		expr := "min(mn)"
		if agg == "max" {
			expr = "max(mx)"
		}
		// An unset min/max is stored as 0; only points that carry one count.
		expr += " FILTER (WHERE c > 0 AND (mn <> 0 OR mx <> 0))"
		pts := `SELECT ` + gdefs + `CAST(count AS DOUBLE) AS c, min AS mn, max AS mx
		        FROM metrics WHERE ` + conds
		rows, _, err := c.engine.QuerySince(cutoff, `SELECT `+gsel+expr+` AS value FROM (`+pts+`)`+gby, params...)
		if err != nil {
			return nil, nil, err
		}
		return rename(rows), cols, nil
	}

	withBuckets := q != 0
	cte, cteParams := histogramDeltasCTE(groupExprs, conds, withBuckets, cutoff)
	params = append(params, cteParams...)

	if q == 0 {
		var expr string
		switch agg {
		case "count":
			expr = "sum(dc)"
		case "sum":
			expr = "sum(ds)"
		case "avg", "mean":
			expr = "sum(ds) / nullif(sum(dc), 0)"
		}
		rows, _, err := c.engine.QuerySince(cutoff, cte+"SELECT "+gsel+expr+" AS value FROM d"+gby, params...)
		if err != nil {
			return nil, nil, err
		}
		return rename(rows), cols, nil
	}

	// Percentiles and buckets: merge the increments bucket by bucket, per
	// group and bounds.
	var bq string
	if len(aliases) > 0 && aliases[0] == "time" {
		// With a step the group key includes the time bucket. Unnesting every
		// point in SQL then creates points * bucket count rows and one hash
		// group per (time, group, bounds, bucket): a 6 h window at a 5 m step
		// with 9 buckets peaks around 1.4 GB. Merge the lists per (time,
		// group, bounds) in SQL and walk the resulting small list in Go
		// instead. list_reduce adds two bucket lists element by element;
		// greatest(len) keeps every index of unequal lists, matching what the
		// old unnest-and-group did (a point whose list is shorter contributes 0
		// beyond its end).
		bq = cte + `, merged AS (
	SELECT ` + gsel + `bounds,
	       CAST(list_reduce(list(db), (x, y) -> list_transform(
	           range(1, greatest(len(x), len(y)) + 1),
	           i -> (CASE WHEN i <= len(x) THEN x[i] ELSE 0 END) +
	                (CASE WHEN i <= len(y) THEN y[i] ELSE 0 END))) AS DOUBLE[]) AS db
	FROM d WHERE db IS NOT NULL
	GROUP BY ` + gsel + `bounds
)
SELECT ` + gsel + `bounds, db FROM merged`
	} else {
		bq = cte + `, bk AS (
	SELECT ` + gsel + `bounds, unnest(db) AS v, unnest(generate_series(1, len(db))) AS i
	FROM d WHERE db IS NOT NULL
)
SELECT ` + gsel + `bounds, i, sum(v) AS v FROM bk GROUP BY ` + gsel + `bounds, i`
	}
	rows, _, err := c.engine.QuerySince(cutoff, bq, params...)
	if err != nil {
		return nil, nil, err
	}

	type group struct {
		vals    []any
		buckets []bucket
	}
	groups := map[string]*group{}
	var order []string
	for _, r := range rows {
		vals := make([]any, len(gcols))
		for i, g := range gcols {
			vals[i] = r[g]
		}
		key := fmt.Sprint(vals...)
		g, ok := groups[key]
		if !ok {
			g = &group{vals: vals}
			groups[key] = g
			order = append(order, key)
		}
		var bounds []float64
		if s, _ := r["bounds"].(string); s != "" {
			if err := json.Unmarshal([]byte(s), &bounds); err != nil {
				continue
			}
		}
		add := func(idx int, n float64) {
			lo, hi := math.Inf(-1), math.Inf(1)
			if idx-2 >= 0 && idx-2 < len(bounds) {
				lo = bounds[idx-2]
			}
			if idx-1 < len(bounds) {
				hi = bounds[idx-1]
			}
			g.buckets = append(g.buckets, bucket{lo: lo, hi: hi, n: n})
		}
		if db, ok := r["db"]; ok {
			// The step path returns one merged list per (time, group, bounds).
			for j, v := range toFloats(db) {
				add(j+1, v)
			}
			continue
		}
		add(int(toFloat(r["i"])), toFloat(r["v"]))
	}
	// The query has no ORDER BY because the groups are assembled in Go. When
	// time leads the group keys it must sort numerically (a string sort would
	// rely on every timestamp having the same width), then by the remaining
	// group values.
	if len(aliases) > 0 && aliases[0] == "time" {
		sort.SliceStable(order, func(i, j int) bool {
			a, b := groups[order[i]], groups[order[j]]
			if ta, tb := toFloat(a.vals[0]), toFloat(b.vals[0]); ta != tb {
				return ta < tb
			}
			return fmt.Sprint(a.vals[1:]) < fmt.Sprint(b.vals[1:])
		})
	} else {
		sort.Strings(order)
	}

	if q < 0 {
		return bucketRows(order, func(k string) ([]any, []bucket) { return groups[k].vals, groups[k].buckets }, aliases)
	}

	out := make([]map[string]any, 0, len(order))
	for _, key := range order {
		g := groups[key]
		m := make(map[string]any, len(cols))
		for i, a := range aliases {
			m[a] = g.vals[i]
		}
		if v, ok := bucketQuantile(q, g.buckets); ok {
			m["value"] = v
		} else {
			m["value"] = nil
		}
		out = append(out, m)
	}
	return out, cols, nil
}

type bucket struct{ lo, hi, n float64 }

// bucketRows is aggregation=buckets: one row per group and upper bound, with
// columns <group keys>, le, value. le is the bucket's inclusive upper bound
// (null for the last bucket, above the highest bound) and value the number
// of observations in it within the window. Buckets with the same upper bound
// (series with different bound sets) are summed.
func bucketRows(order []string, get func(string) ([]any, []bucket), aliases []string) ([]map[string]any, []string, error) {
	cols := append(append([]string{}, aliases...), "le", "value")
	out := []map[string]any{}
	for _, key := range order {
		vals, bs := get(key)
		sum := map[float64]float64{}
		for _, b := range bs {
			sum[b.hi] += b.n
		}
		his := make([]float64, 0, len(sum))
		for hi := range sum {
			his = append(his, hi)
		}
		sort.Float64s(his)
		for _, hi := range his {
			m := make(map[string]any, len(cols))
			for i, a := range aliases {
				m[a] = vals[i]
			}
			if math.IsInf(hi, 1) {
				m["le"] = nil
			} else {
				m["le"] = hi
			}
			m["value"] = sum[hi]
			out = append(out, m)
		}
	}
	return out, cols, nil
}

// bucketQuantile interpolates the q-quantile linearly inside the bucket where
// the cumulative count crosses q*total, as Prometheus' histogram_quantile
// does. The first bucket's lower edge is taken as 0 when its upper edge is
// positive; a quantile landing in the +Inf bucket returns the highest finite
// bound. Buckets from different bound sets are merged by their upper edge,
// which keeps the answer approximate rather than wrong when bounds changed.
func bucketQuantile(q float64, bs []bucket) (float64, bool) {
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].hi < bs[j].hi })
	var total float64
	for _, b := range bs {
		if b.n > 0 {
			total += b.n
		}
	}
	if total <= 0 {
		return 0, false
	}
	rank := q * total
	var cum float64
	for i, b := range bs {
		if b.n <= 0 {
			continue
		}
		if cum+b.n >= rank {
			if math.IsInf(b.hi, 1) {
				// Highest finite edge seen.
				for j := i; j >= 0; j-- {
					if !math.IsInf(bs[j].lo, 0) {
						return bs[j].lo, true
					}
					if !math.IsInf(bs[j].hi, 0) {
						return bs[j].hi, true
					}
				}
				return 0, false
			}
			lo := b.lo
			if math.IsInf(lo, -1) {
				if b.hi > 0 {
					lo = 0
				} else {
					return b.hi, true
				}
			}
			return lo + (b.hi-lo)*(rank-cum)/b.n, true
		}
		cum += b.n
	}
	return 0, false
}

// toFloats reads a DuckDB list (scanned as []interface{}) into []float64.
// The driver returns DOUBLE[] as []interface{} of float64.
func toFloats(v any) []float64 {
	switch x := v.(type) {
	case []float64:
		return x
	case []any:
		out := make([]float64, len(x))
		for i, e := range x {
			out[i] = toFloat(e)
		}
		return out
	}
	return nil
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case int:
		return float64(x)
	case uint64:
		return float64(x)
	case uint32:
		return float64(x)
	}
	return 0
}
