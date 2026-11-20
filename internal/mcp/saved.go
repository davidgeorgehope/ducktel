package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/savedquery"
)

type listSavedQueriesArgs struct {
	Tenant string `json:"tenant" jsonschema:"tenant whose saved queries to list"`
}

func (s *Server) listSavedQueries(ctx context.Context, req *sdkmcp.CallToolRequest, args listSavedQueriesArgs) (*sdkmcp.CallToolResult, any, error) {
	if args.Tenant == "" {
		return errResult("tenant is required")
	}
	list, err := s.mcpSaved.List(args.Tenant)
	if err != nil {
		return errResult(err.Error())
	}
	type row struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Tool        string          `json:"tool"`
		Arguments   json.RawMessage `json:"arguments"`
	}
	out := make([]row, len(list))
	for i, q := range list {
		out[i] = row{Name: q.Name, Description: q.Description, Tool: q.Tool, Arguments: q.Arguments}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return errResult(fmt.Sprintf("encoding results: %v", err))
	}
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(data)}},
	}, nil, nil
}

type saveQueryArgs struct {
	Tenant      string         `json:"tenant" jsonschema:"tenant that owns this saved query"`
	Name        string         `json:"name" jsonschema:"unique name within the tenant"`
	Tool        string         `json:"tool" jsonschema:"MCP tool to invoke when the query is run"`
	Arguments   map[string]any `json:"arguments" jsonschema:"arguments passed to the tool"`
	Description string         `json:"description,omitempty" jsonschema:"optional description"`
}

func (s *Server) saveQuery(ctx context.Context, req *sdkmcp.CallToolRequest, args saveQueryArgs) (*sdkmcp.CallToolResult, any, error) {
	if args.Tenant == "" {
		return errResult("tenant is required")
	}
	if args.Name == "" {
		return errResult("name is required")
	}
	if args.Tool == "" {
		return errResult("tool is required")
	}
	if args.Arguments == nil {
		return errResult("arguments is required")
	}
	if !isSavableTool(args.Tool) {
		return errResult(fmt.Sprintf("tool %q cannot be saved. Savable tools: %s", args.Tool, savableToolList()))
	}
	argTenant, _ := args.Arguments["tenant"].(string)
	if argTenant != "" && argTenant != args.Tenant {
		return errResult(fmt.Sprintf("arguments.tenant %q does not match tenant %q", argTenant, args.Tenant))
	}
	args.Arguments["tenant"] = args.Tenant

	raw, err := json.Marshal(args.Arguments)
	if err != nil {
		return errResult(fmt.Sprintf("encoding arguments: %v", err))
	}
	canonical, err := s.decodeAndValidateTool(args.Tool, raw)
	if err != nil {
		return errResult(err.Error())
	}

	if err := s.mcpSaved.Save(savedquery.MCPSavedQuery{
		Tenant:      args.Tenant,
		Name:        args.Name,
		Description: args.Description,
		Tool:        args.Tool,
		Arguments:   canonical,
	}); err != nil {
		return errResult(err.Error())
	}

	out, _ := json.Marshal(map[string]string{"name": args.Name, "tool": args.Tool, "tenant": args.Tenant})
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(out)}},
	}, nil, nil
}

type runSavedQueryArgs struct {
	Tenant       string `json:"tenant" jsonschema:"tenant that owns the saved query"`
	Name         string `json:"name" jsonschema:"saved query name"`
	SinceMinutes *int   `json:"since_minutes,omitempty" jsonschema:"optional override for tools that accept since_minutes"`
	UntilMinutes *int   `json:"until_minutes,omitempty" jsonschema:"optional override for tools that accept until_minutes"`
}

func (s *Server) runSavedQuery(ctx context.Context, req *sdkmcp.CallToolRequest, args runSavedQueryArgs) (*sdkmcp.CallToolResult, any, error) {
	if args.Tenant == "" {
		return errResult("tenant is required")
	}
	if args.Name == "" {
		return errResult("name is required")
	}

	q, err := s.mcpSaved.Get(args.Tenant, args.Name)
	if err != nil {
		names, listErr := s.mcpSaved.Names(args.Tenant)
		if listErr != nil {
			return errResult(err.Error())
		}
		if len(names) == 0 {
			return errResult(fmt.Sprintf("saved query %q not found for tenant %q (no saved queries for this tenant)", args.Name, args.Tenant))
		}
		return errResult(fmt.Sprintf("saved query %q not found for tenant %q; saved queries for this tenant: %s",
			args.Name, args.Tenant, strings.Join(names, ", ")))
	}

	entry, ok := savableTools[q.Tool]
	if !ok {
		return errResult(fmt.Sprintf("saved query %q uses unsupported tool %q", args.Name, q.Tool))
	}

	var argMap map[string]any
	if err := json.Unmarshal(q.Arguments, &argMap); err != nil {
		return errResult(fmt.Sprintf("decoding saved arguments: %v", err))
	}
	argMap["tenant"] = args.Tenant

	argMap, err = applyWindowOverride(entry.window, q.Tool, argMap, args.SinceMinutes, args.UntilMinutes)
	if err != nil {
		return errResult(err.Error())
	}

	raw, err := json.Marshal(argMap)
	if err != nil {
		return errResult(fmt.Sprintf("encoding arguments: %v", err))
	}
	res, err := s.invokeSavableTool(ctx, q.Tool, raw)
	if err != nil {
		return errResult(err.Error())
	}
	if res.IsError {
		return res, nil, nil
	}
	return res, nil, nil
}
