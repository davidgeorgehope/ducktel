package telemetry

import (
	"strings"
	"time"
)

// SlowRequest is one slow request, already carrying a brief trace summary so a
// caller does not have to follow up with a trace lookup just to see the shape
// of it. Value and Unit say what "slow" was measured in: the exemplar value in
// the metric's own unit, or the root span's duration in milliseconds.
type SlowRequest struct {
	TraceID   string       `json:"trace_id"`
	SpanID    string       `json:"span_id,omitempty"`
	Timestamp int64        `json:"timestamp"` // microseconds
	Value     float64      `json:"value"`     // exemplar value in the metric's unit, or span duration_ms
	Unit      string       `json:"unit"`      // metric_unit of the metric, or "ms"
	Trace     TraceSummary `json:"trace"`     // BriefSummary
}

type SlowOpts struct {
	MetricName  string // when set: source = exemplars of this metric
	ServiceName string
	SpanName    string // spans source only: exact span_name
	Filters     []AttrFilter
	Since       int   // minutes, default 60
	Until       int   // minutes before now where the window ends; default 0 (now)
	Value       Range // metric: exemplar value; spans: duration_ms
	Limit       int   // default 5, cap 20
}

// SlowRequests answers "what is slow right now" in one call. With MetricName
// it follows that metric's exemplars (the concrete requests senders attached
// to slow observations); otherwise it lists the slowest root spans. Either way
// every result row carries a brief TraceSummary, built with ONE batch
// TraceSummaries call for all the trace ids.
func (c *Core) SlowRequests(tenant string, o SlowOpts) ([]SlowRequest, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, err
	}

	limit := o.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}

	win := Window{Since: o.Since, Until: o.Until}
	if err := win.validate(); err != nil {
		return nil, err
	}
	cutoffTime, endTime := win.bounds(time.Now())

	out := make([]SlowRequest, 0, limit)
	ids := make([]string, 0, limit)

	if o.MetricName != "" {
		filters := make([]AttrFilter, 0, len(o.Filters)+1)
		filters = append(filters, o.Filters...)
		if o.ServiceName != "" {
			// QueryMetricWith filters a metric by attributes or resource
			// attributes; service.name is a resource attribute, and also
			// promoted to the service_name column. Reuse that filter path
			// rather than adding a second service predicate to the SQL.
			filters = append(filters, AttrFilter{Key: "service.name", Value: o.ServiceName})
		}

		rows, _, err := c.QueryMetricWith(tenant, o.MetricName, "exemplars", o.Since, "", filters,
			MetricOpts{Value: o.Value, Limit: limit, Until: o.Until})
		if err != nil {
			return nil, err
		}

		unit, err := c.metricUnit(tenant, o.MetricName, cutoffTime)
		if err != nil {
			return nil, err
		}

		for _, r := range rows {
			value := toFloat(r["value"])
			if unit == "ms" {
				value = roundMs(value)
			}
			sr := SlowRequest{
				TraceID:   strVal(r["trace_id"]),
				SpanID:    strVal(r["span_id"]),
				Timestamp: intVal(r["timestamp"]),
				Value:     value,
				Unit:      unit,
			}
			out = append(out, sr)
			ids = append(ids, sr.TraceID)
		}
	} else {
		conds, params, err := c.spanConds(tenant, o.ServiceName, o.Filters, o.Value)
		if err != nil {
			return nil, err
		}
		conds = append([]string{"parent_span_id = ''", "start_time >= ?"}, conds...)
		params = append([]any{cutoffTime.UnixMicro()}, params...)
		if !endTime.IsZero() {
			conds = append(conds, "start_time < ?")
			params = append(params, endTime.UnixMicro())
		}
		if o.SpanName != "" {
			conds = append(conds, "span_name = ?")
			params = append(params, o.SpanName)
		}

		q := `SELECT trace_id, span_id, start_time, duration_ms
		      FROM traces
		      WHERE ` + strings.Join(conds, " AND ") + `
		      ORDER BY duration_ms DESC, start_time DESC, trace_id, span_id
		      LIMIT ?`
		rows, _, err := c.engine.QuerySince(cutoffTime, q, append(params, limit)...)
		if err != nil {
			return nil, err
		}

		for _, r := range rows {
			sr := SlowRequest{
				TraceID:   strVal(r["trace_id"]),
				SpanID:    strVal(r["span_id"]),
				Timestamp: intVal(r["start_time"]),
				Value:     roundMs(toFloat(r["duration_ms"])),
				Unit:      "ms",
			}
			out = append(out, sr)
			ids = append(ids, sr.TraceID)
		}
	}

	if len(out) == 0 {
		return out, nil
	}

	// A trace can start before its exemplar (the exemplar is stamped at
	// observation time), so widen the file window by an hour. The summaries
	// are one batch call for every id, not one per request.
	unique := uniqueStrings(ids)
	sums, err := c.TraceSummaries(tenant, unique, cutoffTime.Add(-time.Hour), BriefSummary)
	if err != nil {
		return nil, err
	}
	byTrace := make(map[string]TraceSummary, len(sums))
	for _, s := range sums {
		byTrace[s.TraceID] = s
	}
	for i := range out {
		out[i].Trace = byTrace[out[i].TraceID]
	}
	return out, nil
}

// metricUnit returns the metric's most recently reported unit, or "" when the
// metric has no points in the window (or no unit was set).
func (c *Core) metricUnit(tenant, metricName string, cutoff time.Time) (string, error) {
	q := `SELECT metric_unit FROM metrics
	      WHERE metric_name = ? AND ` + c.tenantPredicate() + `
	      ORDER BY timestamp DESC
	      LIMIT 1`
	rows, _, err := c.queryAt(cutoff, q, metricName, tenant)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return strVal(rows[0]["metric_unit"]), nil
}

func uniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
