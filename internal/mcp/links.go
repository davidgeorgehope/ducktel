package mcp

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SetDashboardURL turns on dashboard links: every tool result then ends with
// a dashboard_url line that opens the same query in the dashboard, so an
// agent can show a person where it found something. The dashboard's deep
// links use the same parameter names as the tools, so the link is the query.
// Empty disables links.
func (s *Server) SetDashboardURL(u string) {
	s.dashboardURL = strings.TrimSpace(u)
}

// link builds <dashboardURL>#<view>?<params>, or "" when links are off.
func (s *Server) link(view string, p url.Values) string {
	if s.dashboardURL == "" {
		return ""
	}
	return strings.TrimSuffix(s.dashboardURL, "#") + "#" + view + "?" + p.Encode()
}

// linked appends the dashboard link to a successful result.
func linked(res *sdkmcp.CallToolResult, link string) (*sdkmcp.CallToolResult, any, error) {
	if link != "" && res != nil && !res.IsError {
		res.Content = append(res.Content, &sdkmcp.TextContent{Text: "dashboard_url: " + link})
	}
	return res, nil, nil
}

func setIf(p url.Values, k, v string) {
	if v != "" {
		p.Set(k, v)
	}
}

func setInt(p url.Values, k string, v int) {
	if v > 0 {
		p.Set(k, strconv.Itoa(v))
	}
}

func setFloat(p url.Values, k string, v *float64) {
	if v != nil {
		p.Set(k, strconv.FormatFloat(*v, 'g', -1, 64))
	}
}

func addFilters(p url.Values, fs []attrFilter) {
	for _, f := range fs {
		p.Add("filter", fmt.Sprintf("%s:%s", f.Key, f.Value))
	}
}

func (s *Server) traceLink(tenant, traceID string) string {
	p := url.Values{}
	p.Set("tenant", tenant)
	p.Set("trace_id", traceID)
	return s.link("traces", p)
}

func (s *Server) spanLink(tenant string, a spanSearchArgs) string {
	p := url.Values{}
	p.Set("tenant", tenant)
	setIf(p, "service_name", a.ServiceName)
	setInt(p, "since_minutes", a.SinceMinutes)
	setInt(p, "limit", a.Limit)
	setFloat(p, "min_ms", a.MinMs)
	setFloat(p, "max_ms", a.MaxMs)
	addFilters(p, a.Filters)
	return s.link("traces", p)
}

// logLink: a trace-scoped log search is best seen as that trace's waterfall,
// which shows the trace's logs under it.
func (s *Server) logLink(tenant string, a logSearchArgs) string {
	if a.TraceID != "" {
		return s.traceLink(tenant, a.TraceID)
	}
	p := url.Values{}
	p.Set("tenant", tenant)
	setIf(p, "service_name", a.ServiceName)
	setIf(p, "severity", a.Severity)
	setIf(p, "search", a.Search)
	setInt(p, "since_minutes", a.SinceMinutes)
	setInt(p, "limit", a.Limit)
	return s.link("logs", p)
}

// slowLink opens the same question the slow_requests tool asked: the metric's
// exemplars when metric_name was set, otherwise the span search sorted by
// duration via the dashboard's min_ms/max_ms.
func (s *Server) slowLink(tenant string, a slowRequestsArgs) string {
	if a.MetricName != "" {
		filters := append([]attrFilter{}, a.Filters...)
		if a.ServiceName != "" {
			filters = append(filters, attrFilter{Key: "service.name", Value: a.ServiceName})
		}
		return s.metricLink(tenant, metricQueryArgs{
			Tenant:       tenant,
			MetricName:   a.MetricName,
			Aggregation:  "exemplars",
			SinceMinutes: a.SinceMinutes,
			Filters:      filters,
			MinValue:     a.MinValue,
			MaxValue:     a.MaxValue,
			Limit:        a.Limit,
		})
	}
	return s.spanLink(tenant, spanSearchArgs{
		Tenant:       tenant,
		ServiceName:  a.ServiceName,
		SinceMinutes: a.SinceMinutes,
		Limit:        a.Limit,
		MinMs:        a.MinValue,
		MaxMs:        a.MaxValue,
		Filters:      a.Filters,
	})
}

// compareLink opens the comparison view for the same scope and duration
// range the compare_spans tool used.
func (s *Server) compareLink(tenant string, a compareSpansArgs) string {
	p := url.Values{}
	p.Set("tenant", tenant)
	setIf(p, "service_name", a.ServiceName)
	setIf(p, "span_name", a.SpanName)
	setInt(p, "since_minutes", a.SinceMinutes)
	setInt(p, "until_minutes", a.UntilMinutes)
	setFloat(p, "min_ms", a.MinMs)
	setFloat(p, "max_ms", a.MaxMs)
	setInt(p, "limit", a.Limit)
	addFilters(p, a.Filters)
	return s.link("compare", p)
}

func (s *Server) metricLink(tenant string, a metricQueryArgs) string {
	p := url.Values{}
	p.Set("tenant", tenant)
	p.Set("metric_name", a.MetricName)
	p.Set("aggregation", a.Aggregation)
	setInt(p, "since_minutes", a.SinceMinutes)
	setIf(p, "group_by", a.GroupBy)
	setIf(p, "step", a.Step)
	addFilters(p, a.Filters)
	setFloat(p, "min_value", a.MinValue)
	setFloat(p, "max_value", a.MaxValue)
	setInt(p, "limit", a.Limit)
	return s.link("metrics", p)
}
