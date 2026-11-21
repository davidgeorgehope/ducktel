// Package testharness generates synthetic OpenTelemetry data and sends it to
// ducktel's OTLP receiver. It simulates a realistic microservice topology with
// traces, logs, and metrics — including injectable failure scenarios.
//
// Inspired by github.com/davidgeorgehope/otel-demo-gen but written in Go to
// live inside the ducktel repo as a first-class testing tool.
package testharness

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Service describes a simulated microservice.
type Service struct {
	Name       string
	Language   string
	Operations []Operation
	DependsOn  []Dependency
}

// Operation is a named endpoint/span within a service.
type Operation struct {
	Name     string
	SpanName string
	MinMs    int
	MaxMs    int
}

// Dependency links one service to another (or to a database).
type Dependency struct {
	Service  string // downstream service name (empty if DB)
	DB       string // database name (empty if service)
	DBSystem string // e.g. "postgres", "redis"
	Protocol string // "http", "grpc"
}

// Scenario injects failures into the generated telemetry.
type Scenario struct {
	Name            string
	TargetService   string
	Type            string  // "error_rate", "latency_spike", "service_unavailable"
	ErrorRate       float64 // 0.0–1.0 for error_rate
	LatencyMultiple float64 // multiplier for latency_spike
}

// Config holds the full harness configuration.
type Config struct {
	Endpoint  string // OTLP HTTP endpoint (default http://localhost:4318)
	Services  []Service
	Scenarios []Scenario
	TraceRate float64 // traces per second (default 2)
	MetricsMs int     // metrics interval in ms (default 10000)
	ErrorRate float64 // baseline error rate (default 0.05)
	// AuthToken, when set, is sent as `Authorization: Bearer <token>` so the
	// generator can drive a receiver started with --auth-token.
	AuthToken string
	// Tenant, when set, is emitted as the `tenant.id` resource attribute. The
	// MCP query tools filter on it, so generated data is only visible to them
	// when this is set.
	Tenant string
}

// Span is one span EmitBatch emitted.
type Span struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Service      string
	Name         string
	Kind         int
	StartNano    int64
	EndNano      int64
	Status       int
}

// Log is one log record EmitBatch emitted.
type Log struct {
	Service     string
	Severity    string
	SeverityNum int
	Body        string
	TraceID     string
	SpanID      string
}

// Metric is one numeric point EmitBatch emitted.
type Metric struct {
	Name    string
	Service string
	Value   float64
}

// Batch is the traces, logs, and metrics snapshot EmitBatch produced.
type Batch struct {
	// Tenant is the tenant.id resource attribute applied to the batch.
	// Empty when Config.Tenant is empty.
	Tenant  string
	Spans   []Span
	Logs    []Log
	Metrics []Metric
}

// MetricNames returns the metric names the generator emits, in catalog order.
func MetricNames() []string {
	cat := metricCatalog()
	names := make([]string, len(cat))
	for i, m := range cat {
		names[i] = m.name
	}
	return names
}

type metricKind int

const (
	metricGauge metricKind = iota
	metricSum
)

type metricDef struct {
	name  string
	unit  string
	kind  metricKind
	asInt bool
}

func metricCatalog() []metricDef {
	return []metricDef{
		{name: "system.cpu.utilization", unit: "%", kind: metricGauge},
		{name: "process.memory.usage", unit: "By", kind: metricGauge, asInt: true},
		{name: "http.server.request.count", unit: "requests", kind: metricSum},
		{name: "http.server.request.error.count", unit: "errors", kind: metricSum},
	}
}

// Harness generates and sends synthetic telemetry.
type Harness struct {
	cfg    Config
	client *http.Client
	stop   chan struct{}
	wg     sync.WaitGroup

	svcMap  map[string]*Service
	scenMap map[string]*Scenario // service -> scenario

	// endpointSet is true when Config.Endpoint was non-empty. EmitBatch posts
	// only then. Start still sends to the default endpoint New fills in.
	endpointSet bool

	rnd   *rand.Rand
	rndMu sync.Mutex
}

// New creates a harness from config, applying defaults.
func New(cfg Config) *Harness {
	endpointSet := cfg.Endpoint != ""
	if cfg.Endpoint == "" {
		cfg.Endpoint = "http://localhost:4318"
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	if cfg.TraceRate <= 0 {
		cfg.TraceRate = 2
	}
	if cfg.MetricsMs <= 0 {
		cfg.MetricsMs = 10000
	}

	h := &Harness{
		cfg:         cfg,
		client:      &http.Client{Timeout: 5 * time.Second},
		stop:        make(chan struct{}),
		svcMap:      make(map[string]*Service),
		scenMap:     make(map[string]*Scenario),
		endpointSet: endpointSet,
	}
	for i := range cfg.Services {
		h.svcMap[cfg.Services[i].Name] = &cfg.Services[i]
	}
	for i := range cfg.Scenarios {
		h.scenMap[cfg.Scenarios[i].TargetService] = &cfg.Scenarios[i]
	}
	return h
}

// SetRand makes generation deterministic. Nil keeps the global math/rand and
// crypto/rand path the CLI uses.
func (h *Harness) SetRand(r *rand.Rand) {
	h.rndMu.Lock()
	h.rnd = r
	h.rndMu.Unlock()
}

func (h *Harness) intn(n int) int {
	h.rndMu.Lock()
	r := h.rnd
	if r == nil {
		h.rndMu.Unlock()
		return rand.Intn(n)
	}
	v := r.Intn(n)
	h.rndMu.Unlock()
	return v
}

func (h *Harness) int63n(n int64) int64 {
	h.rndMu.Lock()
	r := h.rnd
	if r == nil {
		h.rndMu.Unlock()
		return rand.Int63n(n)
	}
	v := r.Int63n(n)
	h.rndMu.Unlock()
	return v
}

func (h *Harness) float64() float64 {
	h.rndMu.Lock()
	r := h.rnd
	if r == nil {
		h.rndMu.Unlock()
		return rand.Float64()
	}
	v := r.Float64()
	h.rndMu.Unlock()
	return v
}

func (h *Harness) randomHex(byteLen int) string {
	h.rndMu.Lock()
	r := h.rnd
	if r == nil {
		h.rndMu.Unlock()
		return cryptoHex(byteLen)
	}
	b := make([]byte, byteLen)
	_, _ = r.Read(b)
	h.rndMu.Unlock()
	return hex.EncodeToString(b)
}

// DefaultConfig returns a ready-to-use e-commerce topology.
func DefaultConfig() Config {
	return Config{
		Services: []Service{
			{
				Name: "api-gateway", Language: "go",
				Operations: []Operation{
					{Name: "ListProducts", SpanName: "GET /products", MinMs: 5, MaxMs: 20},
					{Name: "Checkout", SpanName: "POST /checkout", MinMs: 10, MaxMs: 50},
				},
				DependsOn: []Dependency{
					{Service: "product-service", Protocol: "grpc"},
					{Service: "order-service", Protocol: "http"},
				},
			},
			{
				Name: "product-service", Language: "java",
				Operations: []Operation{
					{Name: "GetProduct", SpanName: "GET /products/{id}", MinMs: 3, MaxMs: 15},
					{Name: "SearchProducts", SpanName: "GET /products/search", MinMs: 10, MaxMs: 80},
				},
				DependsOn: []Dependency{
					{DB: "products-db", DBSystem: "postgres"},
					{DB: "product-cache", DBSystem: "redis"},
				},
			},
			{
				Name: "order-service", Language: "python",
				Operations: []Operation{
					{Name: "CreateOrder", SpanName: "POST /orders", MinMs: 15, MaxMs: 60},
					{Name: "GetOrder", SpanName: "GET /orders/{id}", MinMs: 5, MaxMs: 20},
				},
				DependsOn: []Dependency{
					{Service: "payment-service", Protocol: "http"},
					{DB: "orders-db", DBSystem: "postgres"},
				},
			},
			{
				Name: "payment-service", Language: "nodejs",
				Operations: []Operation{
					{Name: "ProcessPayment", SpanName: "POST /payments", MinMs: 50, MaxMs: 200},
					{Name: "RefundPayment", SpanName: "POST /refunds", MinMs: 30, MaxMs: 150},
				},
				DependsOn: []Dependency{
					{DB: "payments-db", DBSystem: "postgres"},
				},
			},
			{
				Name: "notification-service", Language: "go",
				Operations: []Operation{
					{Name: "SendEmail", SpanName: "POST /notifications/email", MinMs: 20, MaxMs: 100},
				},
				DependsOn: []Dependency{},
			},
		},
		TraceRate: 2,
		MetricsMs: 2000,
		ErrorRate: 0.05,
	}
}

// Start begins generating telemetry in background goroutines.
func (h *Harness) Start() {
	h.wg.Add(2)
	go h.traceLoop()
	go h.metricsLoop()
	log.Printf("[testharness] started — %d services, %.0f traces/s, metrics every %dms, endpoint %s",
		len(h.cfg.Services), h.cfg.TraceRate, h.cfg.MetricsMs, h.cfg.Endpoint)
}

// Stop signals all goroutines to finish and waits.
func (h *Harness) Stop() {
	close(h.stop)
	h.wg.Wait()
	log.Println("[testharness] stopped")
}

// ---------- trace generation ----------

func (h *Harness) traceLoop() {
	defer h.wg.Done()
	interval := time.Duration(float64(time.Second) / h.cfg.TraceRate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			h.generateTrace()
		}
	}
}

func (h *Harness) generateTrace() {
	spans, logs := h.buildTrace(time.Now())
	if len(spans) > 0 {
		h.sendTraces(spans)
	}
	if len(logs) > 0 {
		h.sendLogs(logs)
	}
}

func (h *Harness) buildTrace(now time.Time) ([]spanData, []logData) {
	// Pick a random entry-point service (those not depended on by others).
	entries := h.findEntryPoints()
	if len(entries) == 0 {
		return nil, nil
	}
	entry := entries[h.intn(len(entries))]

	traceID := h.randomHex(16)

	// Decide if this trace has an error and pick the source.
	errorRate := h.cfg.ErrorRate
	var errorSource string
	// Check scenario overrides first
	for _, s := range h.cfg.Scenarios {
		if s.Type == "error_rate" && h.float64() < s.ErrorRate {
			errorSource = s.TargetService
			break
		}
	}
	if errorSource == "" && h.float64() < errorRate {
		errorSource = h.cfg.Services[h.intn(len(h.cfg.Services))].Name
	}

	var allSpans []spanData
	var allLogs []logData

	h.walkService(entry.Name, traceID, "", now, errorSource, &allSpans, &allLogs, map[string]bool{}, 0)
	return allSpans, allLogs
}

// EmitBatch generates n traces and one metrics snapshot, posts them to
// cfg.Endpoint when Endpoint is set, and returns the records it emitted.
// now is the clock for timestamps. The zero time means time.Now().
// A non-2xx or a transport error is returned. Do not swallow it on this path.
func (h *Harness) EmitBatch(n int, now time.Time) (Batch, error) {
	if now.IsZero() {
		now = time.Now()
	}
	batch := Batch{Tenant: h.cfg.Tenant}
	// Each trace starts a second later so span start times do not tie.
	for i := 0; i < n; i++ {
		spans, logs := h.buildTrace(now.Add(time.Duration(i) * time.Second))
		batch.Spans = append(batch.Spans, exportedSpans(spans)...)
		batch.Logs = append(batch.Logs, exportedLogs(logs)...)
		if err := h.postTrace(spans, logs); err != nil {
			return batch, err
		}
	}
	counters := map[string]int64{}
	errCounters := map[string]int64{}
	points, payload := h.buildMetrics(now, counters, errCounters)
	batch.Metrics = points
	if h.endpointSet {
		if err := h.postPayload("/v1/metrics", payload); err != nil {
			return batch, err
		}
	}
	return batch, nil
}

func (h *Harness) postTrace(spans []spanData, logs []logData) error {
	if !h.endpointSet {
		return nil
	}
	if len(spans) > 0 {
		if err := h.postPayload("/v1/traces", tracePayload(spans)); err != nil {
			return err
		}
	}
	if len(logs) > 0 {
		if err := h.postPayload("/v1/logs", logPayload(logs)); err != nil {
			return err
		}
	}
	return nil
}

func exportedSpans(spans []spanData) []Span {
	out := make([]Span, len(spans))
	for i, s := range spans {
		out[i] = Span{
			TraceID:      s.TraceID,
			SpanID:       s.SpanID,
			ParentSpanID: s.ParentSpanID,
			Service:      s.ServiceName,
			Name:         s.SpanName,
			Kind:         s.Kind,
			StartNano:    s.StartNano,
			EndNano:      s.EndNano,
			Status:       s.StatusCode,
		}
	}
	return out
}

func exportedLogs(logs []logData) []Log {
	out := make([]Log, len(logs))
	for i, l := range logs {
		out[i] = Log{
			Service:     l.ServiceName,
			Severity:    l.Severity,
			SeverityNum: l.SeverityNum,
			Body:        l.Body,
			TraceID:     l.TraceID,
			SpanID:      l.SpanID,
		}
	}
	return out
}

type spanData struct {
	TraceID       string
	SpanID        string
	ParentSpanID  string
	ServiceName   string
	SpanName      string
	Kind          int // 1=INTERNAL,2=SERVER,3=CLIENT
	StartNano     int64
	EndNano       int64
	StatusCode    int // 0=UNSET,1=OK,2=ERROR
	Attributes    map[string]interface{}
	ResourceAttrs map[string]string
}

type logData struct {
	Timestamp     int64
	ServiceName   string
	Severity      string
	SeverityNum   int
	Body          string
	TraceID       string
	SpanID        string
	ResourceAttrs map[string]string
}

func (h *Harness) walkService(
	svcName, traceID, parentSpanID string,
	startTime time.Time,
	errorSource string,
	spans *[]spanData, logs *[]logData,
	visited map[string]bool, depth int,
) (endTime time.Time, hadError bool) {
	if visited[svcName] || depth > 15 {
		return startTime, false
	}
	visited[svcName] = true

	svc, ok := h.svcMap[svcName]
	if !ok {
		return startTime, false
	}

	spanID := h.randomHex(8)

	// Pick an operation
	var op Operation
	if len(svc.Operations) > 0 {
		op = svc.Operations[h.intn(len(svc.Operations))]
	} else {
		op = Operation{Name: svcName, SpanName: svcName + " process", MinMs: 5, MaxMs: 30}
	}

	// Calculate own processing time
	ownMs := op.MinMs + h.intn(max(op.MaxMs-op.MinMs, 1)+1)
	// Apply scenario latency multiplier
	if sc, ok := h.scenMap[svcName]; ok && sc.Type == "latency_spike" {
		ownMs = int(float64(ownMs) * sc.LatencyMultiple)
	}
	ownDuration := time.Duration(ownMs) * time.Millisecond

	isErrorSource := (errorSource == svcName)
	childStart := startTime.Add(time.Duration(h.intn(2)+1) * time.Millisecond)
	latestEnd := childStart
	downstreamError := false

	resAttrs := h.resourceAttrs(svc)

	// Walk dependencies
	for _, dep := range svc.DependsOn {
		if dep.Service != "" {
			// Service dependency — create client span + recurse
			clientSpanID := h.randomHex(8)
			clientAttrs := map[string]interface{}{
				"net.peer.name": dep.Service,
			}
			if dep.Protocol == "grpc" {
				clientAttrs["rpc.system"] = "grpc"
				clientAttrs["rpc.service"] = PascalCase(dep.Service) + "Service"
				clientAttrs["rpc.method"] = "Process"
			} else {
				clientAttrs["http.request.method"] = "GET"
				clientAttrs["url.path"] = "/" + dep.Service
			}

			downstreamStart := childStart.Add(time.Duration(h.intn(2)+1) * time.Millisecond)
			depEnd, depError := h.walkService(dep.Service, traceID, clientSpanID, downstreamStart, errorSource, spans, logs, visited, depth+1)
			if depError {
				downstreamError = true
			}

			clientStatus := 1 // OK
			if depError {
				clientStatus = 2 // ERROR
				if dep.Protocol != "grpc" {
					clientAttrs["http.response.status_code"] = 500
				}
			}

			*spans = append(*spans, spanData{
				TraceID: traceID, SpanID: clientSpanID, ParentSpanID: spanID,
				ServiceName: svcName, SpanName: fmt.Sprintf("HTTP %s", dep.Service),
				Kind: 3, StartNano: childStart.UnixNano(), EndNano: depEnd.UnixNano(),
				StatusCode: clientStatus, Attributes: clientAttrs, ResourceAttrs: resAttrs,
			})

			if depEnd.After(latestEnd) {
				latestEnd = depEnd
			}
			childStart = depEnd.Add(time.Millisecond)

		} else if dep.DB != "" {
			// DB dependency — create client span
			dbSpanID := h.randomHex(8)
			dbMs := 2 + h.intn(30)
			if sc, ok := h.scenMap[svcName]; ok && sc.Type == "latency_spike" {
				dbMs = int(float64(dbMs) * sc.LatencyMultiple)
			}
			dbEnd := childStart.Add(time.Duration(dbMs) * time.Millisecond)

			dbAttrs := map[string]interface{}{
				"db.system":     dep.DBSystem,
				"db.name":       dep.DB,
				"net.peer.name": dep.DB,
			}
			switch dep.DBSystem {
			case "redis":
				dbAttrs["db.statement"] = fmt.Sprintf("GET session:%s", h.randomHex(4))
				dbAttrs["db.operation"] = "GET"
			default:
				dbAttrs["db.statement"] = "SELECT * FROM items WHERE id = $1"
				dbAttrs["db.operation"] = "SELECT"
			}

			*spans = append(*spans, spanData{
				TraceID: traceID, SpanID: dbSpanID, ParentSpanID: spanID,
				ServiceName: svcName, SpanName: fmt.Sprintf("QUERY %s", dep.DB),
				Kind: 3, StartNano: childStart.UnixNano(), EndNano: dbEnd.UnixNano(),
				StatusCode: 1, Attributes: dbAttrs, ResourceAttrs: resAttrs,
			})

			if dbEnd.After(latestEnd) {
				latestEnd = dbEnd
			}
			childStart = dbEnd.Add(time.Millisecond)
		}
	}

	totalError := isErrorSource || downstreamError
	serverEnd := latestEnd
	if startTime.Add(ownDuration).After(serverEnd) {
		serverEnd = startTime.Add(ownDuration)
	}

	statusCode := 1 // OK
	if totalError {
		statusCode = 2 // ERROR
	}

	serverAttrs := map[string]interface{}{
		"http.request.method":       strings.Split(op.SpanName, " ")[0],
		"url.path":                  "/" + svcName,
		"http.response.status_code": 200,
	}
	if totalError {
		serverAttrs["http.response.status_code"] = 500
	}

	*spans = append(*spans, spanData{
		TraceID: traceID, SpanID: spanID, ParentSpanID: parentSpanID,
		ServiceName: svcName, SpanName: op.SpanName,
		Kind: 2, StartNano: startTime.UnixNano(), EndNano: serverEnd.UnixNano(),
		StatusCode: statusCode, Attributes: serverAttrs, ResourceAttrs: resAttrs,
	})

	// Generate logs
	*logs = append(*logs, logData{
		Timestamp: serverEnd.UnixNano(), ServiceName: svcName,
		Severity: "INFO", SeverityNum: 9,
		Body:    fmt.Sprintf("Handled %s in %dms", op.SpanName, serverEnd.Sub(startTime).Milliseconds()),
		TraceID: traceID, SpanID: spanID, ResourceAttrs: resAttrs,
	})

	if sc, ok := h.scenMap[svcName]; ok && sc.Type == "latency_spike" {
		*logs = append(*logs, logData{
			Timestamp: serverEnd.UnixNano(), ServiceName: svcName,
			Severity: "WARN", SeverityNum: 13,
			Body:    fmt.Sprintf("latency above baseline for %s", op.SpanName),
			TraceID: traceID, SpanID: spanID, ResourceAttrs: resAttrs,
		})
	}

	if isErrorSource {
		*logs = append(*logs, logData{
			Timestamp: serverEnd.UnixNano(), ServiceName: svcName,
			Severity: "WARN", SeverityNum: 13,
			Body:    fmt.Sprintf("degraded %s: retrying after upstream failure", op.SpanName),
			TraceID: traceID, SpanID: spanID, ResourceAttrs: resAttrs,
		})
		errMsgs := []string{
			"ConnectionTimeoutException: upstream service unreachable",
			"NullPointerException: attempt to invoke method on null object",
			"PaymentProcessingException: gateway rejected transaction",
			"DatabaseExecutionException: deadlock detected",
			"ServiceUnavailableException: 503 from downstream",
		}
		*logs = append(*logs, logData{
			Timestamp: serverEnd.UnixNano(), ServiceName: svcName,
			Severity: "ERROR", SeverityNum: 17,
			Body:    errMsgs[h.intn(len(errMsgs))],
			TraceID: traceID, SpanID: spanID, ResourceAttrs: resAttrs,
		})
	}

	return serverEnd, totalError
}

func (h *Harness) resourceAttrs(svc *Service) map[string]string {
	attrs := map[string]string{
		"service.name":           svc.Name,
		"service.version":        "1.0.0",
		"telemetry.sdk.language": svc.Language,
		"telemetry.sdk.name":     "opentelemetry",
		"deployment.environment": "production",
		"host.name":              fmt.Sprintf("k8s-node-%s", svc.Name),
		"k8s.namespace.name":     "default",
		"k8s.pod.name":           fmt.Sprintf("%s-7f8b9c-x4k2p", svc.Name),
	}
	if h.cfg.Tenant != "" {
		attrs["tenant.id"] = h.cfg.Tenant
	}
	return attrs
}

func (h *Harness) findEntryPoints() []Service {
	depOf := map[string]bool{}
	for _, svc := range h.cfg.Services {
		for _, d := range svc.DependsOn {
			if d.Service != "" {
				depOf[d.Service] = true
			}
		}
	}
	var entries []Service
	for _, svc := range h.cfg.Services {
		if !depOf[svc.Name] {
			entries = append(entries, svc)
		}
	}
	if len(entries) == 0 {
		return h.cfg.Services
	}
	return entries
}

// ---------- metrics generation ----------

func (h *Harness) metricsLoop() {
	defer h.wg.Done()
	ticker := time.NewTicker(time.Duration(h.cfg.MetricsMs) * time.Millisecond)
	defer ticker.Stop()

	counters := make(map[string]int64) // request counters per service
	errCounters := make(map[string]int64)

	// Send one batch immediately so a short --duration (e.g. 5s) still
	// produces metrics; waiting for the first tick used to leave metrics empty.
	h.sendMetrics(time.Now(), counters, errCounters)

	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			h.sendMetrics(time.Now(), counters, errCounters)
		}
	}
}

func (h *Harness) sendMetrics(now time.Time, counters, errCounters map[string]int64) {
	_, payload := h.buildMetrics(now, counters, errCounters)
	h.post("/v1/metrics", payload)
}

func (h *Harness) buildMetrics(now time.Time, counters, errCounters map[string]int64) ([]Metric, map[string]interface{}) {
	nowStr := fmt.Sprintf("%d", now.UnixNano())
	hourAgo := fmt.Sprintf("%d", now.Add(-time.Hour).UnixNano())
	cat := metricCatalog()

	var points []Metric
	var resourceMetrics []interface{}

	for _, svc := range h.cfg.Services {
		counters[svc.Name] += int64(5 + h.intn(15))

		errRate := h.cfg.ErrorRate
		if sc, ok := h.scenMap[svc.Name]; ok && sc.Type == "error_rate" {
			errRate = sc.ErrorRate
		}
		if h.float64() < errRate {
			errCounters[svc.Name]++
		}

		cpuUtil := 0.1 + h.float64()*0.6
		memUsage := 200_000_000 + h.int63n(600_000_000)
		if sc, ok := h.scenMap[svc.Name]; ok {
			if sc.Type == "latency_spike" {
				cpuUtil = 0.7 + h.float64()*0.3
			}
		}
		values := []float64{
			cpuUtil,
			float64(memUsage),
			float64(counters[svc.Name]),
			float64(errCounters[svc.Name]),
		}

		metrics := make([]interface{}, 0, len(cat))
		for i, def := range cat {
			points = append(points, Metric{Name: def.name, Service: svc.Name, Value: values[i]})
			metrics = append(metrics, otlpMetric(def, values[i], nowStr, hourAgo))
		}

		resAttrs := h.resourceAttrs(&svc)
		resourceMetrics = append(resourceMetrics, map[string]interface{}{
			"resource": map[string]interface{}{
				"attributes": formatResourceAttrs(resAttrs),
			},
			"scopeMetrics": []map[string]interface{}{
				{
					"scope":   map[string]string{"name": "ducktel-testharness"},
					"metrics": metrics,
				},
			},
		})
	}

	return points, map[string]interface{}{"resourceMetrics": resourceMetrics}
}

func otlpMetric(def metricDef, value float64, now, hourAgo string) map[string]interface{} {
	switch def.kind {
	case metricSum:
		asInt := fmt.Sprintf("%d", int64(value))
		return map[string]interface{}{
			"name": def.name, "unit": def.unit,
			"sum": map[string]interface{}{
				"isMonotonic": true, "aggregationTemporality": 2,
				"dataPoints": []map[string]interface{}{
					{"timeUnixNano": now, "startTimeUnixNano": hourAgo, "asInt": asInt},
				},
			},
		}
	default:
		point := map[string]interface{}{"timeUnixNano": now}
		if def.asInt {
			point["asInt"] = fmt.Sprintf("%d", int64(value))
		} else {
			point["asDouble"] = value
		}
		return map[string]interface{}{
			"name": def.name, "unit": def.unit,
			"gauge": map[string]interface{}{
				"dataPoints": []map[string]interface{}{point},
			},
		}
	}
}

// ---------- OTLP formatting + sending ----------

func (h *Harness) sendTraces(spans []spanData) {
	h.post("/v1/traces", tracePayload(spans))
}

func tracePayload(spans []spanData) map[string]interface{} {
	byService := map[string][]spanData{}
	for _, s := range spans {
		byService[s.ServiceName] = append(byService[s.ServiceName], s)
	}

	var resourceSpans []interface{}
	for _, svcName := range sortedKeys(byService) {
		svcSpans := byService[svcName]
		var otlpSpans []interface{}
		for _, s := range svcSpans {
			sp := map[string]interface{}{
				"traceId":           s.TraceID,
				"spanId":            s.SpanID,
				"name":              s.SpanName,
				"kind":              s.Kind,
				"startTimeUnixNano": fmt.Sprintf("%d", s.StartNano),
				"endTimeUnixNano":   fmt.Sprintf("%d", s.EndNano),
				"status":            map[string]interface{}{"code": s.StatusCode},
				"attributes":        formatAttrs(s.Attributes),
			}
			if s.ParentSpanID != "" {
				sp["parentSpanId"] = s.ParentSpanID
			}
			otlpSpans = append(otlpSpans, sp)
		}

		resAttrs := svcSpans[0].ResourceAttrs
		if resAttrs == nil {
			resAttrs = map[string]string{"service.name": svcName}
		}

		resourceSpans = append(resourceSpans, map[string]interface{}{
			"resource": map[string]interface{}{
				"attributes": formatResourceAttrs(resAttrs),
			},
			"scopeSpans": []map[string]interface{}{
				{
					"scope": map[string]string{"name": "ducktel-testharness"},
					"spans": otlpSpans,
				},
			},
		})
	}

	return map[string]interface{}{"resourceSpans": resourceSpans}
}

func (h *Harness) sendLogs(entries []logData) {
	h.post("/v1/logs", logPayload(entries))
}

func logPayload(entries []logData) map[string]interface{} {
	byService := map[string][]logData{}
	for _, l := range entries {
		byService[l.ServiceName] = append(byService[l.ServiceName], l)
	}

	var resourceLogs []interface{}
	for _, svcName := range sortedKeys(byService) {
		svcLogs := byService[svcName]
		var records []interface{}
		for _, l := range svcLogs {
			rec := map[string]interface{}{
				"timeUnixNano":   fmt.Sprintf("%d", l.Timestamp),
				"severityText":   l.Severity,
				"severityNumber": l.SeverityNum,
				"body":           map[string]interface{}{"stringValue": l.Body},
			}
			if l.TraceID != "" {
				rec["traceId"] = l.TraceID
			}
			if l.SpanID != "" {
				rec["spanId"] = l.SpanID
			}
			records = append(records, rec)
		}

		resAttrs := svcLogs[0].ResourceAttrs
		if resAttrs == nil {
			resAttrs = map[string]string{"service.name": svcName}
		}

		resourceLogs = append(resourceLogs, map[string]interface{}{
			"resource": map[string]interface{}{
				"attributes": formatResourceAttrs(resAttrs),
			},
			"scopeLogs": []map[string]interface{}{
				{
					"scope":      map[string]string{"name": "ducktel-testharness"},
					"logRecords": records,
				},
			},
		})
	}

	return map[string]interface{}{"resourceLogs": resourceLogs}
}

func (h *Harness) post(path string, payload interface{}) {
	if err := h.postPayload(path, payload); err != nil {
		log.Printf("[testharness] %s", err.Error())
	}
}

func (h *Harness) postPayload(path string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal error: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, h.cfg.Endpoint+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("request error (%s): %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Present the bearer token when configured, so the generator can drive a
	// receiver started with --auth-token.
	if h.cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("send error (%s): %v", path, err)
	}
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d for %s", resp.StatusCode, path)
	}
	return nil
}

// ---------- helpers ----------

func formatAttrs(attrs map[string]interface{}) []map[string]interface{} {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []map[string]interface{}
	for _, k := range keys {
		v := attrs[k]
		var val map[string]interface{}
		switch tv := v.(type) {
		case string:
			val = map[string]interface{}{"stringValue": tv}
		case int:
			val = map[string]interface{}{"intValue": fmt.Sprintf("%d", tv)}
		case int64:
			val = map[string]interface{}{"intValue": fmt.Sprintf("%d", tv)}
		case float64:
			val = map[string]interface{}{"doubleValue": tv}
		case bool:
			val = map[string]interface{}{"boolValue": tv}
		default:
			val = map[string]interface{}{"stringValue": fmt.Sprintf("%v", tv)}
		}
		out = append(out, map[string]interface{}{"key": k, "value": val})
	}
	return out
}

func formatResourceAttrs(attrs map[string]string) []map[string]interface{} {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []map[string]interface{}
	for _, k := range keys {
		v := attrs[k]
		out = append(out, map[string]interface{}{
			"key": k, "value": map[string]interface{}{"stringValue": v},
		})
	}
	return out
}

// PascalCase upper-cases the first letter of each hyphen-separated segment and
// joins them: "product-service" -> "ProductService". Replaces the deprecated
// strings.Title, whose Unicode word-boundary rules do not match this intent.
func PascalCase(s string) string {
	parts := strings.Split(s, "-")
	var b strings.Builder
	b.Grow(len(s))
	for _, p := range parts {
		if p == "" {
			continue
		}
		r := []rune(p)
		b.WriteString(strings.ToUpper(string(r[0])))
		b.WriteString(string(r[1:]))
	}
	return b.String()
}

func cryptoHex(byteLen int) string {
	b := make([]byte, byteLen)
	if _, err := cryptorand.Read(b); err != nil {
		// fallback
		n, _ := cryptorand.Int(cryptorand.Reader, big.NewInt(1<<62))
		return fmt.Sprintf("%032x", n)[:byteLen*2]
	}
	return hex.EncodeToString(b)
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
