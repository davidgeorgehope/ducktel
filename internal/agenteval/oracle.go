package agenteval

import (
	"sort"

	"github.com/davidgeorgehope/ducktel/internal/testharness"
)

const (
	statusError    = 2
	kindServer     = 2
	severityWarn   = "WARN"
	paymentService = "payment-service"
	errorCounter   = "http.server.request.error.count"
)

// Root is one root span tied for the longest duration in the batch.
type Root struct {
	TraceID  string
	SpanName string
}

// Oracle is the answer key for one batch. It is a pure function of the
// records the generator emitted.
type Oracle struct {
	Tenants           []string
	ErrorCount        map[string]int
	MostErrorServices []string
	SlowestRoots      []Root
	FailingPayment    []string
	MetricNames       []string
	WarnCount         int
	WarnBodies        map[string]int
	ServerSpans       map[string]int
	SpanServices      []string
	LogServices       []string
	ErrorCounter      map[string]float64
	SpansInTrace      map[string]int
}

// Derive builds the answer key from batch.
func Derive(batch testharness.Batch) Oracle {
	o := Oracle{
		ErrorCount:   map[string]int{},
		WarnBodies:   map[string]int{},
		ServerSpans:  map[string]int{},
		ErrorCounter: map[string]float64{},
		SpansInTrace: map[string]int{},
	}
	if batch.Tenant != "" {
		o.Tenants = []string{batch.Tenant}
	}

	spanSvcs := map[string]bool{}
	var maxDur int64
	var roots []Root
	rootDur := map[string]int64{}
	pay := map[string]bool{}
	for _, s := range batch.Spans {
		spanSvcs[s.Service] = true
		o.SpansInTrace[s.TraceID]++
		if s.Kind == kindServer {
			o.ServerSpans[s.Service]++
		}
		if s.Status == statusError {
			o.ErrorCount[s.Service]++
		}
		if s.Service == paymentService && s.Status == statusError {
			pay[s.TraceID] = true
		}
		if s.ParentSpanID == "" {
			d := s.EndNano - s.StartNano
			roots = append(roots, Root{TraceID: s.TraceID, SpanName: s.Name})
			rootDur[s.TraceID] = d
			if d > maxDur {
				maxDur = d
			}
		}
	}
	for _, r := range roots {
		if rootDur[r.TraceID] == maxDur {
			o.SlowestRoots = append(o.SlowestRoots, r)
		}
	}
	sort.Slice(o.SlowestRoots, func(i, j int) bool {
		return o.SlowestRoots[i].TraceID < o.SlowestRoots[j].TraceID
	})

	maxErr := 0
	for _, n := range o.ErrorCount {
		if n > maxErr {
			maxErr = n
		}
	}
	for svc, n := range o.ErrorCount {
		if maxErr > 0 && n == maxErr {
			o.MostErrorServices = append(o.MostErrorServices, svc)
		}
	}
	sort.Strings(o.MostErrorServices)

	for id := range pay {
		o.FailingPayment = append(o.FailingPayment, id)
	}
	sort.Strings(o.FailingPayment)

	logSvcs := map[string]bool{}
	for _, l := range batch.Logs {
		logSvcs[l.Service] = true
		if l.Severity == severityWarn {
			o.WarnCount++
			o.WarnBodies[l.Body]++
		}
	}

	names := map[string]bool{}
	for _, m := range batch.Metrics {
		names[m.Name] = true
		if m.Name == errorCounter {
			o.ErrorCounter[m.Service] += m.Value
		}
	}
	for name := range names {
		o.MetricNames = append(o.MetricNames, name)
	}
	sort.Strings(o.MetricNames)
	o.SpanServices = setKeys(spanSvcs)
	o.LogServices = setKeys(logSvcs)
	return o
}

func setKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
