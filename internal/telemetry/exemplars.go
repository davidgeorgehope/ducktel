package telemetry

import (
	"fmt"
	"strings"
	"time"
)

// exemplarsJSONShape is how the receiver stores a point's exemplars (see
// exemplarsToJSON): an array of {time_unix_nano, trace_id, span_id, value,
// filtered_attributes}. Only the fields a caller can follow are read.
const exemplarsJSONShape = `[{"time_unix_nano":"UBIGINT","trace_id":"VARCHAR","span_id":"VARCHAR","value":"DOUBLE"}]`

// queryExemplars is aggregation=exemplars: the concrete requests the SDK
// attached to the metric's points, slowest (highest value) first. This is the
// step from "p99 is 779ms" to "this trace was one of them".
//
// A cumulative series may repeat an exemplar in several exports, so each
// (trace_id, span_id) is returned once. Exemplars without a trace id (taken
// outside a sampled span) cannot be followed and are skipped.
func (c *Core) queryExemplars(groupExprs, aliases []string, conds string, params []any, cutoff time.Time, opts MetricOpts) ([]map[string]any, []string, error) {
	if err := opts.Value.validate("value"); err != nil {
		return nil, nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}

	gsel, gout := "", ""
	for i, e := range groupExprs {
		gsel += fmt.Sprintf("%s AS g%d, ", e, i+1)
		gout += fmt.Sprintf(`g%d AS "%s", `, i+1, aliases[i])
	}

	vc, vp := opts.Value.conds("e.value")
	where := []string{"e.trace_id IS NOT NULL", "e.trace_id <> ''", "e.value IS NOT NULL"}
	where = append(where, vc...)

	q := `WITH ex AS (
	SELECT ` + gsel + `unnest(from_json(exemplars, '` + exemplarsJSONShape + `')) AS e
	FROM metrics
	WHERE ` + conds + ` AND exemplars IS NOT NULL AND exemplars NOT IN ('', '[]')
)
SELECT ` + gout + `CAST(e.time_unix_nano // 1000 AS BIGINT) AS timestamp,
       e.value AS value, e.trace_id AS trace_id, e.span_id AS span_id
FROM ex
WHERE ` + strings.Join(where, " AND ") + `
QUALIFY row_number() OVER (PARTITION BY e.trace_id, e.span_id ORDER BY e.time_unix_nano DESC) = 1
ORDER BY value DESC, timestamp DESC
LIMIT ?`

	args := append(append(append([]any{}, params...), vp...), limit)
	rows, _, err := c.engine.QuerySince(cutoff, q, args...)
	if err != nil {
		return nil, nil, err
	}
	cols := append(append([]string{}, aliases...), "timestamp", "value", "trace_id", "span_id")
	return rows, cols, nil
}
