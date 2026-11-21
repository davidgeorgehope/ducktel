package mcp

import (
	"context"
	"encoding/json"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/feedback"
)

type submitFeedbackArgs struct {
	ReportID    string `json:"report_id,omitempty" jsonschema:"optional id, 1 to 64 bytes matching [A-Za-z0-9._-]. Empty generates one. A retry with the same id returns the first result."`
	Tenant      string `json:"tenant" jsonschema:"tenant this report is stored under. Required. This is the only tenant filter."`
	ToolName    string `json:"tool_name" jsonschema:"the ducktel tool that was confusing, wrong, or missing"`
	Category    string `json:"category" jsonschema:"one of bug, confusing_output, missing_capability, wrong_docs, other"`
	Severity    string `json:"severity" jsonschema:"one of low, medium, high"`
	Summary     string `json:"summary" jsonschema:"what went wrong, 1 to 200 bytes"`
	Details     string `json:"details" jsonschema:"what the tool did, 1 to 8000 bytes"`
	ProposedFix string `json:"proposed_fix" jsonschema:"a concrete change a maintainer could apply, 1 to 8000 bytes"`
	ExampleCall string `json:"example_call,omitempty" jsonschema:"optional example of the call that went wrong, up to 4000 bytes"`
}

func (s *Server) submitFeedback(ctx context.Context, req *sdkmcp.CallToolRequest, args submitFeedbackArgs) (*sdkmcp.CallToolResult, any, error) {
	if s.feedback == nil {
		return errResult("feedback storage is not configured")
	}
	report, err := feedback.ParseFields(feedback.Fields{
		ReportID:    args.ReportID,
		Tenant:      args.Tenant,
		ToolName:    args.ToolName,
		Category:    args.Category,
		Severity:    args.Severity,
		Summary:     args.Summary,
		Details:     args.Details,
		ProposedFix: args.ProposedFix,
		ExampleCall: args.ExampleCall,
	})
	if err != nil {
		return errResult(err.Error())
	}
	res, err := s.feedback.Submit(ctx, report)
	if err != nil {
		return errResult(err.Error())
	}
	out, err := json.Marshal(res)
	if err != nil {
		return errResult("encoding feedback result: " + err.Error())
	}
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(out)}},
	}, nil, nil
}
