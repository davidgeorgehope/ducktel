package main

import (
	"strings"
	"testing"
)

// TestMCPHelpListsEveryTool: ducktel mcp --help is a tool list. A name that
// Register advertises and this text omits is a tool an operator cannot see.
func TestMCPHelpListsEveryTool(t *testing.T) {
	help := mcpCmd().Long
	for _, name := range []string{
		"list_tenants",
		"list_services",
		"list_metric_names",
		"list_attribute_keys",
		"trace_lookup",
		"span_search",
		"compare_spans",
		"log_search",
		"slow_requests",
		"metric_query",
		"list_saved_queries",
		"save_query",
		"run_saved_query",
		"sql_query",
		"submit_feedback",
		"flush_buffer",
	} {
		if !strings.Contains(help, name) {
			t.Errorf("mcp help omits %s", name)
		}
	}
	if !strings.Contains(help, "list_tenants takes no arguments") {
		t.Error("mcp help should say list_tenants takes no tenant")
	}
	for _, phrase := range []string{
		"off by default",
		"does not scrub",
		"save_query will not store it",
		"--feedback-url http://127.0.0.1:4318/feedback",
		"second",
	} {
		if !strings.Contains(help, phrase) {
			t.Errorf("mcp help missing %q", phrase)
		}
	}
}
