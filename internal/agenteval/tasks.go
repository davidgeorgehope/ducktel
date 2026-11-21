package agenteval

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	tenantName    = "acme"
	apiGateway    = "api-gateway"
	windowMinutes = 180
	searchLimit   = 1000
)

// Call is one scripted MCP call. Bind fills arguments that depend on an
// earlier result in the same task.
type Call struct {
	Label string
	Tool  string
	Args  map[string]any
	Bind  func(prev []Result, args map[string]any) error
}

// Result is the text an agent would read from one call.
type Result struct {
	Label   string
	Tool    string
	Text    string
	IsError bool
	Bytes   int
	Tokens  int
}

// Task is one agent question, the calls that answer it, and the check
// against the oracle.
type Task struct {
	Name     string
	Question string
	Calls    []Call
	Grade    func(results []Result, o Oracle) error
}

// FlushCall is the flush_buffer call. It is registered only when a flush URL
// is configured, and it is the only call that tool has.
var FlushCall = Call{
	Label: "flush_buffer",
	Tool:  "flush_buffer",
}

// Tasks is the scripted suite. flush_buffer is not here.
func Tasks() []Task {
	return []Task{
		{
			Name:     "list_tenants",
			Question: "Which tenant ids have stored telemetry?",
			Calls: []Call{{
				Label: "list_tenants",
				Tool:  "list_tenants",
			}},
			Grade: gradeTenants,
		},
		{
			Name:     "list_services",
			Question: "Which services emitted spans for tenant acme, and which emitted logs?",
			Calls: []Call{
				{Label: "list_services", Tool: "list_services", Args: map[string]any{"tenant": tenantName}},
				{Label: "list_services source=logs", Tool: "list_services", Args: map[string]any{"tenant": tenantName, "source": "logs"}},
			},
			Grade: gradeServices,
		},
		{
			Name:     "list_metric_names",
			Question: "Which metric names are stored for tenant acme?",
			Calls: []Call{{
				Label: "list_metric_names",
				Tool:  "list_metric_names",
				Args:  map[string]any{"tenant": tenantName},
			}},
			Grade: gradeMetricNames,
		},
		{
			Name:     "list_attribute_keys",
			Question: "Which trace attribute keys does tenant acme have, including resource attributes?",
			Calls: []Call{{
				Label: "list_attribute_keys",
				Tool:  "list_attribute_keys",
				Args:  map[string]any{"tenant": tenantName, "since_minutes": windowMinutes},
			}},
			Grade: gradeAttributeKeys,
		},
		{
			Name:     "most_error_spans",
			Question: "Which service has the most error spans for tenant acme?",
			Calls: []Call{{
				Label: "span_search status=error",
				Tool:  "span_search",
				Args: map[string]any{
					"tenant":        tenantName,
					"since_minutes": windowMinutes,
					"limit":         searchLimit,
					"filters":       []map[string]any{{"key": "status_code", "value": "error"}},
				},
			}},
			Grade: gradeMostErrors,
		},
		{
			Name:     "slowest_endpoint",
			Question: "What is the span name of the slowest root request for tenant acme?",
			Calls: []Call{{
				Label: "slow_requests limit=1",
				Tool:  "slow_requests",
				Args: map[string]any{
					"tenant":        tenantName,
					"since_minutes": windowMinutes,
					"limit":         1,
				},
			}},
			Grade: gradeSlowest,
		},
		{
			Name:     "failing_payment",
			Question: "Give one trace id for a failing payment-service request in tenant acme, and how many spans that trace has.",
			Calls: []Call{
				{
					Label: "span_search service=payment-service status=error",
					Tool:  "span_search",
					Args: map[string]any{
						"tenant":        tenantName,
						"service_name":  paymentService,
						"since_minutes": windowMinutes,
						"limit":         5,
						"filters":       []map[string]any{{"key": "status_code", "value": "error"}},
					},
				},
				{
					Label: "trace_lookup detail=summary",
					Tool:  "trace_lookup",
					Args:  map[string]any{"tenant": tenantName, "detail": "summary"},
					Bind:  bindPaymentTrace,
				},
			},
			Grade: gradeFailingPayment,
		},
		{
			Name:     "error_counter",
			Question: "What is the sum of http.server.request.error.count by service for tenant acme?",
			Calls: []Call{{
				Label: "metric_query http.server.request.error.count sum service_name",
				Tool:  "metric_query",
				Args: map[string]any{
					"tenant":        tenantName,
					"metric_name":   errorCounter,
					"aggregation":   "sum",
					"group_by":      "service_name",
					"since_minutes": windowMinutes,
				},
			}},
			Grade: gradeErrorCounter,
		},
		{
			Name:     "warn_logs",
			Question: "What WARN logs does tenant acme have?",
			Calls: []Call{{
				Label: "log_search severity=WARN",
				Tool:  "log_search",
				Args: map[string]any{
					"tenant":        tenantName,
					"severity":      severityWarn,
					"since_minutes": windowMinutes,
					"limit":         searchLimit,
				},
			}},
			Grade: gradeWarnLogs,
		},
		{
			Name:     "compare_spans",
			Question: "How do slow api-gateway server spans differ from the rest for tenant acme?",
			Calls: []Call{{
				Label: "compare_spans service=api-gateway",
				Tool:  "compare_spans",
				Args: map[string]any{
					"tenant":        tenantName,
					"service_name":  apiGateway,
					"since_minutes": windowMinutes,
				},
			}},
			Grade: gradeCompare,
		},
		{
			Name:     "saved_warn_logs",
			Question: "Save the WARN log search for tenant acme under the name warn-logs, then run that saved query.",
			Calls: []Call{
				{
					Label: "save_query warn-logs",
					Tool:  "save_query",
					Args: map[string]any{
						"tenant":      tenantName,
						"name":        savedWarnName,
						"tool":        "log_search",
						"description": "WARN logs",
						"arguments": map[string]any{
							"tenant":        tenantName,
							"severity":      severityWarn,
							"since_minutes": windowMinutes,
							"limit":         searchLimit,
						},
					},
				},
				{
					Label: "list_saved_queries",
					Tool:  "list_saved_queries",
					Args:  map[string]any{"tenant": tenantName},
				},
				{
					Label: "run_saved_query warn-logs",
					Tool:  "run_saved_query",
					Args:  map[string]any{"tenant": tenantName, "name": savedWarnName},
				},
			},
			Grade: gradeSavedWarn,
		},
		{
			Name:     "sql_query_services",
			Question: "Which services emitted spans for tenant acme? Answer with one sql_query.",
			Calls: []Call{{
				Label: "sql_query span services",
				Tool:  "sql_query",
				Args: map[string]any{
					"tenant": tenantName,
					"sql":    "SELECT service_name FROM spans GROUP BY service_name ORDER BY service_name",
				},
			}},
			Grade: gradeSQLServices,
		},
	}
}

const savedWarnName = "warn-logs"

func gradeTenants(results []Result, o Oracle) error {
	rows, err := decodeRows(results[0].Text)
	if err != nil {
		return err
	}
	got, err := columnStrings(rows, "tenant")
	if err != nil {
		return err
	}
	if !sameStrings(got, o.Tenants) {
		return fmt.Errorf("tenants %v, oracle %v", got, o.Tenants)
	}
	return nil
}

func gradeServices(results []Result, o Oracle) error {
	spans, err := decodeRows(results[0].Text)
	if err != nil {
		return err
	}
	logs, err := decodeRows(results[1].Text)
	if err != nil {
		return err
	}
	spanNames, err := columnStrings(spans, "service_name")
	if err != nil {
		return err
	}
	logNames, err := columnStrings(logs, "service_name")
	if err != nil {
		return err
	}
	if !sameStrings(spanNames, o.SpanServices) {
		return fmt.Errorf("span services %v, oracle %v", spanNames, o.SpanServices)
	}
	if !sameStrings(logNames, o.LogServices) {
		return fmt.Errorf("log services %v, oracle %v", logNames, o.LogServices)
	}
	return nil
}

func gradeMetricNames(results []Result, o Oracle) error {
	rows, err := decodeRows(results[0].Text)
	if err != nil {
		return err
	}
	got, err := columnStrings(rows, "metric_name")
	if err != nil {
		return err
	}
	if !sameStrings(got, o.MetricNames) {
		return fmt.Errorf("metric names %v, oracle %v", got, o.MetricNames)
	}
	return nil
}

func gradeAttributeKeys(results []Result, _ Oracle) error {
	rows, err := decodeRows(results[0].Text)
	if err != nil {
		return err
	}
	var method, tenant bool
	for _, row := range rows {
		signal, _ := row["signal"].(string)
		source, _ := row["source"].(string)
		key, _ := row["key"].(string)
		if signal == "traces" && source == "attributes" && key == "http.request.method" {
			method = true
		}
		if signal == "traces" && source == "resource_attributes" && key == "tenant.id" {
			tenant = true
		}
	}
	if !method || !tenant {
		return fmt.Errorf("attribute keys missing http.request.method on traces/attributes (%v) or tenant.id on traces/resource_attributes (%v): %s", method, tenant, clip(results[0].Text))
	}
	return nil
}

func gradeMostErrors(results []Result, o Oracle) error {
	rows, err := decodeRows(results[0].Text)
	if err != nil {
		return err
	}
	if len(rows) == searchLimit {
		return fmt.Errorf("span_search truncated at limit %d; raise the limit or lower n", searchLimit)
	}
	counts := map[string]int{}
	for _, row := range rows {
		svc, _ := row["service_name"].(string)
		if svc == "" {
			return fmt.Errorf("span row missing service_name: %v", row)
		}
		counts[svc]++
	}
	if !sameCounts(counts, o.ErrorCount) {
		return fmt.Errorf("error counts %v, oracle %v", counts, o.ErrorCount)
	}
	got := maxServices(counts)
	if !sameStrings(got, o.MostErrorServices) {
		return fmt.Errorf("most-error services %v, oracle %v", got, o.MostErrorServices)
	}
	for _, svc := range o.MostErrorServices {
		if counts[svc] == 0 {
			return fmt.Errorf("winning service %s is not in the rows", svc)
		}
	}
	return nil
}

func gradeSlowest(results []Result, o Oracle) error {
	var rows []struct {
		TraceID string `json:"trace_id"`
		Trace   struct {
			Root *struct {
				SpanName string `json:"span_name"`
			} `json:"root"`
		} `json:"trace"`
	}
	if err := json.Unmarshal([]byte(results[0].Text), &rows); err != nil {
		return fmt.Errorf("slow_requests: %w: %s", err, clip(results[0].Text))
	}
	if len(rows) == 0 {
		return fmt.Errorf("slow_requests returned no rows: %s", clip(results[0].Text))
	}
	got := rows[0]
	if got.Trace.Root == nil {
		return fmt.Errorf("slow_requests result has no root: %s", clip(results[0].Text))
	}
	for _, root := range o.SlowestRoots {
		if root.TraceID == got.TraceID && root.SpanName == got.Trace.Root.SpanName {
			return nil
		}
	}
	return fmt.Errorf("trace %s span %q is not a slowest root %v", got.TraceID, got.Trace.Root.SpanName, o.SlowestRoots)
}

func gradeFailingPayment(results []Result, o Oracle) error {
	rows, err := decodeRows(results[0].Text)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("no failing payment span: %s", clip(results[0].Text))
	}
	id, _ := rows[0]["trace_id"].(string)
	if !contains(o.FailingPayment, id) {
		return fmt.Errorf("trace %s is not a failing payment trace %v", id, o.FailingPayment)
	}
	var sum struct {
		SpanCount  int `json:"span_count"`
		ErrorCount int `json:"error_count"`
		Errors     []struct {
			ServiceName string `json:"service_name"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(results[1].Text), &sum); err != nil {
		return fmt.Errorf("trace_lookup: %w: %s", err, clip(results[1].Text))
	}
	if sum.SpanCount != o.SpansInTrace[id] {
		return fmt.Errorf("span_count %d, oracle %d for %s", sum.SpanCount, o.SpansInTrace[id], id)
	}
	if sum.ErrorCount <= 0 {
		return fmt.Errorf("error_count %d, want > 0", sum.ErrorCount)
	}
	for _, e := range sum.Errors {
		if e.ServiceName == paymentService {
			return nil
		}
	}
	return fmt.Errorf("summary errors have no %s span: %s", paymentService, clip(results[1].Text))
}

func gradeErrorCounter(results []Result, o Oracle) error {
	rows, err := decodeRows(results[0].Text)
	if err != nil {
		return err
	}
	got := map[string]float64{}
	for svc := range o.ErrorCounter {
		got[svc] = 0
	}
	for _, row := range rows {
		svc, ok := row["service_name"].(string)
		if !ok {
			return fmt.Errorf("metric row columns %s", rowKeys(row))
		}
		value, ok := asFloat(row["value"])
		if !ok {
			return fmt.Errorf("metric value column %T (%v), columns %s", row["value"], row["value"], rowKeys(row))
		}
		got[svc] = value
	}
	for svc, want := range o.ErrorCounter {
		if math.Abs(got[svc]-want) > 1e-6 {
			return fmt.Errorf("error counter %s = %v, oracle %v (rows %s)", svc, got[svc], want, clip(results[0].Text))
		}
	}
	return nil
}

func gradeWarnLogs(results []Result, o Oracle) error {
	rows, err := decodeRows(results[0].Text)
	if err != nil {
		return err
	}
	if len(rows) == searchLimit {
		return fmt.Errorf("log_search truncated at limit %d; raise the limit or lower n", searchLimit)
	}
	if len(rows) != o.WarnCount {
		return fmt.Errorf("WARN count %d, oracle %d", len(rows), o.WarnCount)
	}
	got := map[string]int{}
	for _, row := range rows {
		sev, _ := row["severity_text"].(string)
		if sev != severityWarn {
			return fmt.Errorf("severity %q, want WARN", sev)
		}
		body, _ := row["body"].(string)
		if o.WarnBodies[body] == 0 {
			return fmt.Errorf("body %q was not a WARN the generator emitted", body)
		}
		got[body]++
	}
	for body, n := range o.WarnBodies {
		if got[body] != n {
			return fmt.Errorf("WARN body %q count %d, oracle %d", body, got[body], n)
		}
	}
	return nil
}

func gradeSavedWarn(results []Result, o Oracle) error {
	var saved struct {
		Name   string `json:"name"`
		Tool   string `json:"tool"`
		Tenant string `json:"tenant"`
	}
	if err := json.Unmarshal([]byte(results[0].Text), &saved); err != nil {
		return fmt.Errorf("save_query: %w: %s", err, clip(results[0].Text))
	}
	if saved.Name != savedWarnName || saved.Tool != "log_search" || saved.Tenant != tenantName {
		return fmt.Errorf("save_query returned name %q tool %q tenant %q", saved.Name, saved.Tool, saved.Tenant)
	}
	rows, err := decodeRows(results[1].Text)
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return fmt.Errorf("list_saved_queries returned %d rows, want 1: %s", len(rows), clip(results[1].Text))
	}
	name, _ := rows[0]["name"].(string)
	tool, _ := rows[0]["tool"].(string)
	if name != savedWarnName || tool != "log_search" {
		return fmt.Errorf("saved query name %q tool %q", name, tool)
	}
	return gradeWarnLogs([]Result{results[2]}, o)
}

func gradeSQLServices(results []Result, o Oracle) error {
	rows, err := decodeRows(results[0].Text)
	if err != nil {
		return err
	}
	got, err := columnStrings(rows, "service_name")
	if err != nil {
		return err
	}
	if !sameStrings(got, o.SpanServices) {
		return fmt.Errorf("sql_query services %v, oracle %v", got, o.SpanServices)
	}
	return nil
}

func gradeCompare(results []Result, o Oracle) error {
	var cmp struct {
		SlowN int64 `json:"slow_n"`
		RestN int64 `json:"rest_n"`
	}
	if err := json.Unmarshal([]byte(results[0].Text), &cmp); err != nil {
		return fmt.Errorf("compare_spans: %w: %s", err, clip(results[0].Text))
	}
	want := int64(o.ServerSpans[apiGateway])
	if cmp.SlowN+cmp.RestN != want {
		return fmt.Errorf("slow_n + rest_n = %d, oracle api-gateway server spans %d: %s", cmp.SlowN+cmp.RestN, want, clip(results[0].Text))
	}
	return nil
}

func bindPaymentTrace(prev []Result, args map[string]any) error {
	if len(prev) == 0 {
		return fmt.Errorf("trace_lookup has no prior span_search")
	}
	rows, err := decodeRows(prev[len(prev)-1].Text)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("span_search returned no payment error span")
	}
	id, _ := rows[0]["trace_id"].(string)
	if id == "" {
		return fmt.Errorf("payment span row has no trace_id: %v", rows[0])
	}
	args["trace_id"] = id
	return nil
}

func decodeRows(text string) ([]map[string]any, error) {
	var rows []map[string]any
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return nil, fmt.Errorf("decode rows: %w: %s", err, clip(text))
	}
	return rows, nil
}

func columnStrings(rows []map[string]any, col string) ([]string, error) {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		s, ok := row[col].(string)
		if !ok {
			return nil, fmt.Errorf("column %s = %T in %v", col, row[col], rowKeys(row))
		}
		out = append(out, s)
	}
	return out, nil
}

func sameStrings(got, want []string) bool {
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) != len(w) {
		return false
	}
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

func sameCounts(got, want map[string]int) bool {
	if len(got) != len(want) {
		return false
	}
	for k, n := range want {
		if got[k] != n {
			return false
		}
	}
	return true
}

func maxServices(counts map[string]int) []string {
	max := 0
	for _, n := range counts {
		if n > max {
			max = n
		}
	}
	var out []string
	for svc, n := range counts {
		if n == max && max > 0 {
			out = append(out, svc)
		}
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	default:
		f, err := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(v)), 64)
		return f, err == nil
	}
}

func rowKeys(row map[string]any) string {
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func clip(s string) string {
	if len(s) > 2000 {
		return s[:2000] + "..."
	}
	return s
}
