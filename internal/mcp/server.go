package mcp

import (
	"context"
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
		Description: "Retrieve every span belonging to one trace, ordered by start time. " +
			"Use this to reconstruct the full call tree of a single request: it returns all " +
			"spans sharing the given trace id, with parent/child relationships, timing, status " +
			"and attributes. Requires the tenant the trace belongs to.",
	}, s.traceLookup)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "span_search",
		Description: "Find spans matching attribute filters within a time range. " +
			"Use this to locate spans by any attribute key/value pair — for example " +
			"service.name, http.request.method, or any custom attribute. Returns matching " +
			"spans with timing, status and attributes, capped at limit results.",
	}, s.spanSearch)

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
		Name: "metric_query",
		Description: "Aggregate a metric over a time range, optionally grouped by a column. " +
			"Use this for numeric questions — request rates, resource utilisation, " +
			"min/avg/max of a recorded value — rather than fetching raw points. " +
			"Supports avg, sum, min, max, count and percentiles. " +
			"IMPORTANT for senders: this aggregates the value_double column, which is " +
			"only populated for gauge and sum points. A histogram point stores its data " +
			"in sum/min/max/count/bucket_counts instead and is therefore INVISIBLE here " +
			"— it returns 0 rather than an error. Send a per-event duration or size as a " +
			"gauge or sum, not a histogram, or it will not be aggregatable.",
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
	TraceID string `json:"trace_id" jsonschema:"the trace id to retrieve, as 32 lowercase hex characters"`
	Tenant  string `json:"tenant" jsonschema:"the tenant this trace belongs to; only data for this tenant is returned"`
	Limit   int    `json:"limit,omitempty" jsonschema:"maximum spans to return, default 1000"`
}

func (s *Server) traceLookup(ctx context.Context, req *sdkmcp.CallToolRequest, args traceLookupArgs) (*sdkmcp.CallToolResult, any, error) {
	rows, cols, err := s.core.LookupTrace(args.TraceID, args.Tenant, args.Limit)
	if err != nil {
		return errResult(err.Error())
	}
	return rowsResult(rows, cols)
}

// --- span_search ---

type attrFilter struct {
	Key   string `json:"key" jsonschema:"attribute key to match, e.g. service.name or http.request.method. Dotted keys are handled correctly."`
	Value string `json:"value" jsonschema:"exact value to match for this key"`
}

type spanSearchArgs struct {
	Tenant       string       `json:"tenant" jsonschema:"the tenant to query; only data for this tenant is returned"`
	Filters      []attrFilter `json:"filters,omitempty" jsonschema:"attribute key/value pairs to match (ANDed together). Omit to match all spans in the time range."`
	ServiceName  string       `json:"service_name,omitempty" jsonschema:"restrict to one service name"`
	SinceMinutes int          `json:"since_minutes,omitempty" jsonschema:"how far back to search, in minutes. Default 60."`
	Limit        int          `json:"limit,omitempty" jsonschema:"maximum spans to return, default 100. Cap 1000."`
}

func (s *Server) spanSearch(ctx context.Context, req *sdkmcp.CallToolRequest, args spanSearchArgs) (*sdkmcp.CallToolResult, any, error) {
	filters := make([]telemetry.AttrFilter, len(args.Filters))
	for i, f := range args.Filters {
		filters[i] = telemetry.AttrFilter{Key: f.Key, Value: f.Value}
	}

	rows, cols, err := s.core.SearchSpans(args.Tenant, args.ServiceName, filters, args.SinceMinutes, args.Limit)
	if err != nil {
		return errResult(err.Error())
	}
	return rowsResult(rows, cols)
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
	Limit        int          `json:"limit,omitempty" jsonschema:"maximum log records to return, default 100. Cap 1000."`
}

func (s *Server) logSearch(ctx context.Context, req *sdkmcp.CallToolRequest, args logSearchArgs) (*sdkmcp.CallToolResult, any, error) {
	filters := make([]telemetry.AttrFilter, len(args.Filters))
	for i, f := range args.Filters {
		filters[i] = telemetry.AttrFilter{Key: f.Key, Value: f.Value}
	}

	rows, cols, err := s.core.SearchLogs(args.Tenant, args.TraceID, args.ServiceName, args.Severity, args.Search, filters, args.SinceMinutes, args.Limit)
	if err != nil {
		return errResult(err.Error())
	}
	return rowsResult(rows, cols)
}

// --- metric_query ---

type metricQueryArgs struct {
	Tenant       string       `json:"tenant" jsonschema:"the tenant to query; only data for this tenant is returned"`
	MetricName   string       `json:"metric_name" jsonschema:"the metric name to aggregate, e.g. process.cpu.utilization"`
	Aggregation  string       `json:"aggregation" jsonschema:"one of: avg, sum, min, max, count, p50, p95, p99"`
	SinceMinutes int          `json:"since_minutes,omitempty" jsonschema:"how far back to aggregate, in minutes. Default 60."`
	GroupBy      string       `json:"group_by,omitempty" jsonschema:"optional comma-separated grouping keys: a column name (service_name, metric_type, ...) or a data-point attribute as attr.<key> / attributes.<key>, e.g. attr.kind groups by the metric point's attributes JSON key 'kind'. Result rows carry the key as their column name."`
	Filters      []attrFilter `json:"filters,omitempty" jsonschema:"attribute key/value pairs to match, ANDed together; matched against data-point attributes OR resource attributes, same as span_search filters. Dotted keys are handled correctly."`
}

func (s *Server) metricQuery(ctx context.Context, req *sdkmcp.CallToolRequest, args metricQueryArgs) (*sdkmcp.CallToolResult, any, error) {
	filters := make([]telemetry.AttrFilter, len(args.Filters))
	for i, f := range args.Filters {
		filters[i] = telemetry.AttrFilter{Key: f.Key, Value: f.Value}
	}

	rows, cols, err := s.core.QueryMetric(args.Tenant, args.MetricName, args.Aggregation, args.SinceMinutes, args.GroupBy, filters)
	if err != nil {
		return errResult(err.Error())
	}
	return rowsResult(rows, cols)
}

// --- shared helpers ---

func errResult(msg string) (*sdkmcp.CallToolResult, any, error) {
	return &sdkmcp.CallToolResult{
		IsError: true,
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: msg}},
	}, nil, nil
}

func rowsResult(rows []map[string]any, cols []string) (*sdkmcp.CallToolResult, any, error) {
	// Return the rows as JSON text: unambiguous, and the shape the caller
	// expects from a query tool.
	out, err := telemetry.MarshalRows(rows, cols)
	if err != nil {
		return errResult(fmt.Sprintf("encoding results: %v", err))
	}
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: out}},
	}, nil, nil
}
