package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/telemetry"
)

// windowPolicy describes how a saved tool accepts time-window overrides on run.
type windowPolicy int

const (
	windowNone windowPolicy = iota
	windowSinceOnly
	windowSinceUntil
)

// savableTool wires strict decode, validation, dispatch, and window policy for one MCP tool.
type savableTool struct {
	window   windowPolicy
	decode   func([]byte) (any, error)
	validate func(s *Server, args any) error
	invoke   func(s *Server, ctx context.Context, req *sdkmcp.CallToolRequest, args any) (*sdkmcp.CallToolResult, any, error)
}

func strictDecode[T any](data []byte) (any, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func attrFiltersFrom(in []attrFilter) []telemetry.AttrFilter {
	out := make([]telemetry.AttrFilter, len(in))
	for i, f := range in {
		out[i] = telemetry.AttrFilter{Key: f.Key, Value: f.Value, Op: f.Op}
	}
	return out
}

// savableTools is the allowlist of MCP tools save_query may persist.
// sql_query is one entry here beside Register.
var savableTools = map[string]savableTool{
	"list_services": {
		window: windowNone,
		decode: func(b []byte) (any, error) { return strictDecode[listServicesArgs](b) },
		validate: func(s *Server, args any) error {
			a := args.(listServicesArgs)
			switch a.Source {
			case "", "traces":
				_, _, err := s.core.ListServices(a.Tenant)
				return err
			case "logs":
				_, _, err := s.core.ListLogServices(a.Tenant)
				return err
			default:
				return fmt.Errorf("source %q is not supported; use \"traces\" or \"logs\", or omit it for traces", a.Source)
			}
		},
		invoke: func(s *Server, ctx context.Context, req *sdkmcp.CallToolRequest, args any) (*sdkmcp.CallToolResult, any, error) {
			return s.listServices(ctx, req, args.(listServicesArgs))
		},
	},
	"list_metric_names": {
		window: windowNone,
		decode: func(b []byte) (any, error) { return strictDecode[listMetricNamesArgs](b) },
		validate: func(s *Server, args any) error {
			a := args.(listMetricNamesArgs)
			_, _, err := s.core.ListMetricNames(a.Tenant)
			return err
		},
		invoke: func(s *Server, ctx context.Context, req *sdkmcp.CallToolRequest, args any) (*sdkmcp.CallToolResult, any, error) {
			return s.listMetricNames(ctx, req, args.(listMetricNamesArgs))
		},
	},
	"list_attribute_keys": {
		window: windowSinceOnly,
		decode: func(b []byte) (any, error) { return strictDecode[listAttributeKeysArgs](b) },
		validate: func(s *Server, args any) error {
			a := args.(listAttributeKeysArgs)
			_, _, err := s.core.ListAttributeKeys(a.Tenant, a.Signal, a.ServiceName, a.SinceMinutes, a.Limit)
			return err
		},
		invoke: func(s *Server, ctx context.Context, req *sdkmcp.CallToolRequest, args any) (*sdkmcp.CallToolResult, any, error) {
			return s.listAttributeKeys(ctx, req, args.(listAttributeKeysArgs))
		},
	},
	"trace_lookup": {
		window: windowNone,
		decode: func(b []byte) (any, error) { return strictDecode[traceLookupArgs](b) },
		validate: func(s *Server, args any) error {
			a := args.(traceLookupArgs)
			switch a.Detail {
			case "", "summary":
				_, err := s.core.SummarizeTrace(a.TraceID, a.Tenant)
				return err
			case "spans":
				_, _, err := s.core.LookupTrace(a.TraceID, a.Tenant, a.Limit)
				return err
			default:
				return fmt.Errorf("detail %q is not supported; use \"summary\" (the default) for the compact summary, or \"spans\" for every span with attributes", a.Detail)
			}
		},
		invoke: func(s *Server, ctx context.Context, req *sdkmcp.CallToolRequest, args any) (*sdkmcp.CallToolResult, any, error) {
			return s.traceLookup(ctx, req, args.(traceLookupArgs))
		},
	},
	"span_search": {
		window: windowSinceUntil,
		decode: func(b []byte) (any, error) { return strictDecode[spanSearchArgs](b) },
		validate: func(s *Server, args any) error {
			a := args.(spanSearchArgs)
			filters := attrFiltersFrom(a.Filters)
			_, _, err := s.core.SearchSpansIn(a.Tenant, a.ServiceName, filters, a.SinceMinutes, a.UntilMinutes, a.Limit,
				telemetry.Range{Min: a.MinMs, Max: a.MaxMs})
			return err
		},
		invoke: func(s *Server, ctx context.Context, req *sdkmcp.CallToolRequest, args any) (*sdkmcp.CallToolResult, any, error) {
			return s.spanSearch(ctx, req, args.(spanSearchArgs))
		},
	},
	"compare_spans": {
		window: windowSinceUntil,
		decode: func(b []byte) (any, error) { return strictDecode[compareSpansArgs](b) },
		validate: func(s *Server, args any) error {
			a := args.(compareSpansArgs)
			filters := attrFiltersFrom(a.Filters)
			_, err := s.core.CompareSpans(a.Tenant, telemetry.CompareOpts{
				ServiceName: a.ServiceName,
				SpanName:    a.SpanName,
				SpanKind:    a.SpanKind,
				Filters:     filters,
				Since:       a.SinceMinutes,
				Until:       a.UntilMinutes,
				Slow:        telemetry.Range{Min: a.MinMs, Max: a.MaxMs},
				Limit:       a.Limit,
			})
			return err
		},
		invoke: func(s *Server, ctx context.Context, req *sdkmcp.CallToolRequest, args any) (*sdkmcp.CallToolResult, any, error) {
			return s.compareSpans(ctx, req, args.(compareSpansArgs))
		},
	},
	"log_search": {
		window: windowSinceUntil,
		decode: func(b []byte) (any, error) { return strictDecode[logSearchArgs](b) },
		validate: func(s *Server, args any) error {
			a := args.(logSearchArgs)
			filters := attrFiltersFrom(a.Filters)
			_, _, err := s.core.SearchLogs(a.Tenant, a.TraceID, a.ServiceName, a.Severity, a.Search, filters, a.SinceMinutes, a.UntilMinutes, a.Limit)
			return err
		},
		invoke: func(s *Server, ctx context.Context, req *sdkmcp.CallToolRequest, args any) (*sdkmcp.CallToolResult, any, error) {
			return s.logSearch(ctx, req, args.(logSearchArgs))
		},
	},
	"slow_requests": {
		window: windowSinceUntil,
		decode: func(b []byte) (any, error) { return strictDecode[slowRequestsArgs](b) },
		validate: func(s *Server, args any) error {
			a := args.(slowRequestsArgs)
			filters := attrFiltersFrom(a.Filters)
			_, err := s.core.SlowRequests(a.Tenant, telemetry.SlowOpts{
				MetricName:  a.MetricName,
				ServiceName: a.ServiceName,
				SpanName:    a.SpanName,
				Filters:     filters,
				Since:       a.SinceMinutes,
				Until:       a.UntilMinutes,
				Value:       telemetry.Range{Min: a.MinValue, Max: a.MaxValue},
				Limit:       a.Limit,
			})
			return err
		},
		invoke: func(s *Server, ctx context.Context, req *sdkmcp.CallToolRequest, args any) (*sdkmcp.CallToolResult, any, error) {
			return s.slowRequests(ctx, req, args.(slowRequestsArgs))
		},
	},
	"sql_query": {
		window: windowNone,
		decode: func(b []byte) (any, error) { return strictDecode[sqlQueryArgs](b) },
		validate: func(s *Server, args any) error {
			a := args.(sqlQueryArgs)
			_, _, err := s.core.SQLQuery(context.Background(), a.Tenant, a.SQL, a.Limit)
			return err
		},
		invoke: func(s *Server, ctx context.Context, req *sdkmcp.CallToolRequest, args any) (*sdkmcp.CallToolResult, any, error) {
			return s.sqlQuery(ctx, req, args.(sqlQueryArgs))
		},
	},
	"metric_query": {
		window: windowSinceUntil,
		decode: func(b []byte) (any, error) { return strictDecode[metricQueryArgs](b) },
		validate: func(s *Server, args any) error {
			a := args.(metricQueryArgs)
			filters := attrFiltersFrom(a.Filters)
			_, _, err := s.core.QueryMetricWith(a.Tenant, a.MetricName, a.Aggregation, a.SinceMinutes, a.GroupBy, filters,
				telemetry.MetricOpts{Value: telemetry.Range{Min: a.MinValue, Max: a.MaxValue}, Limit: a.Limit, Step: a.Step, Until: a.UntilMinutes})
			return err
		},
		invoke: func(s *Server, ctx context.Context, req *sdkmcp.CallToolRequest, args any) (*sdkmcp.CallToolResult, any, error) {
			return s.metricQuery(ctx, req, args.(metricQueryArgs))
		},
	},
}

func isSavableTool(name string) bool {
	_, ok := savableTools[name]
	return ok
}

func savableToolList() string {
	names := make([]string, 0, len(savableTools))
	for name := range savableTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (s *Server) decodeAndValidateTool(tool string, argsJSON []byte) (json.RawMessage, error) {
	entry, ok := savableTools[tool]
	if !ok {
		return nil, fmt.Errorf("tool %q cannot be saved. Savable tools: %s", tool, savableToolList())
	}
	decoded, err := entry.decode(argsJSON)
	if err != nil {
		return nil, err
	}
	if err := entry.validate(s, decoded); err != nil {
		return nil, err
	}
	// Store canonical compact JSON matching the decoded struct.
	out, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("encoding arguments: %w", err)
	}
	return out, nil
}

func (s *Server) invokeSavableTool(ctx context.Context, tool string, argsJSON []byte) (*sdkmcp.CallToolResult, error) {
	entry, ok := savableTools[tool]
	if !ok {
		return nil, fmt.Errorf("tool %q cannot be run from saved queries", tool)
	}
	decoded, err := entry.decode(argsJSON)
	if err != nil {
		return nil, err
	}
	res, _, err := entry.invoke(s, ctx, &sdkmcp.CallToolRequest{}, decoded)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func applyWindowOverride(policy windowPolicy, tool string, args map[string]any, since, until *int) (map[string]any, error) {
	if since == nil && until == nil {
		return args, nil
	}
	switch policy {
	case windowNone:
		return nil, fmt.Errorf("saved tool %q has no time window; since_minutes and until_minutes cannot be overridden", tool)
	case windowSinceOnly:
		if until != nil {
			return nil, fmt.Errorf("saved tool %q accepts since_minutes only; until_minutes cannot be overridden", tool)
		}
		if since != nil {
			out := copyArgs(args)
			out["since_minutes"] = *since
			return out, nil
		}
		return args, nil
	case windowSinceUntil:
		out := copyArgs(args)
		if since != nil {
			out["since_minutes"] = *since
		}
		if until != nil {
			out["until_minutes"] = *until
		}
		return out, nil
	default:
		return args, nil
	}
}

func copyArgs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
