// Package testharness generates synthetic OpenTelemetry data and sends it to
// ducktel's OTLP receiver. It simulates a realistic microservice topology with
// traces, logs, and metrics — including injectable failure scenarios.
//
// Inspired by github.com/davidgeorgehope/otel-demo-gen but written in Go to
// live inside the ducktel repo as a first-class testing tool.
package testharness

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	mathrand "math/rand"
	"net/http"
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
	Endpoint  string     // OTLP HTTP endpoint (default http://localhost:4318)
	Services  []Service
	Scenarios []Scenario
	TraceRate float64    // traces per second (default 2)
	MetricsMs int        // metrics interval in ms (default 10000)
	ErrorRate float64    // baseline error rate (default 0.05)
}

// Harness generates and sends synthetic telemetry.
type Harness struct {
	cfg    Config
	client *http.Client
	stop   chan struct{}
	wg     sync.WaitGroup

	svcMap   map[string]*Service
	scenMap  map[string]*Scenario // service -> scenario
}

// New creates a harness from config, applying defaults.
func New(cfg Config) *Harness {
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
	if cfg.ErrorRate <= 0 {
		cfg.ErrorRate = 0.05
	}

	h := &Harness{
		cfg:     cfg,
		client:  &http.Client{Timeout: 5 * time.Second},
		stop:    make(chan struct{}),
		svcMap:  make(map[string]*Service),
		scenMap: make(map[string]*Scenario),
	}
	for i := range cfg.Services {
		h.svcMap[cfg.Services[i].Name] = &cfg.Services[i]
	}
	for i := range cfg.Scenarios {
		h.scenMap[cfg.Scenarios[i].TargetService] = &cfg.Scenarios[i]
	}
	return h
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
		MetricsMs: 10000,
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
	// Pick a random entry-point service (those not depended on by others).
	entries := h.findEntryPoints()
	if len(entries) == 0 {
		return
	}
	entry := entries[mathrand.Intn(len(entries))]

	traceID := randomHex(16)

	// Decide if this trace has an error and pick the source.
	errorRate := h.cfg.ErrorRate
	var errorSource string
	// Check scenario overrides first
	for _, s := range h.cfg.Scenarios {
		if s.Type == "error_rate" && mathrand.Float64() < s.ErrorRate {
			errorSource = s.TargetService
			break
		}
	}
	if errorSource == "" && mathrand.Float64() < errorRate {
		errorSource = h.cfg.Services[mathrand.Intn(len(h.cfg.Services))].Name
	}

	var allSpans []spanData
	var allLogs []logData

	h.walkService(entry.Name, traceID, "", time.Now(), errorSource, &allSpans, &allLogs, map[string]bool{}, 0)

	if len(allSpans) > 0 {
		h.sendTraces(allSpans)
	}
	if len(allLogs) > 0 {
		h.sendLogs(allLogs)
	}
}

type spanData struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	ServiceName  string
	SpanName     string
	Kind         int // 1=INTERNAL,2=SERVER,3=CLIENT
	StartNano    int64
	EndNano      int64
	StatusCode   int // 0=UNSET,1=OK,2=ERROR
	Attributes   map[string]interface{}
	ResourceAttrs map[string]string
}

type logData struct {
	Timestamp    int64
	ServiceName  string
	Severity     string
	SeverityNum  int
	Body         string
	TraceID      string
	SpanID       string
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

	spanID := randomHex(8)

	// Pick an operation
	var op Operation
	if len(svc.Operations) > 0 {
		op = svc.Operations[mathrand.Intn(len(svc.Operations))]
	} else {
		op = Operation{Name: svcName, SpanName: svcName + " process", MinMs: 5, MaxMs: 30}
	}

	// Calculate own processing time
	ownMs := op.MinMs + mathrand.Intn(max(op.MaxMs-op.MinMs, 1)+1)
	// Apply scenario latency multiplier
	if sc, ok := h.scenMap[svcName]; ok && sc.Type == "latency_spike" {
		ownMs = int(float64(ownMs) * sc.LatencyMultiple)
	}
	ownDuration := time.Duration(ownMs) * time.Millisecond

	isErrorSource := (errorSource == svcName)
	childStart := startTime.Add(time.Duration(mathrand.Intn(2)+1) * time.Millisecond)
	latestEnd := childStart
	downstreamError := false

	resAttrs := h.resourceAttrs(svc)

	// Walk dependencies
	for _, dep := range svc.DependsOn {
		if dep.Service != "" {
			// Service dependency — create client span + recurse
			clientSpanID := randomHex(8)
			clientAttrs := map[string]interface{}{
				"net.peer.name": dep.Service,
			}
			if dep.Protocol == "grpc" {
				clientAttrs["rpc.system"] = "grpc"
				clientAttrs["rpc.service"] = strings.ReplaceAll(strings.Title(dep.Service), "-", "") + "Service"
				clientAttrs["rpc.method"] = "Process"
			} else {
				clientAttrs["http.request.method"] = "GET"
				clientAttrs["url.path"] = "/" + dep.Service
			}

			downstreamStart := childStart.Add(time.Duration(mathrand.Intn(2)+1) * time.Millisecond)
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
			dbSpanID := randomHex(8)
			dbMs := 2 + mathrand.Intn(30)
			if sc, ok := h.scenMap[svcName]; ok && sc.Type == "latency_spike" {
				dbMs = int(float64(dbMs) * sc.LatencyMultiple)
			}
			dbEnd := childStart.Add(time.Duration(dbMs) * time.Millisecond)

			dbAttrs := map[string]interface{}{
				"db.system":    dep.DBSystem,
				"db.name":      dep.DB,
				"net.peer.name": dep.DB,
			}
			switch dep.DBSystem {
			case "redis":
				dbAttrs["db.statement"] = fmt.Sprintf("GET session:%s", randomHex(4))
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

	if isErrorSource {
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
			Body:    errMsgs[mathrand.Intn(len(errMsgs))],
			TraceID: traceID, SpanID: spanID, ResourceAttrs: resAttrs,
		})
	}

	return serverEnd, totalError
}

func (h *Harness) resourceAttrs(svc *Service) map[string]string {
	return map[string]string{
		"service.name":             svc.Name,
		"service.version":          "1.0.0",
		"telemetry.sdk.language":   svc.Language,
		"telemetry.sdk.name":       "opentelemetry",
		"deployment.environment":   "production",
		"host.name":               fmt.Sprintf("k8s-node-%s", svc.Name),
		"k8s.namespace.name":       "default",
		"k8s.pod.name":            fmt.Sprintf("%s-7f8b9c-x4k2p", svc.Name),
	}
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

	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			h.generateMetrics(counters, errCounters)
		}
	}
}

func (h *Harness) generateMetrics(counters, errCounters map[string]int64) {
	now := fmt.Sprintf("%d", time.Now().UnixNano())
	hourAgo := fmt.Sprintf("%d", time.Now().Add(-time.Hour).UnixNano())

	var resourceMetrics []interface{}

	for _, svc := range h.cfg.Services {
		counters[svc.Name] += int64(5 + mathrand.Intn(15))

		errRate := h.cfg.ErrorRate
		if sc, ok := h.scenMap[svc.Name]; ok && sc.Type == "error_rate" {
			errRate = sc.ErrorRate
		}
		if mathrand.Float64() < errRate {
			errCounters[svc.Name]++
		}

		cpuUtil := 0.1 + mathrand.Float64()*0.6
		memUsage := 200_000_000 + mathrand.Int63n(600_000_000)
		if sc, ok := h.scenMap[svc.Name]; ok {
			if sc.Type == "latency_spike" {
				cpuUtil = 0.7 + mathrand.Float64()*0.3
			}
		}

		metrics := []interface{}{
			map[string]interface{}{
				"name": "system.cpu.utilization", "unit": "%",
				"gauge": map[string]interface{}{
					"dataPoints": []map[string]interface{}{
						{"timeUnixNano": now, "asDouble": cpuUtil},
					},
				},
			},
			map[string]interface{}{
				"name": "process.memory.usage", "unit": "By",
				"gauge": map[string]interface{}{
					"dataPoints": []map[string]interface{}{
						{"timeUnixNano": now, "asInt": fmt.Sprintf("%d", memUsage)},
					},
				},
			},
			map[string]interface{}{
				"name": "http.server.request.count", "unit": "requests",
				"sum": map[string]interface{}{
					"isMonotonic": true, "aggregationTemporality": 2,
					"dataPoints": []map[string]interface{}{
						{"timeUnixNano": now, "startTimeUnixNano": hourAgo, "asInt": fmt.Sprintf("%d", counters[svc.Name])},
					},
				},
			},
			map[string]interface{}{
				"name": "http.server.request.error.count", "unit": "errors",
				"sum": map[string]interface{}{
					"isMonotonic": true, "aggregationTemporality": 2,
					"dataPoints": []map[string]interface{}{
						{"timeUnixNano": now, "startTimeUnixNano": hourAgo, "asInt": fmt.Sprintf("%d", errCounters[svc.Name])},
					},
				},
			},
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

	payload := map[string]interface{}{"resourceMetrics": resourceMetrics}
	h.post("/v1/metrics", payload)
}

// ---------- OTLP formatting + sending ----------

func (h *Harness) sendTraces(spans []spanData) {
	// Group by service
	byService := map[string][]spanData{}
	for _, s := range spans {
		byService[s.ServiceName] = append(byService[s.ServiceName], s)
	}

	var resourceSpans []interface{}
	for svcName, svcSpans := range byService {
		var otlpSpans []interface{}
		for _, s := range svcSpans {
			sp := map[string]interface{}{
				"traceId":            s.TraceID,
				"spanId":             s.SpanID,
				"name":               s.SpanName,
				"kind":               s.Kind,
				"startTimeUnixNano":  fmt.Sprintf("%d", s.StartNano),
				"endTimeUnixNano":    fmt.Sprintf("%d", s.EndNano),
				"status":             map[string]interface{}{"code": s.StatusCode},
				"attributes":         formatAttrs(s.Attributes),
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

	payload := map[string]interface{}{"resourceSpans": resourceSpans}
	h.post("/v1/traces", payload)
}

func (h *Harness) sendLogs(entries []logData) {
	byService := map[string][]logData{}
	for _, l := range entries {
		byService[l.ServiceName] = append(byService[l.ServiceName], l)
	}

	var resourceLogs []interface{}
	for svcName, svcLogs := range byService {
		var records []interface{}
		for _, l := range svcLogs {
			rec := map[string]interface{}{
				"timeUnixNano":  fmt.Sprintf("%d", l.Timestamp),
				"severityText":  l.Severity,
				"severityNumber": l.SeverityNum,
				"body":          map[string]interface{}{"stringValue": l.Body},
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

	payload := map[string]interface{}{"resourceLogs": resourceLogs}
	h.post("/v1/logs", payload)
}

func (h *Harness) post(path string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[testharness] marshal error: %v", err)
		return
	}

	resp, err := h.client.Post(h.cfg.Endpoint+path, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("[testharness] send error (%s): %v", path, err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 300 {
		log.Printf("[testharness] unexpected status %d for %s", resp.StatusCode, path)
	}
}

// ---------- helpers ----------

func formatAttrs(attrs map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for k, v := range attrs {
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
	var out []map[string]interface{}
	for k, v := range attrs {
		out = append(out, map[string]interface{}{
			"key": k, "value": map[string]interface{}{"stringValue": v},
		})
	}
	return out
}

func randomHex(byteLen int) string {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		// fallback
		n, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
		return fmt.Sprintf("%032x", n)[:byteLen*2]
	}
	return hex.EncodeToString(b)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
