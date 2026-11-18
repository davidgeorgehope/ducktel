package telemetry

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Compact, already-aggregated trace summaries. A 120-span trace with every
// attribute is far more than an agent needs in order to answer "what happened
// in this request"; the summary keeps the shape of the trace (root, services,
// repeated span groups, slowest spans, errors) and the log problems, and
// leaves the full rows to a detail=spans lookup.

const statusCodeError = "STATUS_CODE_ERROR"

// maxSpansPerTrace bounds how many spans of one trace are loaded and
// aggregated. A var so a test can lower it; the value is otherwise fixed.
var maxSpansPerTrace = 5000

// jsonTags on these types are part of the API: both the MCP summary result and
// /api/traces/{id}/summary are these structs marshalled directly.

type SpanBrief struct {
	SpanID        string  `json:"span_id"`
	ParentSpanID  string  `json:"parent_span_id,omitempty"`
	ServiceName   string  `json:"service_name"`
	SpanName      string  `json:"span_name"`
	StartTime     int64   `json:"start_time"` // microseconds, as stored
	DurationMs    float64 `json:"duration_ms"`
	StatusCode    string  `json:"status_code"`
	StatusMessage string  `json:"status_message,omitempty"`
}

type SpanGroup struct {
	ServiceName string  `json:"service_name"`
	SpanName    string  `json:"span_name"`
	Count       int     `json:"count"`
	TotalMs     float64 `json:"total_ms"`
	MaxMs       float64 `json:"max_ms"`
	Errors      int     `json:"errors"`
}

type ServiceCount struct {
	ServiceName string `json:"service_name"`
	Spans       int    `json:"spans"`
	Errors      int    `json:"errors"`
}

type LogBrief struct {
	Timestamp    int64  `json:"timestamp"`
	ServiceName  string `json:"service_name"`
	SeverityText string `json:"severity_text"`
	Body         string `json:"body"` // cut to 300 runes, then "..."
	SpanID       string `json:"span_id,omitempty"`
}

type LogSummary struct {
	Count      int            `json:"count"`
	BySeverity map[string]int `json:"by_severity"` // key = UPPER(severity_text), "" -> "UNSET"
	Problems   []LogBrief     `json:"problems"`    // severity_number >= 13 (WARN and above), oldest first
}

type TraceSummary struct {
	TraceID     string         `json:"trace_id"`
	SpanCount   int            `json:"span_count"`
	ErrorCount  int            `json:"error_count"`
	StartTime   int64          `json:"start_time"`  // min start_time
	DurationMs  float64        `json:"duration_ms"` // (max end_time - min start_time)/1000
	Root        *SpanBrief     `json:"root"`
	RootMissing bool           `json:"root_missing,omitempty"`
	Services    []ServiceCount `json:"services"`
	Groups      []SpanGroup    `json:"groups"`
	GroupsTotal int            `json:"groups_total"`
	Slowest     []SpanBrief    `json:"slowest"`
	Errors      []SpanBrief    `json:"errors"`
	Logs        LogSummary     `json:"logs"`
	Truncated   bool           `json:"truncated,omitempty"`
}

type SummaryLimits struct {
	Groups, Slowest, Errors, Problems int
}

var FullSummary = SummaryLimits{Groups: 15, Slowest: 10, Errors: 10, Problems: 10}
var BriefSummary = SummaryLimits{Groups: 3, Slowest: 0, Errors: 1, Problems: 1}

// TraceSummaries summarises several traces with one spans query and one logs
// query. since bounds which files are read (zero means every file); it is not
// a WHERE predicate, so a trace that starts before since but lives in a
// selected file is still summarised.
//
// The result is in the order of ids. An id with no spans gets a summary with
// SpanCount 0 and empty (never nil) slices.
func (c *Core) TraceSummaries(tenant string, ids []string, since time.Time, lim SummaryLimits) ([]TraceSummary, error) {
	if len(ids) == 0 {
		return nil, invalidInput("trace_id is required")
	}
	for _, id := range ids {
		if id == "" {
			return nil, invalidInput("trace_id is required")
		}
	}
	if err := validateTenant(tenant); err != nil {
		return nil, err
	}

	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	in, inParams := inPlaceholders(unique)

	spanLimit := maxSpansPerTrace
	if spanLimit <= 0 {
		spanLimit = 5000
	}

	// One spans query for every trace. row_number() caps the rows per trace at
	// spanLimit without a per-trace round trip; count(*) over the same
	// partition says whether that cap was hit. Attributes are deliberately not
	// selected: this is the compact path.
	spanQ := `WITH ranked AS (
	SELECT trace_id, span_id, parent_span_id, service_name, span_name,
	       start_time, end_time, duration_ms, status_code, status_message,
	       row_number() OVER (PARTITION BY trace_id ORDER BY start_time, span_id) AS rn,
	       count(*) OVER (PARTITION BY trace_id) AS span_total
	FROM traces
	WHERE trace_id IN (` + in + `) AND ` + c.tenantPredicate() + `
)
SELECT trace_id, span_id, parent_span_id, service_name, span_name,
       start_time, end_time, duration_ms, status_code, status_message, span_total
FROM ranked
WHERE rn <= ?
ORDER BY trace_id, start_time, span_id`
	spanParams := append(append([]any{}, inParams...), tenant, spanLimit)

	spanRows, _, err := c.queryAt(since, spanQ, spanParams...)
	if err != nil {
		return nil, err
	}

	// One logs query for every trace, oldest first so each trace's problems
	// are already in the required order.
	logQ := `SELECT timestamp, trace_id, span_id, service_name, severity_text,
	                severity_number, body
	         FROM logs
	         WHERE trace_id IN (` + in + `) AND ` + c.tenantPredicate() + `
	         ORDER BY timestamp, trace_id, span_id`
	logParams := append(append([]any{}, inParams...), tenant)

	logRows, _, err := c.queryAt(since, logQ, logParams...)
	if err != nil {
		return nil, err
	}

	aggs := make(map[string]*traceAgg, len(unique))
	for _, id := range unique {
		aggs[id] = newTraceAgg(id)
	}

	for _, r := range spanRows {
		id := strVal(r["trace_id"])
		a := aggs[id]
		if a == nil {
			continue
		}
		b := SpanBrief{
			SpanID:        strVal(r["span_id"]),
			ParentSpanID:  strVal(r["parent_span_id"]),
			ServiceName:   strVal(r["service_name"]),
			SpanName:      strVal(r["span_name"]),
			StartTime:     intVal(r["start_time"]),
			DurationMs:    roundMs(toFloat(r["duration_ms"])),
			StatusCode:    strVal(r["status_code"]),
			StatusMessage: strVal(r["status_message"]),
		}
		a.spans = append(a.spans, b)

		start, end := b.StartTime, intVal(r["end_time"])
		if !a.hasTime || start < a.minStart {
			a.minStart = start
		}
		if !a.hasTime || end > a.maxEnd {
			a.maxEnd = end
		}
		a.hasTime = true
		if total := intVal(r["span_total"]); total > a.spanTotal {
			a.spanTotal = total
		}

		errSpan := b.StatusCode == statusCodeError
		sc := a.services[b.ServiceName]
		if sc == nil {
			sc = &ServiceCount{ServiceName: b.ServiceName}
			a.services[b.ServiceName] = sc
		}
		sc.Spans++
		if errSpan {
			sc.Errors++
		}

	}

	for _, r := range logRows {
		id := strVal(r["trace_id"])
		a := aggs[id]
		if a == nil {
			continue
		}
		a.summary.Logs.Count++
		sev := strVal(r["severity_text"])
		key := strings.ToUpper(sev)
		if key == "" {
			key = "UNSET"
		}
		a.summary.Logs.BySeverity[key]++
		if intVal(r["severity_number"]) >= 13 {
			a.summary.Logs.Problems = append(a.summary.Logs.Problems, LogBrief{
				Timestamp:    intVal(r["timestamp"]),
				ServiceName:  strVal(r["service_name"]),
				SeverityText: sev,
				Body:         cutBody(strVal(r["body"])),
				SpanID:       strVal(r["span_id"]),
			})
		}
	}

	for _, a := range aggs {
		a.finish(lim)
	}

	out := make([]TraceSummary, len(ids))
	for i, id := range ids {
		out[i] = aggs[id].summary
	}
	return out, nil
}

// SummarizeTrace is TraceSummaries for one trace, with the full limits and no
// file-time bound.
func (c *Core) SummarizeTrace(traceID, tenant string) (*TraceSummary, error) {
	sums, err := c.TraceSummaries(tenant, []string{traceID}, time.Time{}, FullSummary)
	if err != nil {
		return nil, err
	}
	return &sums[0], nil
}

// traceAgg accumulates one trace's spans and logs before they are turned into
// the public summary.
type traceAgg struct {
	summary TraceSummary

	spans     []SpanBrief
	spanTotal int64
	minStart  int64
	maxEnd    int64
	hasTime   bool

	services map[string]*ServiceCount
}

func newTraceAgg(id string) *traceAgg {
	return &traceAgg{
		summary: TraceSummary{
			TraceID:  id,
			Services: []ServiceCount{},
			Groups:   []SpanGroup{},
			Slowest:  []SpanBrief{},
			Errors:   []SpanBrief{},
			Logs: LogSummary{
				BySeverity: map[string]int{},
				Problems:   []LogBrief{},
			},
		},
		services: map[string]*ServiceCount{},
	}
}

func (a *traceAgg) finish(lim SummaryLimits) {
	a.summary.Logs.Problems = take(a.summary.Logs.Problems, lim.Problems)
	if len(a.spans) == 0 {
		return
	}

	rootIdx := -1
	for i, s := range a.spans {
		if s.ParentSpanID != "" {
			continue
		}
		if rootIdx < 0 || earlierSpan(s, a.spans[rootIdx]) {
			rootIdx = i
		}
	}
	if rootIdx < 0 {
		rootIdx = 0
		for i := range a.spans {
			if earlierSpan(a.spans[i], a.spans[rootIdx]) {
				rootIdx = i
			}
		}
		a.summary.RootMissing = true
	}
	root := a.spans[rootIdx]
	a.summary.Root = &root

	// SpanCount is the trace's real size even when only the first
	// maxSpansPerTrace spans were aggregated (Truncated says so).
	a.summary.SpanCount = max(len(a.spans), int(a.spanTotal))
	a.summary.Truncated = a.spanTotal > int64(len(a.spans))
	a.summary.StartTime = a.minStart
	if a.maxEnd > a.minStart {
		a.summary.DurationMs = roundMs(float64(a.maxEnd-a.minStart) / 1000)
	}

	for _, s := range a.spans {
		if s.StatusCode == statusCodeError {
			a.summary.ErrorCount++
		}
	}

	for _, sc := range a.services {
		a.summary.Services = append(a.summary.Services, *sc)
	}
	sort.Slice(a.summary.Services, func(i, j int) bool {
		if a.summary.Services[i].Spans != a.summary.Services[j].Spans {
			return a.summary.Services[i].Spans > a.summary.Services[j].Spans
		}
		return a.summary.Services[i].ServiceName < a.summary.Services[j].ServiceName
	})

	// Groups leave the root out: it is already reported as root, and in a
	// brief summary it would take one of the few group slots.
	byKey := map[string]*SpanGroup{}
	for i, sp := range a.spans {
		if i == rootIdx {
			continue
		}
		key := sp.ServiceName + "\x00" + sp.SpanName
		g := byKey[key]
		if g == nil {
			g = &SpanGroup{ServiceName: sp.ServiceName, SpanName: sp.SpanName}
			byKey[key] = g
		}
		g.Count++
		g.TotalMs += sp.DurationMs
		g.MaxMs = max(g.MaxMs, sp.DurationMs)
		if sp.StatusCode == statusCodeError {
			g.Errors++
		}
	}
	groups := make([]SpanGroup, 0, len(byKey))
	for _, g := range byKey {
		g.TotalMs, g.MaxMs = roundMs(g.TotalMs), roundMs(g.MaxMs)
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].TotalMs != groups[j].TotalMs {
			return groups[i].TotalMs > groups[j].TotalMs
		}
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		if groups[i].ServiceName != groups[j].ServiceName {
			return groups[i].ServiceName < groups[j].ServiceName
		}
		return groups[i].SpanName < groups[j].SpanName
	})
	a.summary.GroupsTotal = len(groups)
	a.summary.Groups = take(groups, lim.Groups)

	slowest := make([]SpanBrief, 0, len(a.spans))
	for i, s := range a.spans {
		if i == rootIdx {
			continue
		}
		slowest = append(slowest, s)
	}
	sort.Slice(slowest, func(i, j int) bool {
		if slowest[i].DurationMs != slowest[j].DurationMs {
			return slowest[i].DurationMs > slowest[j].DurationMs
		}
		if slowest[i].StartTime != slowest[j].StartTime {
			return slowest[i].StartTime < slowest[j].StartTime
		}
		return slowest[i].SpanID < slowest[j].SpanID
	})
	a.summary.Slowest = take(slowest, lim.Slowest)

	errs := make([]SpanBrief, 0)
	for _, s := range a.spans {
		if s.StatusCode == statusCodeError {
			errs = append(errs, s)
		}
	}
	sort.Slice(errs, func(i, j int) bool {
		if errs[i].StartTime != errs[j].StartTime {
			return errs[i].StartTime < errs[j].StartTime
		}
		return errs[i].SpanID < errs[j].SpanID
	})
	a.summary.Errors = take(errs, lim.Errors)
}

// earlierSpan is the deterministic order used to pick the earliest of several
// candidate root spans (and the earliest span when a trace has no root).
func earlierSpan(a, b SpanBrief) bool {
	if a.StartTime != b.StartTime {
		return a.StartTime < b.StartTime
	}
	return a.SpanID < b.SpanID
}

func inPlaceholders(ids []string) (string, []any) {
	parts := make([]string, len(ids))
	params := make([]any, len(ids))
	for i, id := range ids {
		parts[i] = "?"
		params[i] = id
	}
	return strings.Join(parts, ", "), params
}

func (c *Core) queryAt(since time.Time, q string, params ...any) ([]map[string]any, []string, error) {
	if since.IsZero() {
		return c.engine.Query(q, params...)
	}
	return c.engine.QuerySince(since, q, params...)
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intVal(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case uint64:
		return int64(x)
	case uint32:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

// cutBody keeps the first 300 runes of a log body, then marks the cut. Runes,
// not bytes, so a multi-byte character is never split.
func cutBody(s string) string {
	r := []rune(s)
	if len(r) <= 300 {
		return s
	}
	return string(r[:300]) + "..."
}

// take returns a copy of at most the first n elements, never nil.
func take[T any](s []T, n int) []T {
	n = max(0, min(n, len(s)))
	out := make([]T, n)
	copy(out, s[:n])
	return out
}

// roundMs keeps milliseconds to two decimals (10 microseconds): float noise
// such as 2266.731370000001 costs a reader's context and says nothing.
func roundMs(v float64) float64 { return math.Round(v*100) / 100 }
