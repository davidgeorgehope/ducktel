package telemetry

import (
	"time"

	"github.com/davidgeorgehope/ducktel/internal/feedback"
)

// ListFeedback returns stored agent feedback for one tenant, newest first.
// since is the lower bound. The zero time means no lower bound.
// The service is fixed to the feedback service.
func (c *Core) ListFeedback(tenant string, since time.Time, limit int) ([]map[string]any, []string, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}

	params := []any{
		jsonPath("report_id"),
		jsonPath("tool_name"),
		jsonPath("category"),
		jsonPath("severity"),
		jsonPath("summary"),
		jsonPath("details"),
		jsonPath("proposed_fix"),
		jsonPath("example_call"),
		tenant,
		feedback.ServiceName,
	}
	q := `SELECT timestamp,
	             json_extract_string(attributes, ?) AS report_id,
	             json_extract_string(attributes, ?) AS tool_name,
	             json_extract_string(attributes, ?) AS category,
	             json_extract_string(attributes, ?) AS severity,
	             json_extract_string(attributes, ?) AS summary,
	             json_extract_string(attributes, ?) AS details,
	             json_extract_string(attributes, ?) AS proposed_fix,
	             json_extract_string(attributes, ?) AS example_call
	      FROM logs
	      WHERE ` + c.tenantPredicate() + ` AND service_name = ?`
	if !since.IsZero() {
		q += ` AND timestamp >= ?`
		params = append(params, since.UnixMicro())
	}
	q += ` ORDER BY timestamp DESC LIMIT ?`
	params = append(params, limit)
	return c.engine.QuerySince(since, q, params...)
}
