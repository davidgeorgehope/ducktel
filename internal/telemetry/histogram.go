package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
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

// histogramDeltasCTE returns the WITH clause that yields one row per point
// with its increment (dc, ds, db) and the group columns g1..gN. db is NULL
// for exponential histograms, whose buckets are not merged here.
//
// The temporality and cutoff parameters are bound after conds' own params.
func histogramDeltasCTE(groupExprs []string, conds string) string {
	gsel := ""
	for i, e := range groupExprs {
		gsel += fmt.Sprintf("%s AS g%d, ", e, i+1)
	}
	return `WITH pts AS (
	SELECT ` + gsel + `
	       attributes AS a, resource_attributes AS ra, start_timestamp AS st,
	       explicit_bounds AS bounds, timestamp AS ts, aggregation_temporality AS temp,
	       CAST(count AS DOUBLE) AS c, sum AS s, min AS mn, max AS mx,
	       CASE WHEN metric_type = 'histogram'
	            THEN from_json(bucket_counts, '["DOUBLE"]') END AS b
	FROM metrics WHERE ` + conds + `
), lagged AS (
	SELECT *,
	       lag(c) OVER w AS pc, lag(s) OVER w AS ps, lag(b) OVER w AS pb
	FROM pts
	WINDOW w AS (PARTITION BY a, ra, st, bounds ORDER BY ts)
), modes AS (
	SELECT *,
	       CASE WHEN temp <> ? THEN 1
	            WHEN pc IS NULL THEN CASE WHEN st >= ? THEN 1 ELSE 0 END
	            WHEN c < pc THEN 1
	            ELSE 2 END AS mode
	FROM lagged
), d AS (
	SELECT *,
	       CASE mode WHEN 1 THEN c WHEN 2 THEN c - pc ELSE 0 END AS dc,
	       CASE mode WHEN 1 THEN s WHEN 2 THEN s - ps ELSE 0 END AS ds,
	       CASE mode WHEN 1 THEN b
	                 WHEN 2 THEN CASE WHEN len(b) = len(pb)
	                                  THEN list_transform(b, (x, i) -> x - pb[i]) END
	       END AS db
	FROM modes
)
`
}

// queryHistogram aggregates histogram points. groupExprs/aliases are the
// resolved group_by keys; conds/params the WHERE clause (already including
// the metric type predicate).
func (c *Core) queryHistogram(metricType, aggregation string, groupExprs, aliases []string, conds string, params []any, cutoff int64) ([]map[string]any, []string, error) {
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
		return nil, nil, invalidInput("aggregation %q is not supported; use one of: avg, sum, min, max, count, p50, p95, p99, buckets", aggregation)
	}

	params = append(params, temporalityCumulative, cutoff)
	cte := histogramDeltasCTE(groupExprs, conds)

	gcols := make([]string, len(groupExprs))
	for i := range groupExprs {
		gcols[i] = fmt.Sprintf("g%d", i+1)
	}
	gsel, gby := "", ""
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

	if q == 0 {
		var expr string
		switch agg {
		case "count":
			expr = "sum(dc)"
		case "sum":
			expr = "sum(ds)"
		case "avg", "mean":
			expr = "sum(ds) / nullif(sum(dc), 0)"
		case "min":
			// An unset min/max is stored as 0; only points that carry one count.
			expr = "min(mn) FILTER (WHERE c > 0 AND (mn <> 0 OR mx <> 0))"
		case "max":
			expr = "max(mx) FILTER (WHERE c > 0 AND (mn <> 0 OR mx <> 0))"
		}
		rows, _, err := c.engine.Query(cte+"SELECT "+gsel+expr+" AS value FROM d"+gby, params...)
		if err != nil {
			return nil, nil, err
		}
		return rename(rows), cols, nil
	}

	// Percentiles and buckets: merge the increments bucket by bucket, per
	// group and bounds.
	bq := cte + `, bk AS (
	SELECT ` + gsel + `bounds, unnest(db) AS v, unnest(generate_series(1, len(db))) AS i
	FROM d WHERE db IS NOT NULL
)
SELECT ` + gsel + `bounds, i, sum(v) AS v FROM bk GROUP BY ` + gsel + `bounds, i`
	rows, _, err := c.engine.Query(bq, params...)
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
		idx := int(toFloat(r["i"])) // 1-based
		lo, hi := math.Inf(-1), math.Inf(1)
		if idx-2 >= 0 && idx-2 < len(bounds) {
			lo = bounds[idx-2]
		}
		if idx-1 < len(bounds) {
			hi = bounds[idx-1]
		}
		g.buckets = append(g.buckets, bucket{lo: lo, hi: hi, n: toFloat(r["v"])})
	}
	sort.Strings(order)

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
