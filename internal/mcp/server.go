package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
)

// Server exposes ducktel's stored telemetry as MCP tools.
//
// Every tool takes a tenant and hard-filters on it, so a caller cannot widen
// its scope by omitting a parameter or by crafting a filter. The tools are
// generic: they operate on spans, attributes, metrics and time, and attach no
// meaning to any particular attribute key.
//
// The actual query logic — tenant isolation, SQL construction, the aggregation
// allowlist — lives in internal/telemetry, shared by every query consumer.
// This type only adapts MCP's argument/result shapes onto that shared core.
type Server struct {
	core    *telemetry.Core
	flusher *telemetry.Flusher
	// dashboardURL, when set, adds a dashboard link to every result.
	dashboardURL string
}

// NewServer wires the query engine into an MCP server. An empty flushURL
// disables the flush tool.
func NewServer(engine *query.Engine, flushURL, flushToken string) *Server {
	return &Server{
		core:    telemetry.NewCore(engine),
		flusher: telemetry.NewFlusher(flushURL, flushToken),
	}
}

// Register adds the tools to an MCP server.
func (s *Server) Register(srv *sdkmcp.Server) {
	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "trace_lookup",
		Description: "Look up one trace. By default (detail=summary) this returns a compact, already-aggregated summary: " +
			"the root span, span count, duration, services, repeated span groups with total_ms and self_ms " +
			"(time not spent in child spans, so groups add up), untraced_ms (root time no child span covers, " +
			"i.e. uninstrumented code), the slowest spans, error spans, and WARN-and-above log records. " +
			"Use this first to understand a request without spending context on every span. " +
			"detail=spans returns every span ordered by start time. Attributes are included unless " +
			"include_attributes is false. The result can be large: " +
			"use it only when the summary is not enough. Requires the tenant the trace belongs to.",
	}, s.traceLookup)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "span_search",
		Description: "Find spans matching attribute filters within a time range. " +
			"Use this to locate spans by any attribute key/value pair — for example " +
			"service.name, http.request.method, or any custom attribute. Returns matching " +
			"spans with timing and status, capped at limit results. " +
			"Attributes are omitted unless include_attributes is true. " +
			"min_ms/max_ms keep spans by duration, e.g. the requests of one histogram bucket.",
	}, s.spanSearch)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "compare_spans",
		Description: "Why are the slow spans different? Compares spans in a duration range (default: " +
			"slower than the scope's p95) with the rest of the same scope (service, span name, attribute " +
			"filters, time window) and returns the attribute values whose share differs most, biggest " +
			"difference first: e.g. cache=miss in 90% of slow spans vs 5% of the rest. " +
			"One call; follow up with slow_requests or span_search narrowed by the top value.",
	}, s.compareSpans)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "log_search",
		Description: "Find log records matching filters within a time range, newest first. " +
			"Use this for what a service actually logged — error messages, warnings, " +
			"failure detail. Filter by service, exact severity (case-insensitive), a " +
			"substring of the body, or attribute key/value pairs. Records that carry " +
			"trace_id/span_id can be joined to spans to see a failure's log line next " +
			"to its trace.",
	}, s.logSearch)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "slow_requests",
		Description: "The slowest requests in one call, each with a brief trace summary " +
			"(root, span count, untraced_ms, top repeated span groups with self_ms, the 3 slowest spans, " +
			"errors, first WARN-or-above log). " +
			"This is the first call for \"why is X slow\": with metric_name it follows that " +
			"metric's exemplars (min_value/max_value are in the metric's unit, for example one " +
			"histogram bucket), otherwise it returns the slowest root spans (min_value/max_value " +
			"in milliseconds). Follow a trace_id with trace_lookup for its full summary, or " +
			"trace_lookup(detail=spans) for every span.",
	}, s.slowRequests)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "metric_query",
		Description: "Aggregate a metric over a time range, optionally grouped by a column. " +
			"Use this for numeric questions — request rates, resource utilisation, " +
			"min/avg/max of a recorded value — rather than fetching raw points. " +
			"Supports avg, sum, min, max, count and percentiles. " +
			"Histograms (e.g. http.server.duration from the standard OTel " +
			"instrumentations) are aggregated over observations: count and sum of " +
			"observations, avg = sum/count, min/max as reported, and p50/p95/p99 " +
			"interpolated from the buckets, and aggregation=buckets for the " +
			"distribution itself: one row per bucket with le (inclusive upper bound, " +
			"null above the last bound) and value (observations) — the last two for " +
			"explicit-bucket histograms only; " +
			"cumulative series are turned into increments within the window. " +
			"aggregation=exemplars returns the concrete requests senders attached to " +
			"the points (timestamp, value, trace_id, span_id), highest value first; " +
			"narrow with min_value/max_value (e.g. one bucket) and follow a trace_id " +
			"with trace_lookup and log_search. " +
			"For gauge and sum points, count counts exported POINTS, not events: a " +
			"gauge re-exported every interval is counted once per export, so use a " +
			"counter or a histogram to count events.",
	}, s.metricQuery)

	// Only offered when a flush endpoint is configured: without one the tool
	// could not do anything, and advertising a tool that always fails would
	// waste a caller's attempt and muddy tool selection.
	if s.flusher.Enabled() {
		sdkmcp.AddTool(srv, &sdkmcp.Tool{
			Name: "flush_buffer",
			Description: "Make recently received telemetry queryable immediately. " +
				"This server reads telemetry from files that the receiver writes every " +
				"flush interval, so data sent in the last interval is not yet visible. " +
				"Call this when a query returns nothing for data you just sent, then " +
				"repeat the query. Accepts no arguments.",
		}, s.flushBuffer)
	}
}

// --- flush_buffer ---

type flushArgs struct{}

// flushBuffer asks the writing process to persist its buffered records.
//
// It authenticates with this server's own token, not the ingest token: the
// query side is allowed to request a flush but must never be able to write
// telemetry, so the two secrets stay separate.
func (s *Server) flushBuffer(ctx context.Context, req *sdkmcp.CallToolRequest, args flushArgs) (*sdkmcp.CallToolResult, any, error) {
	if err := s.flusher.Flush(ctx); err != nil {
		return errResult(err.Error())
	}

	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{
			Text: `{"status":"flushed","note":"buffered telemetry is now on disk; repeat your query"}`,
		}},
	}, nil, nil
}

// --- trace_lookup ---

type traceLookupArgs struct {
	TraceID           string `json:"trace_id" jsonschema:"the trace id to retrieve, as 32 lowercase hex characters"`
	Tenant            string `json:"tenant" jsonschema:"the tenant this trace belongs to; only data for this tenant is returned"`
	Detail            string `json:"detail,omitempty" jsonschema:"summary (default) returns a compact aggregated summary; spans returns every span ordered by start time, with attributes unless include_attributes is false"`
	Limit             int    `json:"limit,omitempty" jsonschema:"maximum spans to return for detail=spans; default 1000"`
	IncludeAttributes *bool  `json:"include_attributes,omitempty" jsonschema:"for detail=spans, set false to omit the attributes JSON on every span. Default true, the full span detail. Ignored for summary."`
}

func (s *Server) traceLookup(ctx context.Context, req *sdkmcp.CallToolRequest, args traceLookupArgs) (*sdkmcp.CallToolResult, any, error) {
	tenant := args.Tenant
	switch args.Detail {
	case "", "summary":
		summary, err := s.core.SummarizeTrace(args.TraceID, tenant)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(summary, s.traceLink(tenant, args.TraceID))
	case "spans":
		rows, cols, err := s.core.LookupTrace(args.TraceID, tenant, args.Limit)
		if err != nil {
			return errResult(err.Error())
		}
		if !askedFor(args.IncludeAttributes, true) {
			rows, cols = omitColumn(rows, cols, "attributes")
		}
		return withLink(rows, cols, s.traceLink(tenant, args.TraceID))
	default:
		return errResult(fmt.Sprintf("detail %q is not supported; use \"summary\" (the default) for the compact summary, or \"spans\" for every span with attributes", args.Detail))
	}
}

// --- span_search ---

type attrFilter struct {
	Key   string `json:"key" jsonschema:"attribute key to match, e.g. service.name or http.request.method. Dotted keys are handled correctly."`
	Value string `json:"value" jsonschema:"value to match for this key; with op != the record is excluded when it has this value"`
	Op    string `json:"op,omitempty" jsonschema:"match operator: = (default) or != (exclude records where the key has this value; records without the key are kept)"`
}

type spanSearchArgs struct {
	Tenant            string       `json:"tenant" jsonschema:"the tenant to query; only data for this tenant is returned"`
	Filters           []attrFilter `json:"filters,omitempty" jsonschema:"attribute key/value pairs to match (ANDed together); span_name, status_code and span_kind match the span's own fields, e.g. {\"key\":\"span_name\",\"value\":\"POST /mcp\",\"op\":\"!=\"}. Omit to match all spans in the time range."`
	ServiceName       string       `json:"service_name,omitempty" jsonschema:"restrict to one service name"`
	SinceMinutes      int          `json:"since_minutes,omitempty" jsonschema:"how far back to search, in minutes. Default 60."`
	UntilMinutes      int          `json:"until_minutes,omitempty" jsonschema:"where the window ends, in minutes before now. Default 0 (now). Must be less than since_minutes."`
	Limit             int          `json:"limit,omitempty" jsonschema:"maximum spans to return, default 100. Cap 1000."`
	IncludeAttributes *bool        `json:"include_attributes,omitempty" jsonschema:"when true, each row includes the attributes JSON. Default false."`
	MinMs             *float64     `json:"min_ms,omitempty" jsonschema:"only spans lasting at least this many milliseconds (inclusive)"`
	MaxMs             *float64     `json:"max_ms,omitempty" jsonschema:"only spans lasting at most this many milliseconds (inclusive). With min_ms, e.g. one histogram bucket's bounds."`
}

func (s *Server) spanSearch(ctx context.Context, req *sdkmcp.CallToolRequest, args spanSearchArgs) (*sdkmcp.CallToolResult, any, error) {
	filters := make([]telemetry.AttrFilter, len(args.Filters))
	for i, f := range args.Filters {
		filters[i] = telemetry.AttrFilter{Key: f.Key, Value: f.Value, Op: f.Op}
	}

	tenant := args.Tenant
	rows, cols, err := s.core.SearchSpansIn(tenant, args.ServiceName, filters, args.SinceMinutes, args.UntilMinutes, args.Limit,
		telemetry.Range{Min: args.MinMs, Max: args.MaxMs})
	if err != nil {
		return errResult(err.Error())
	}
	if !askedFor(args.IncludeAttributes, false) {
		rows, cols = omitColumn(rows, cols, "attributes")
	}
	return withLink(rows, cols, s.spanLink(tenant, args))
}

// --- compare_spans ---

type compareSpansArgs struct {
	Tenant       string       `json:"tenant" jsonschema:"the tenant to query; only data for this tenant is returned"`
	ServiceName  string       `json:"service_name,omitempty" jsonschema:"restrict to one service name"`
	SpanName     string       `json:"span_name,omitempty" jsonschema:"optional exact span_name to match"`
	SpanKind     string       `json:"span_kind,omitempty" jsonschema:"which span kind to compare: server (default when span_name is not given), client, internal, producer, consumer, or any"`
	Filters      []attrFilter `json:"filters,omitempty" jsonschema:"attribute key/value pairs to match (ANDed together), against span attributes or resource attributes; span_name, status_code and span_kind match the span's own fields, e.g. {\"key\":\"span_name\",\"value\":\"POST /mcp\",\"op\":\"!=\"}. Omit to compare all spans in the time range."`
	SinceMinutes int          `json:"since_minutes,omitempty" jsonschema:"how far back to look, in minutes. Default 60."`
	UntilMinutes int          `json:"until_minutes,omitempty" jsonschema:"where the window ends, in minutes before now. Default 0 (now). Must be less than since_minutes."`
	MinMs        *float64     `json:"min_ms,omitempty" jsonschema:"the slow group is spans lasting at least this many milliseconds (inclusive). Omit min_ms and max_ms to use the scope's p95 duration instead."`
	MaxMs        *float64     `json:"max_ms,omitempty" jsonschema:"the slow group is spans lasting at most this many milliseconds (inclusive). With min_ms, e.g. one histogram bucket's bounds; omit both to use the scope's p95."`
	Limit        int          `json:"limit,omitempty" jsonschema:"maximum comparison rows to return, default 15, cap 50"`
}

func (s *Server) compareSpans(ctx context.Context, req *sdkmcp.CallToolRequest, args compareSpansArgs) (*sdkmcp.CallToolResult, any, error) {
	filters := make([]telemetry.AttrFilter, len(args.Filters))
	for i, f := range args.Filters {
		filters[i] = telemetry.AttrFilter{Key: f.Key, Value: f.Value, Op: f.Op}
	}

	tenant := args.Tenant
	cmp, err := s.core.CompareSpans(tenant, telemetry.CompareOpts{
		ServiceName: args.ServiceName,
		SpanName:    args.SpanName,
		SpanKind:    args.SpanKind,
		Filters:     filters,
		Since:       args.SinceMinutes,
		Until:       args.UntilMinutes,
		Slow:        telemetry.Range{Min: args.MinMs, Max: args.MaxMs},
		Limit:       args.Limit,
	})
	if err != nil {
		return errResult(err.Error())
	}
	return jsonResult(cmp, s.compareLink(tenant, args))
}

// --- log_search ---

type logSearchArgs struct {
	Tenant       string       `json:"tenant" jsonschema:"the tenant to query; only data for this tenant is returned"`
	TraceID      string       `json:"trace_id,omitempty" jsonschema:"restrict to the records of one trace id. A trace id is a precise key, so this searches the whole history unless since_minutes is given."`
	ServiceName  string       `json:"service_name,omitempty" jsonschema:"restrict to one service name"`
	Severity     string       `json:"severity,omitempty" jsonschema:"exact severity to match, case-insensitive: DEBUG, INFO, WARN, ERROR, FATAL. Matches the severity text senders set."`
	Search       string       `json:"search,omitempty" jsonschema:"case-insensitive substring to find in the log body"`
	Filters      []attrFilter `json:"filters,omitempty" jsonschema:"attribute key/value pairs to match (ANDed together), against record attributes OR resource attributes. Omit to match all records in the time range."`
	SinceMinutes int          `json:"since_minutes,omitempty" jsonschema:"how far back to search, in minutes. Default 60 (whole history when trace_id is given)."`
	UntilMinutes int          `json:"until_minutes,omitempty" jsonschema:"where the window ends, in minutes before now. Default 0 (now). Must be less than since_minutes."`
	Limit        int          `json:"limit,omitempty" jsonschema:"maximum log records to return, default 100. Cap 1000."`
}

func (s *Server) logSearch(ctx context.Context, req *sdkmcp.CallToolRequest, args logSearchArgs) (*sdkmcp.CallToolResult, any, error) {
	filters := make([]telemetry.AttrFilter, len(args.Filters))
	for i, f := range args.Filters {
		filters[i] = telemetry.AttrFilter{Key: f.Key, Value: f.Value, Op: f.Op}
	}

	rows, cols, err := s.core.SearchLogs(args.Tenant, args.TraceID, args.ServiceName, args.Severity, args.Search, filters, args.SinceMinutes, args.UntilMinutes, args.Limit)
	if err != nil {
		return errResult(err.Error())
	}
	return withLink(rows, cols, s.logLink(args.Tenant, args))
}

// --- slow_requests ---

type slowRequestsArgs struct {
	Tenant       string       `json:"tenant" jsonschema:"the tenant to query; only data for this tenant is returned"`
	MetricName   string       `json:"metric_name,omitempty" jsonschema:"when set, follow this metric's exemplars. Omit to use the slowest root spans instead."`
	ServiceName  string       `json:"service_name,omitempty" jsonschema:"restrict to one service name"`
	SpanName     string       `json:"span_name,omitempty" jsonschema:"spans source only: exact span_name to match"`
	Filters      []attrFilter `json:"filters,omitempty" jsonschema:"attribute key/value pairs to match (ANDed together); on the spans source span_name, status_code and span_kind match the span's own fields, e.g. {\"key\":\"span_name\",\"value\":\"POST /mcp\",\"op\":\"!=\"}"`
	SinceMinutes int          `json:"since_minutes,omitempty" jsonschema:"how far back to look, in minutes. Default 60."`
	UntilMinutes int          `json:"until_minutes,omitempty" jsonschema:"where the window ends, in minutes before now. Default 0 (now). Must be less than since_minutes."`
	MinValue     *float64     `json:"min_value,omitempty" jsonschema:"with metric_name: keep exemplars at least this value in the metric's unit; without it: keep spans lasting at least this many milliseconds"`
	MaxValue     *float64     `json:"max_value,omitempty" jsonschema:"with metric_name: keep exemplars at most this value in the metric's unit; without it: keep spans lasting at most this many milliseconds"`
	Limit        int          `json:"limit,omitempty" jsonschema:"maximum requests to return, default 5, cap 20"`
}

func (s *Server) slowRequests(ctx context.Context, req *sdkmcp.CallToolRequest, args slowRequestsArgs) (*sdkmcp.CallToolResult, any, error) {
	filters := make([]telemetry.AttrFilter, len(args.Filters))
	for i, f := range args.Filters {
		filters[i] = telemetry.AttrFilter{Key: f.Key, Value: f.Value, Op: f.Op}
	}

	tenant := args.Tenant
	rows, err := s.core.SlowRequests(tenant, telemetry.SlowOpts{
		MetricName:  args.MetricName,
		ServiceName: args.ServiceName,
		SpanName:    args.SpanName,
		Filters:     filters,
		Since:       args.SinceMinutes,
		Until:       args.UntilMinutes,
		Value:       telemetry.Range{Min: args.MinValue, Max: args.MaxValue},
		Limit:       args.Limit,
	})
	if err != nil {
		return errResult(err.Error())
	}
	return jsonResult(rows, s.slowLink(tenant, args))
}

// --- metric_query ---

type metricQueryArgs struct {
	Tenant       string       `json:"tenant" jsonschema:"the tenant to query; only data for this tenant is returned"`
	MetricName   string       `json:"metric_name" jsonschema:"the metric name to aggregate, e.g. process.cpu.utilization"`
	Aggregation  string       `json:"aggregation" jsonschema:"one of: avg, sum, min, max, count, p50, p95, p99, buckets (histograms only), exemplars"`
	SinceMinutes int          `json:"since_minutes,omitempty" jsonschema:"how far back to aggregate, in minutes. Default 60."`
	UntilMinutes int          `json:"until_minutes,omitempty" jsonschema:"where the window ends, in minutes before now. Default 0 (now). Must be less than since_minutes."`
	GroupBy      string       `json:"group_by,omitempty" jsonschema:"optional comma-separated grouping keys: a column name (service_name, metric_type, ...), a data-point attribute as attr.<key> / attributes.<key>, or a resource attribute as resource.<key> / resource_attributes.<key>, e.g. resource.host.name groups by the resource attributes JSON key 'host.name'. Result rows carry the key as their column name."`
	Step         string       `json:"step,omitempty" jsonschema:"optional time-series step as a duration (30s, 1m, 5m, 1h) or a whole number of seconds. Adds a leading time column (bucket start in Unix microseconds, UTC) and one row per time bucket and group, ordered by time then groups. At most 1500 buckets per query; exemplars ignores step."`
	Filters      []attrFilter `json:"filters,omitempty" jsonschema:"attribute key/value pairs to match, ANDed together; matched against data-point attributes OR resource attributes, same as span_search filters. Dotted keys are handled correctly."`
	MinValue     *float64     `json:"min_value,omitempty" jsonschema:"exemplars only: keep exemplars whose value is at least this (inclusive)"`
	MaxValue     *float64     `json:"max_value,omitempty" jsonschema:"exemplars only: keep exemplars whose value is at most this (inclusive); with min_value, e.g. one bucket's bounds"`
	Limit        int          `json:"limit,omitempty" jsonschema:"exemplars only: maximum rows, default 20, cap 200"`
}

func (s *Server) metricQuery(ctx context.Context, req *sdkmcp.CallToolRequest, args metricQueryArgs) (*sdkmcp.CallToolResult, any, error) {
	filters := make([]telemetry.AttrFilter, len(args.Filters))
	for i, f := range args.Filters {
		filters[i] = telemetry.AttrFilter{Key: f.Key, Value: f.Value, Op: f.Op}
	}

	tenant := args.Tenant
	rows, cols, err := s.core.QueryMetricWith(tenant, args.MetricName, args.Aggregation, args.SinceMinutes, args.GroupBy, filters,
		telemetry.MetricOpts{Value: telemetry.Range{Min: args.MinValue, Max: args.MaxValue}, Limit: args.Limit, Step: args.Step, Until: args.UntilMinutes})
	if err != nil {
		return errResult(err.Error())
	}
	return withLink(rows, cols, s.metricLink(tenant, args))
}

// --- shared helpers ---

func errResult(msg string) (*sdkmcp.CallToolResult, any, error) {
	return &sdkmcp.CallToolResult{
		IsError: true,
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: msg}},
	}, nil, nil
}

// withLink is rowsResult plus the dashboard link, when links are on.
func withLink(rows []map[string]any, cols []string, link string) (*sdkmcp.CallToolResult, any, error) {
	res, _, err := rowsResult(rows, cols)
	if err != nil {
		return res, nil, err
	}
	return linked(res, link)
}

// jsonResult is withLink for a JSON-marshalable object (a summary or a list of
// structs), rather than engine rows with a separate column order.
func jsonResult(v any, link string) (*sdkmcp.CallToolResult, any, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return errResult(fmt.Sprintf("encoding results: %v", err))
	}
	res := &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(out)}},
	}
	return linked(res, link)
}

func askedFor(flag *bool, def bool) bool {
	if flag == nil {
		return def
	}
	return *flag
}

func omitColumn(rows []map[string]any, cols []string, name string) ([]map[string]any, []string) {
	kept := make([]string, 0, len(cols))
	for _, c := range cols {
		if c != name {
			kept = append(kept, c)
		}
	}
	for _, row := range rows {
		delete(row, name)
	}
	return rows, kept
}

func rowsResult(rows []map[string]any, cols []string) (*sdkmcp.CallToolResult, any, error) {
	out, err := telemetry.MarshalRows(rows, cols)
	if err != nil {
		return errResult(fmt.Sprintf("encoding results: %v", err))
	}
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: out}},
	}, nil, nil
}
