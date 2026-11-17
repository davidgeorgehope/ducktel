// Package telemetry holds the query logic shared by every consumer of
// ducktel's stored data. It knows about traces, spans, metrics,
// attributes and tenants, and nothing about MCP or HTTP: those are transport
// concerns layered on top by their respective packages.
package telemetry

import (
	"sort"
	"strings"
)

// jsonPath builds a DuckDB JSON path for an attribute key.
//
// This exists because a DOTTED key must be quoted in the path: `$.service.name`
// means field "service" then field "name" and silently yields NULL, whereas
// `$."service.name"` reads the key literally. OTel semantic convention keys are
// dotted (service.name, http.request.method, tenant.id), so the unquoted form
// fails for the most common attributes in existence — with no error, just zero
// rows.
func jsonPath(key string) string {
	escaped := strings.ReplaceAll(key, `"`, `\"`)
	return `$."` + escaped + `"`
}

// knownColumns is the sortable/groupable allowlist. Identifiers cannot be bound
// as parameters, so anything interpolated into SQL is validated against this
// instead.
var knownColumns = []string{
	"service_name", "span_name", "span_kind", "status_code", "scope_name",
	"metric_name", "metric_type", "metric_unit", "severity_text", "trace_id", "span_id",
}

// quoteIdent validates a column name for interpolation into GROUP BY / ORDER BY.
// Returns false for anything not on the allowlist.
func quoteIdent(name string) (string, bool) {
	for _, c := range knownColumns {
		if c == name {
			return c, true
		}
	}
	return "", false
}

// knownColumnsList is the allowlist rendered for error messages.
func knownColumnsList() string {
	cols := append([]string(nil), knownColumns...)
	sort.Strings(cols)
	return strings.Join(cols, ", ")
}
