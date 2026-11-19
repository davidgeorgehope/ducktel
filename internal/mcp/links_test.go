package mcp

import (
	"net/url"
	"strings"
	"testing"
)

// dashboardLink returns the dashboard_url line of a tool result, parsed.
func dashboardLink(t *testing.T, body string) (view string, q url.Values) {
	t.Helper()
	_, link, ok := strings.Cut(body, "dashboard_url: ")
	if !ok {
		t.Fatalf("no dashboard_url in result:\n%s", body)
	}
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "otel.example.com" || u.Path != "/dashboard/" {
		t.Fatalf("link base %q", link)
	}
	v, raw, _ := strings.Cut(u.Fragment, "?")
	q, err = url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v, q
}

func linkedServer(t *testing.T) func(name string, args map[string]any) string {
	s := newTestServerWith(t, func(s *Server) { s.SetDashboardURL("https://otel.example.com/dashboard/") })
	return func(name string, args map[string]any) string {
		body, isErr := call(t, s, name, args)
		if isErr {
			t.Fatalf("%s: %s", name, body)
		}
		return body
	}
}

func TestDashboardLinkSpanSearch(t *testing.T) {
	run := linkedServer(t)
	body := run("span_search", map[string]any{
		"tenant": "acme", "service_name": "api", "since_minutes": 120, "min_ms": 250, "max_ms": 500.5,
		"filters": []map[string]any{{"key": "http.target", "value": "/a b"}},
	})
	view, q := dashboardLink(t, body)
	if view != "traces" || q.Get("tenant") != "acme" || q.Get("service_name") != "api" ||
		q.Get("since_minutes") != "120" || q.Get("min_ms") != "250" || q.Get("max_ms") != "500.5" ||
		q.Get("filter") != "http.target:/a b" {
		t.Fatalf("view %q params %v", view, q)
	}
}

func TestDashboardLinkTraceAndLogs(t *testing.T) {
	run := linkedServer(t)
	view, q := dashboardLink(t, run("trace_lookup", map[string]any{"tenant": "acme", "trace_id": "trace-acme-1"}))
	if view != "traces" || q.Get("trace_id") != "trace-acme-1" {
		t.Fatalf("trace link %q %v", view, q)
	}
	// A trace's logs are shown under its waterfall.
	view, q = dashboardLink(t, run("log_search", map[string]any{"tenant": "acme", "trace_id": "trace-acme-1"}))
	if view != "traces" || q.Get("trace_id") != "trace-acme-1" {
		t.Fatalf("trace-scoped log link %q %v", view, q)
	}
	view, q = dashboardLink(t, run("log_search", map[string]any{"tenant": "acme", "severity": "ERROR", "search": "refused"}))
	if view != "logs" || q.Get("severity") != "ERROR" || q.Get("search") != "refused" {
		t.Fatalf("log link %q %v", view, q)
	}
}

func TestDashboardLinkMetric(t *testing.T) {
	run := linkedServer(t)
	view, q := dashboardLink(t, run("metric_query", map[string]any{
		"tenant": "acme", "metric_name": "x", "aggregation": "exemplars", "group_by": "attr.route",
		"step":      "1m",
		"filters":   []map[string]any{{"key": "service.name", "value": "api"}},
		"min_value": 250, "max_value": 500, "limit": 5,
	}))
	if view != "metrics" || q.Get("metric_name") != "x" || q.Get("aggregation") != "exemplars" ||
		q.Get("group_by") != "attr.route" || q.Get("step") != "1m" || q.Get("filter") != "service.name:api" ||
		q.Get("min_value") != "250" || q.Get("max_value") != "500" || q.Get("limit") != "5" {
		t.Fatalf("metric link %q %v", view, q)
	}
}

func TestNoDashboardLinkByDefault(t *testing.T) {
	s := newTestServer(t)
	body, _ := call(t, s, "span_search", map[string]any{"tenant": "acme"})
	if strings.Contains(body, "dashboard_url") {
		t.Fatalf("link without --dashboard-url:\n%s", body)
	}
}

func TestNoDashboardLinkOnError(t *testing.T) {
	s := newTestServerWith(t, func(s *Server) { s.SetDashboardURL("https://otel.example.com/dashboard/") })
	body, isErr := call(t, s, "metric_query", map[string]any{"tenant": "acme", "metric_name": "x", "aggregation": "nope"})
	if !isErr || strings.Contains(body, "dashboard_url") {
		t.Fatalf("error result: isErr=%v body=%s", isErr, body)
	}
}
