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

// histogramDeltasCTE returns the WITH clause that yields increment rows
// (dc, ds or db) and the group columns g1..gN. db is absent entirely when
// withBuckets is false: count/sum/avg do not read bucket_counts at all, so
// its JSON is never parsed. min/max do not use this CTE either (they
// aggregate the points directly).
//
// Delta-temporality points are already increments: they never enter the
// window, they are unioned in as they are. Only cumulative points are lagged,
// and they are partitioned by the single hashed series key.
//
// Cumulative increments telescope inside a segment: a segment is a run of
// points between counter resets (and, for bucket lists, changes of the list
// length). The running segment id is a window over the scalar count and list
// length only, never over the bucket list itself. One row per segment is then
// enough: last - first gives the sum of every interior increment, and the
// first point contributes itself only when the segment starts with a reset
// or with an in-window start. With a step the same holds per (segment, time
// bucket), with a lag over the few per-bucket rows instead of every point.
//
// step means the first group expression is the time bucket (queryHistogram
// prepends it when the caller asked for a step). Parameters are returned in
// full, including condsParams, because a bucket query reads metrics once more
// to fetch only the segment and bucket boundary lists.
func histogramDeltasCTE(groupExprs []string, conds string, condsParams []any, withBuckets bool, cutoff time.Time, step bool) (string, []any) {
	gdefs, gcols := "", ""
	for i, e := range groupExprs {
		gdefs += fmt.Sprintf("%s AS g%d, ", e, i+1)
		gcols += fmt.Sprintf("g%d, ", i+1)
	}
	if withBuckets {
		return histogramBucketCTE(gdefs, gcols, conds, condsParams, cutoff, step)
	}
	return histogramScalarCTE(gdefs, gcols, conds, condsParams, cutoff, step)
}

// histogramScalarCTE is the count/sum/avg path: no bucket JSON is read.
// A reset is a point whose count drops. Over one series every regular
// increment telescopes to last - first, and each reset adds back the point
// before it (its increment is counted whole instead of as a drop). That needs
// only lag(c) and lag(s), not a running segment id.
func histogramScalarCTE(gdefs, gcols, conds string, condsParams []any, cutoff time.Time, step bool) (string, []any) {
	q := `WITH pts AS NOT MATERIALIZED (
	SELECT ` + gdefs + histogramSeriesKey + ` AS k,
	       timestamp AS ts, start_timestamp AS st, aggregation_temporality AS temp,
	       CAST(count AS DOUBLE) AS c, sum AS s
	FROM metrics WHERE ` + conds + `
), del AS (
	SELECT ` + gcols + `c AS dc, s AS ds
	FROM pts WHERE temp <> ?
), cum AS (
	SELECT ` + gcols + `k, ts, st, c, s,
	       lag(c) OVER (PARTITION BY k ORDER BY ts) AS pc,
	       lag(s) OVER (PARTITION BY k ORDER BY ts) AS ps
	FROM pts WHERE temp IS NOT DISTINCT FROM ?
), marked AS (
	SELECT ` + gcols + `k, ts, st, c, s, pc, ps,
	       CASE WHEN pc IS NOT NULL AND c < pc THEN TRUE ELSE FALSE END AS rst
	FROM cum
), `
	params := append(append([]any{}, condsParams...), temporalityCumulative, temporalityCumulative)

	if step {
		q += `bucketagg AS (
	SELECT ` + gcols + `k,
	       arg_min(c, ts) AS c0, arg_max(c, ts) AS c1,
	       arg_min(s, ts) AS s0, arg_max(s, ts) AS s1,
	       min(st) AS st0,
	       sum(CASE WHEN rst THEN pc ELSE 0 END) AS rc,
	       sum(CASE WHEN rst THEN ps ELSE 0 END) AS rs
	FROM marked GROUP BY ` + gcols + `k
), lagged AS (
	SELECT *,
	       lag(c1) OVER (PARTITION BY k ORDER BY g1) AS pc1,
	       lag(s1) OVER (PARTITION BY k ORDER BY g1) AS ps1,
	       row_number() OVER (PARTITION BY k ORDER BY g1) AS rn
	FROM bucketagg
), cuminc AS (
	SELECT ` + gcols + `CASE WHEN rn = 1 THEN CASE WHEN st0 >= ? THEN c1 ELSE c1 - c0 END
	            ELSE c1 - pc1 END + rc AS dc,
	       CASE WHEN rn = 1 THEN CASE WHEN st0 >= ? THEN s1 ELSE s1 - s0 END
	            ELSE s1 - ps1 END + rs AS ds
	FROM lagged
), d AS (
	SELECT ` + gcols + `dc, ds FROM del
	UNION ALL
	SELECT ` + gcols + `dc, ds FROM cuminc
)
`
		params = append(params, cutoff.UnixMicro(), cutoff.UnixMicro())
		return q, params
	}

	q += `seg AS (
	SELECT ` + gcols + `k,
	       arg_min(c, ts) AS c0, arg_max(c, ts) AS c1,
	       arg_min(s, ts) AS s0, arg_max(s, ts) AS s1,
	       min(st) AS st0,
	       sum(CASE WHEN rst THEN pc ELSE 0 END) AS rc,
	       sum(CASE WHEN rst THEN ps ELSE 0 END) AS rs
	FROM marked GROUP BY ` + gcols + `k
), cuminc AS (
	SELECT ` + gcols + `CASE WHEN st0 >= ? THEN c1 ELSE c1 - c0 END + rc AS dc,
	       CASE WHEN st0 >= ? THEN s1 ELSE s1 - s0 END + rs AS ds
	FROM seg
), d AS (
	SELECT ` + gcols + `dc, ds FROM del
	UNION ALL
	SELECT ` + gcols + `dc, ds FROM cuminc
)
`
	params = append(params, cutoff.UnixMicro(), cutoff.UnixMicro())
	return q, params
}

// histogramBucketCTE is the p50/p95/p99/buckets path. The window carries the
// scalar count and the bucket list length only. Bucket lists are parsed at
// segment boundaries (or per time bucket for a step), which is a small
// multiple of the series count rather than one parse per exported point.
func histogramBucketCTE(gdefs, gcols, conds string, condsParams []any, cutoff time.Time, step bool) (string, []any) {
	q := `WITH pts AS NOT MATERIALIZED (
	SELECT ` + gdefs + histogramSeriesKey + ` AS k,
	       timestamp AS ts, aggregation_temporality AS temp,
	       CAST(count AS DOUBLE) AS c,
	       json_array_length(bucket_counts) AS bl
	FROM metrics WHERE ` + conds + `
), del AS (
	SELECT ` + gdefs + `explicit_bounds AS bounds,
	       from_json(bucket_counts, '["DOUBLE"]') AS db
	FROM metrics WHERE ` + conds + ` AND aggregation_temporality <> ?
), cum AS (
	SELECT ` + gcols + `k, ts, c, bl,
	       lag(c) OVER (PARTITION BY k ORDER BY ts) AS pc,
	       lag(bl) OVER (PARTITION BY k ORDER BY ts) AS pbl
	FROM pts WHERE temp IS NOT DISTINCT FROM ?
), segs AS (
	SELECT ` + gcols + `k, ts, seg, rst FROM (
		SELECT *,
		       sum(CASE WHEN pc IS NOT NULL AND (c < pc OR bl IS DISTINCT FROM pbl) THEN 1 ELSE 0 END)
		         OVER (PARTITION BY k ORDER BY ts ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS seg,
		       CASE WHEN pc IS NOT NULL AND c < pc THEN TRUE ELSE FALSE END AS rst
		FROM cum
	)
), `
	// Placeholder order: pts conds, del conds, del temporality, cum
	// temporality, the boundary join's conds, then the cutoff(s).
	params := append([]any{}, condsParams...)
	params = append(params, condsParams...)
	params = append(params, temporalityCumulative, temporalityCumulative)

	if step {
		q += `bucketmeta AS (
	SELECT ` + gcols + `k, seg,
	       count(*) AS n,
	       arg_min(rst, ts) AS rst0,
	       min(ts) AS ts0, max(ts) AS ts1
	FROM segs GROUP BY ` + gcols + `k, seg
), bucketrows AS (
	SELECT *, row_number() OVER (PARTITION BY k, seg ORDER BY g1) AS rn
	FROM bucketmeta
), lastb AS (
	SELECT br.*,
	       m.bucket_counts AS bc1,
	       m.start_timestamp AS st0, m.explicit_bounds AS bounds
	FROM bucketrows br
	JOIN metrics m
	  ON m.timestamp = br.ts1
	 AND hash(m.attributes, m.resource_attributes, m.start_timestamp, m.explicit_bounds) = br.k
	WHERE ` + conds + `
), firstrows AS (
	SELECT * FROM bucketrows WHERE rn = 1
), firstb AS (
	SELECT fr.k, fr.seg, fr.g1,
	       m.bucket_counts AS bc0
	FROM firstrows fr
	JOIN metrics m
	  ON m.timestamp = fr.ts0
	 AND hash(m.attributes, m.resource_attributes, m.start_timestamp, m.explicit_bounds) = fr.k
	WHERE ` + conds + `
), bucketagg AS (
	SELECT lb.*, fb.bc0
	FROM lastb lb
	LEFT JOIN firstb fb ON lb.k = fb.k AND lb.seg = fb.seg AND lb.g1 = fb.g1
), lagged AS (
	SELECT *,
	       lag(bc1) OVER (PARTITION BY k, seg ORDER BY g1) AS pbc1
	FROM bucketagg
), cuminc AS (
	SELECT ` + gcols + `bounds,
	       CASE WHEN rn = 1 THEN
	                 CASE WHEN rst0 OR (seg = 0 AND st0 >= ?) THEN from_json(bc1, '["DOUBLE"]')
	                      WHEN n = 1 THEN NULL
	                      ELSE list_transform(from_json(bc1, '["DOUBLE"]'),
	                                          (x, i) -> x - from_json(bc0, '["DOUBLE"]')[i]) END
	            ELSE list_transform(from_json(bc1, '["DOUBLE"]'),
	                                (x, i) -> x - from_json(pbc1, '["DOUBLE"]')[i]) END AS db
	FROM lagged
), d AS (
	SELECT ` + gcols + `bounds, db FROM del
	UNION ALL
	SELECT ` + gcols + `bounds, db FROM cuminc
)
`
		params = append(params, condsParams...)
		params = append(params, condsParams...)
		params = append(params, cutoff.UnixMicro())
		return q, params
	}

	q += `segmeta AS (
	SELECT ` + gcols + `k, seg,
	       count(*) AS n,
	       arg_min(rst, ts) AS rst0,
	       min(ts) AS ts0, max(ts) AS ts1
	FROM segs GROUP BY ` + gcols + `k, seg
), seg_b AS (
	SELECT sm.k, sm.seg,
	       arg_min(from_json(m.bucket_counts, '["DOUBLE"]'), m.timestamp) AS b0,
	       arg_max(from_json(m.bucket_counts, '["DOUBLE"]'), m.timestamp) AS b1,
	       min(m.start_timestamp) AS st0, min(m.explicit_bounds) AS bounds
	FROM segmeta sm
	JOIN metrics m
	  ON m.timestamp IN (sm.ts0, sm.ts1)
	 AND hash(m.attributes, m.resource_attributes, m.start_timestamp, m.explicit_bounds) = sm.k
	WHERE ` + conds + `
	GROUP BY sm.k, sm.seg
), cuminc AS (
	SELECT ` + gcols + `bounds,
	       CASE WHEN rst0 OR (sm.seg = 0 AND sb.st0 >= ?) THEN b1
	            WHEN n = 1 THEN NULL
	            ELSE list_transform(b1, (x, i) -> x - b0[i]) END AS db
	FROM segmeta sm
	JOIN seg_b sb ON sm.k = sb.k AND sm.seg = sb.seg
), d AS (
	SELECT ` + gcols + `bounds, db FROM del
	UNION ALL
	SELECT ` + gcols + `bounds, db FROM cuminc
)
`
	params = append(params, condsParams...)
	params = append(params, cutoff.UnixMicro())
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

	step := len(aliases) > 0 && aliases[0] == "time"

	if q == 0 {
		cte, qp := histogramDeltasCTE(groupExprs, conds, params, false, cutoff, step)
		var expr string
		switch agg {
		case "count":
			expr = "sum(dc)"
		case "sum":
			expr = "sum(ds)"
		case "avg", "mean":
			expr = "sum(ds) / nullif(sum(dc), 0)"
		}
		rows, _, err := c.engine.QuerySince(cutoff, cte+"SELECT "+gsel+expr+" AS value FROM d"+gby, qp...)
		if err != nil {
			return nil, nil, err
		}
		return rename(rows), cols, nil
	}

	// Percentiles and buckets: merge the increments bucket by bucket, per
	// group and bounds. The time-bucket path returns one bucket list per
	// (time, group, bounds); the Go side below turns those into quantiles.
	bucketSQL := func(cte string) string {
		if !step {
			return cte + `, bk AS (
	SELECT ` + gsel + `bounds, unnest(db) AS v, unnest(generate_series(1, len(db))) AS i
	FROM d WHERE db IS NOT NULL
)
SELECT ` + gsel + `bounds, i, sum(v) AS v FROM bk GROUP BY ` + gsel + `bounds, i`
		}
		// With a step the group key includes the time bucket. Unnesting every
		// point in SQL then creates points * bucket count rows and one hash
		// group per (time, group, bounds, bucket): a 6 h window at a 5 m step
		// with 9 buckets peaks around 1.4 GB. Merge the lists per (time,
		// group, bounds) in SQL. list_reduce adds two bucket lists element by
		// element; greatest(len) keeps every index of unequal lists, matching
		// what the old unnest-and-group did (a point whose list is shorter
		// contributes 0 beyond its end).
		return cte + `, merged AS (
	SELECT ` + gsel + `bounds,
	       CAST(list_reduce(list(db), (x, y) -> list_transform(
	           range(1, greatest(len(x), len(y)) + 1),
	           i -> (CASE WHEN i <= len(x) THEN x[i] ELSE 0 END) +
	                (CASE WHEN i <= len(y) THEN y[i] ELSE 0 END))) AS DOUBLE[]) AS db
	FROM d WHERE db IS NOT NULL
	GROUP BY ` + gsel + `bounds
)
SELECT ` + gsel + `bounds, db FROM merged`
	}

	var rows []map[string]any
	if step {
		// A day of 2000 cumulative series at a 5 m step still has 576k
		// per-bucket list rows. Query the series space in hash partitions so
		// only a fraction of those lists is live at once, then concatenate the
		// partial (time, group, bounds) aggregates. Summing the partials is
		// the same as summing the series in one query; mergeStepBucketRows
		// below combines duplicate bucket rows by group and bound. Short
		// windows need fewer partitions, and more partitions cost more scans.
		seriesChunks := 4
		switch hours := time.Since(cutoff).Hours(); {
		case hours > 48:
			seriesChunks = 32
		case hours > 12:
			seriesChunks = 16
		case hours > 8:
			seriesChunks = 8
		}
		for i := 0; i < seriesChunks; i++ {
			chunkConds := conds + " AND " + histogramSeriesKey + " % ? = ?"
			chunkParams := append(append([]any{}, params...), seriesChunks, i)
			cte, cp := histogramDeltasCTE(groupExprs, chunkConds, chunkParams, true, cutoff, true)
			part, _, err := c.engine.QuerySince(cutoff, bucketSQL(cte), cp...)
			if err != nil {
				return nil, nil, err
			}
			rows = append(rows, part...)
		}
	} else {
		cte, cp := histogramDeltasCTE(groupExprs, conds, params, true, cutoff, false)
		var err error
		rows, _, err = c.engine.QuerySince(cutoff, bucketSQL(cte), cp...)
		if err != nil {
			return nil, nil, err
		}
	}
	if step {
		rows = mergeStepBucketRows(rows, gcols)
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
	if step {
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

// mergeStepBucketRows combines the per-partition results of a chunked step
// bucket query. Each partition returns one list per (time, group, bounds);
// summing those lists element by element is exactly the sum a single query
// would have produced, but without holding every series' list at once.
func mergeStepBucketRows(rows []map[string]any, gcols []string) []map[string]any {
	type acc struct {
		row map[string]any
		db  []float64
	}
	byKey := map[string]*acc{}
	var keys []string
	for _, r := range rows {
		key := ""
		for _, g := range gcols {
			key += fmt.Sprint(r[g]) + "\x00"
		}
		if b, _ := r["bounds"].(string); b != "" {
			key += b
		}
		a, ok := byKey[key]
		if !ok {
			a = &acc{row: r}
			byKey[key] = a
			keys = append(keys, key)
		}
		db := toFloats(r["db"])
		if len(db) > len(a.db) {
			a.db = append(a.db, make([]float64, len(db)-len(a.db))...)
		}
		for i, v := range db {
			a.db[i] += v
		}
	}
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		a := byKey[key]
		a.row["db"] = a.db
		out = append(out, a.row)
	}
	return out
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
