package telemetry

import "strconv"

// ErrorSpan is one stored error span, small enough to turn into an event.
// Tenant is the tenant.id resource attribute. An empty Tenant is not a tenant.
type ErrorSpan struct {
	Tenant        string
	TraceID       string
	SpanID        string
	Service       string
	SpanName      string
	StatusMessage string
	StartUS       int64
}

// LatestErrorSpan is the newest error span by (start_time, span_id).
// ok is false when the traces view has no error span.
func (c *Core) LatestErrorSpan() (startUS int64, spanID string, ok bool, err error) {
	if c == nil || c.engine == nil {
		return 0, "", false, nil
	}
	rows, _, err := c.engine.Query(`
		SELECT start_time, span_id
		FROM traces
		WHERE status_code = 'STATUS_CODE_ERROR'
		ORDER BY start_time DESC, span_id DESC
		LIMIT 1`)
	if err != nil {
		return 0, "", false, err
	}
	if len(rows) == 0 {
		return 0, "", false, nil
	}
	start, okStart := cellInt(rows[0]["start_time"])
	id := cellString(rows[0]["span_id"])
	if !okStart {
		return 0, "", false, nil
	}
	return start, id, true, nil
}

// ErrorSpansAfter returns error spans strictly after (startUS, spanID),
// oldest first, at most limit rows. The tenant column is the row's own
// tenant.id. The caller files each row under that tenant.
func (c *Core) ErrorSpansAfter(startUS int64, spanID string, limit int) ([]ErrorSpan, error) {
	if c == nil || c.engine == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	rows, _, err := c.engine.Query(`
		SELECT trace_id, span_id, service_name, span_name, status_message, start_time,
		       json_extract_string(resource_attributes, '$."tenant.id"') AS tenant_id
		FROM traces
		WHERE status_code = 'STATUS_CODE_ERROR'
		  AND (start_time > ? OR (start_time = ? AND span_id > ?))
		ORDER BY start_time, span_id
		LIMIT `+strconv.Itoa(limit), startUS, startUS, spanID)
	if err != nil {
		return nil, err
	}
	out := make([]ErrorSpan, 0, len(rows))
	for _, row := range rows {
		start, _ := cellInt(row["start_time"])
		out = append(out, ErrorSpan{
			Tenant:        cellString(row["tenant_id"]),
			TraceID:       cellString(row["trace_id"]),
			SpanID:        cellString(row["span_id"]),
			Service:       cellString(row["service_name"]),
			SpanName:      cellString(row["span_name"]),
			StatusMessage: cellString(row["status_message"]),
			StartUS:       start,
		})
	}
	return out, nil
}

func cellString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case nil:
		return ""
	default:
		return ""
	}
}

func cellInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int32:
		return int64(t), true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}
