package mcp

import (
	"strings"
	"testing"
)

func TestBadFilterOpIsToolError(t *testing.T) {
	s := newTestServer(t)

	for _, op := range []string{"~", "<>"} {
		bad := []map[string]any{{"key": "http.route", "value": "/a", "op": op}}
		for _, name := range []string{"span_search", "log_search", "compare_spans", "slow_requests"} {
			args := map[string]any{"tenant": "acme", "filters": bad}
			if name == "compare_spans" {
				args["span_name"] = "op"
			}
			body, isErr := call(t, s, name, args)
			if !isErr {
				t.Errorf("%s accepted op %q: %s", name, op, body)
				continue
			}
			if !strings.Contains(body, "op") {
				t.Errorf("%s error for op %q does not mention the op: %s", name, op, body)
			}
		}
		body, isErr := call(t, s, "metric_query", map[string]any{
			"tenant": "acme", "metric_name": "http.server.duration", "aggregation": "count", "filters": bad,
		})
		if !isErr || !strings.Contains(body, "op") {
			t.Errorf("metric_query op %q: isErr=%v body=%s", op, isErr, body)
		}
	}
}

func TestDashboardLinkFilterNot(t *testing.T) {
	run := linkedServer(t)

	view, q := dashboardLink(t, run("span_search", map[string]any{
		"tenant": "acme",
		"filters": []map[string]any{
			{"key": "http.route", "value": "/poll", "op": "!="},
			{"key": "env", "value": "prod"},
		},
	}))
	if view != "traces" || q.Get("filter_not") != "http.route:/poll" || q.Get("filter") != "env:prod" {
		t.Errorf("span link view %q params %v, want filter_not and filter separately", view, q)
	}

	view, q = dashboardLink(t, run("metric_query", map[string]any{
		"tenant": "acme", "metric_name": "http.server.duration", "aggregation": "count",
		"filters": []map[string]any{{"key": "http.route", "value": "/poll", "op": "!="}},
	}))
	if view != "metrics" || q.Get("filter_not") != "http.route:/poll" || q.Get("filter") != "" {
		t.Errorf("metric link view %q params %v, want only filter_not", view, q)
	}

	view, q = dashboardLink(t, run("log_search", map[string]any{
		"tenant":  "acme",
		"filters": []map[string]any{{"key": "http.route", "value": "/poll", "op": "!="}},
	}))
	if view != "logs" || q.Get("filter_not") != "http.route:/poll" {
		t.Errorf("log link view %q params %v, want filter_not", view, q)
	}
}
