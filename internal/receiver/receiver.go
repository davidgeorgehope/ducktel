package receiver

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"

	collectlogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectmetricsv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collecttracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/davidhope/otelite/internal/writer"
)

type Consumer interface {
	Add(spans []writer.TraceSpan)
	AddLogs(records []writer.LogRecord)
	AddMetrics(points []writer.MetricPoint)
}

type Receiver struct {
	server   *http.Server
	consumer Consumer
}

func New(port int, consumer Consumer) *Receiver {
	r := &Receiver{consumer: consumer}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/traces", r.handleTraces)
	mux.HandleFunc("POST /v1/logs", r.handleLogs)
	mux.HandleFunc("POST /v1/metrics", r.handleMetrics)
	mux.HandleFunc("GET /", r.handleHealth)

	r.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}
	return r
}

func (r *Receiver) Start() error {
	log.Printf("OTLP receiver listening on %s", r.server.Addr)
	if err := r.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (r *Receiver) Stop(ctx context.Context) error {
	return r.server.Shutdown(ctx)
}

func (r *Receiver) handleHealth(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// --- Traces ---

func (r *Receiver) handleTraces(w http.ResponseWriter, req *http.Request) {
	exportReq := &collecttracev1.ExportTraceServiceRequest{}
	if err := unmarshalOTLP(req, exportReq); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	spans := convertSpans(exportReq)
	if len(spans) > 0 {
		r.consumer.Add(spans)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{}`))
}

func convertSpans(req *collecttracev1.ExportTraceServiceRequest) []writer.TraceSpan {
	var result []writer.TraceSpan

	for _, rs := range req.GetResourceSpans() {
		resourceAttrs := rs.GetResource().GetAttributes()
		serviceName := extractServiceName(resourceAttrs)
		resourceAttrsJSON := attributesToJSON(resourceAttrs)

		for _, ss := range rs.GetScopeSpans() {
			scopeName, scopeVersion := extractScope(ss.GetScope())

			for _, span := range ss.GetSpans() {
				startNano := span.GetStartTimeUnixNano()
				endNano := span.GetEndTimeUnixNano()

				statusCode := "UNSET"
				statusMessage := ""
				if s := span.GetStatus(); s != nil {
					statusCode = s.GetCode().String()
					statusMessage = s.GetMessage()
				}

				result = append(result, writer.TraceSpan{
					TraceID:            hex.EncodeToString(span.GetTraceId()),
					SpanID:             hex.EncodeToString(span.GetSpanId()),
					ParentSpanID:       hex.EncodeToString(span.GetParentSpanId()),
					TraceState:         span.GetTraceState(),
					ServiceName:        serviceName,
					SpanName:           span.GetName(),
					SpanKind:           span.GetKind().String(),
					StartTime:          int64(startNano / 1000),
					EndTime:            int64(endNano / 1000),
					DurationMs:         float64(endNano-startNano) / 1e6,
					StatusCode:         statusCode,
					StatusMessage:      statusMessage,
					Attributes:         attributesToJSON(span.GetAttributes()),
					ResourceAttributes: resourceAttrsJSON,
					ScopeName:          scopeName,
					ScopeVersion:       scopeVersion,
					Events:             eventsToJSON(span.GetEvents()),
					Links:              linksToJSON(span.GetLinks()),
				})
			}
		}
	}
	return result
}

// --- Logs ---

func (r *Receiver) handleLogs(w http.ResponseWriter, req *http.Request) {
	exportReq := &collectlogsv1.ExportLogsServiceRequest{}
	if err := unmarshalOTLP(req, exportReq); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	records := convertLogs(exportReq)
	if len(records) > 0 {
		r.consumer.AddLogs(records)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{}`))
}

func convertLogs(req *collectlogsv1.ExportLogsServiceRequest) []writer.LogRecord {
	var result []writer.LogRecord

	for _, rl := range req.GetResourceLogs() {
		resourceAttrs := rl.GetResource().GetAttributes()
		serviceName := extractServiceName(resourceAttrs)
		resourceAttrsJSON := attributesToJSON(resourceAttrs)

		for _, sl := range rl.GetScopeLogs() {
			scopeName, scopeVersion := extractScope(sl.GetScope())

			for _, lr := range sl.GetLogRecords() {
				body := ""
				if b := lr.GetBody(); b != nil {
					body = anyValueToString(b)
				}

				result = append(result, writer.LogRecord{
					Timestamp:          int64(lr.GetTimeUnixNano() / 1000),
					ObservedTimestamp:  int64(lr.GetObservedTimeUnixNano() / 1000),
					TraceID:            hex.EncodeToString(lr.GetTraceId()),
					SpanID:             hex.EncodeToString(lr.GetSpanId()),
					SeverityNumber:     int32(lr.GetSeverityNumber()),
					SeverityText:       lr.GetSeverityText(),
					Body:               body,
					Attributes:         attributesToJSON(lr.GetAttributes()),
					ResourceAttributes: resourceAttrsJSON,
					ServiceName:        serviceName,
					ScopeName:          scopeName,
					ScopeVersion:       scopeVersion,
					Flags:              lr.GetFlags(),
					EventName:          lr.GetEventName(),
				})
			}
		}
	}
	return result
}

// --- Metrics ---

func (r *Receiver) handleMetrics(w http.ResponseWriter, req *http.Request) {
	exportReq := &collectmetricsv1.ExportMetricsServiceRequest{}
	if err := unmarshalOTLP(req, exportReq); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	points := convertMetrics(exportReq)
	if len(points) > 0 {
		r.consumer.AddMetrics(points)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{}`))
}

func convertMetrics(req *collectmetricsv1.ExportMetricsServiceRequest) []writer.MetricPoint {
	var result []writer.MetricPoint

	for _, rm := range req.GetResourceMetrics() {
		resourceAttrs := rm.GetResource().GetAttributes()
		serviceName := extractServiceName(resourceAttrs)
		resourceAttrsJSON := attributesToJSON(resourceAttrs)

		for _, sm := range rm.GetScopeMetrics() {
			scopeName, scopeVersion := extractScope(sm.GetScope())

			for _, m := range sm.GetMetrics() {
				base := writer.MetricPoint{
					MetricName:         m.GetName(),
					MetricDescription:  m.GetDescription(),
					MetricUnit:         m.GetUnit(),
					ResourceAttributes: resourceAttrsJSON,
					ServiceName:        serviceName,
					ScopeName:          scopeName,
					ScopeVersion:       scopeVersion,
				}

				switch data := m.GetData().(type) {
				case *metricsv1.Metric_Gauge:
					for _, dp := range data.Gauge.GetDataPoints() {
						p := base
						p.MetricType = "gauge"
						fillNumberDataPoint(&p, dp)
						result = append(result, p)
					}
				case *metricsv1.Metric_Sum:
					for _, dp := range data.Sum.GetDataPoints() {
						p := base
						p.MetricType = "sum"
						p.IsMonotonic = data.Sum.GetIsMonotonic()
						p.AggregationTemporality = data.Sum.GetAggregationTemporality().String()
						fillNumberDataPoint(&p, dp)
						result = append(result, p)
					}
				case *metricsv1.Metric_Histogram:
					for _, dp := range data.Histogram.GetDataPoints() {
						p := base
						p.MetricType = "histogram"
						p.AggregationTemporality = data.Histogram.GetAggregationTemporality().String()
						fillHistogramDataPoint(&p, dp)
						result = append(result, p)
					}
				case *metricsv1.Metric_ExponentialHistogram:
					for _, dp := range data.ExponentialHistogram.GetDataPoints() {
						p := base
						p.MetricType = "exponential_histogram"
						p.AggregationTemporality = data.ExponentialHistogram.GetAggregationTemporality().String()
						fillExpHistogramDataPoint(&p, dp)
						result = append(result, p)
					}
				case *metricsv1.Metric_Summary:
					for _, dp := range data.Summary.GetDataPoints() {
						p := base
						p.MetricType = "summary"
						fillSummaryDataPoint(&p, dp)
						result = append(result, p)
					}
				}
			}
		}
	}
	return result
}

func fillNumberDataPoint(p *writer.MetricPoint, dp *metricsv1.NumberDataPoint) {
	p.Timestamp = int64(dp.GetTimeUnixNano() / 1000)
	p.StartTimestamp = int64(dp.GetStartTimeUnixNano() / 1000)
	p.Attributes = attributesToJSON(dp.GetAttributes())
	p.Flags = dp.GetFlags()
	p.Exemplars = exemplarsToJSON(dp.GetExemplars())

	switch v := dp.GetValue().(type) {
	case *metricsv1.NumberDataPoint_AsDouble:
		p.ValueDouble = v.AsDouble
	case *metricsv1.NumberDataPoint_AsInt:
		p.ValueInt = v.AsInt
		p.ValueDouble = float64(v.AsInt)
	}
}

func fillHistogramDataPoint(p *writer.MetricPoint, dp *metricsv1.HistogramDataPoint) {
	p.Timestamp = int64(dp.GetTimeUnixNano() / 1000)
	p.StartTimestamp = int64(dp.GetStartTimeUnixNano() / 1000)
	p.Attributes = attributesToJSON(dp.GetAttributes())
	p.Flags = dp.GetFlags()
	p.Count = dp.GetCount()
	if dp.Sum != nil {
		p.Sum = dp.GetSum()
	}
	if dp.Min != nil {
		p.Min = dp.GetMin()
	}
	if dp.Max != nil {
		p.Max = dp.GetMax()
	}
	p.Exemplars = exemplarsToJSON(dp.GetExemplars())
	b, _ := json.Marshal(dp.GetBucketCounts())
	p.BucketCounts = string(b)
	b, _ = json.Marshal(dp.GetExplicitBounds())
	p.ExplicitBounds = string(b)
}

func fillExpHistogramDataPoint(p *writer.MetricPoint, dp *metricsv1.ExponentialHistogramDataPoint) {
	p.Timestamp = int64(dp.GetTimeUnixNano() / 1000)
	p.StartTimestamp = int64(dp.GetStartTimeUnixNano() / 1000)
	p.Attributes = attributesToJSON(dp.GetAttributes())
	p.Flags = dp.GetFlags()
	p.Count = dp.GetCount()
	if dp.Sum != nil {
		p.Sum = dp.GetSum()
	}
	if dp.Min != nil {
		p.Min = dp.GetMin()
	}
	if dp.Max != nil {
		p.Max = dp.GetMax()
	}
	p.Exemplars = exemplarsToJSON(dp.GetExemplars())

	// Encode exponential histogram buckets as JSON
	buckets := map[string]interface{}{
		"scale":      dp.GetScale(),
		"zero_count": dp.GetZeroCount(),
	}
	if pos := dp.GetPositive(); pos != nil {
		buckets["positive"] = map[string]interface{}{"offset": pos.GetOffset(), "bucket_counts": pos.GetBucketCounts()}
	}
	if neg := dp.GetNegative(); neg != nil {
		buckets["negative"] = map[string]interface{}{"offset": neg.GetOffset(), "bucket_counts": neg.GetBucketCounts()}
	}
	b, _ := json.Marshal(buckets)
	p.BucketCounts = string(b)
}

func fillSummaryDataPoint(p *writer.MetricPoint, dp *metricsv1.SummaryDataPoint) {
	p.Timestamp = int64(dp.GetTimeUnixNano() / 1000)
	p.StartTimestamp = int64(dp.GetStartTimeUnixNano() / 1000)
	p.Attributes = attributesToJSON(dp.GetAttributes())
	p.Flags = dp.GetFlags()
	p.Count = dp.GetCount()
	p.Sum = dp.GetSum()

	if qv := dp.GetQuantileValues(); len(qv) > 0 {
		var out []map[string]float64
		for _, q := range qv {
			out = append(out, map[string]float64{
				"quantile": q.GetQuantile(),
				"value":    q.GetValue(),
			})
		}
		b, _ := json.Marshal(out)
		p.QuantileValues = string(b)
	}
}

func exemplarsToJSON(exemplars []*metricsv1.Exemplar) string {
	if len(exemplars) == 0 {
		return "[]"
	}
	var out []map[string]interface{}
	for _, e := range exemplars {
		m := map[string]interface{}{
			"time_unix_nano": e.GetTimeUnixNano(),
			"trace_id":       hex.EncodeToString(e.GetTraceId()),
			"span_id":        hex.EncodeToString(e.GetSpanId()),
		}
		switch v := e.GetValue().(type) {
		case *metricsv1.Exemplar_AsDouble:
			m["value"] = v.AsDouble
		case *metricsv1.Exemplar_AsInt:
			m["value"] = v.AsInt
		}
		if len(e.GetFilteredAttributes()) > 0 {
			attrs := make(map[string]interface{})
			for _, kv := range e.GetFilteredAttributes() {
				attrs[kv.GetKey()] = anyValueToInterface(kv.GetValue())
			}
			m["filtered_attributes"] = attrs
		}
		out = append(out, m)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// --- Shared helpers ---

func unmarshalOTLP(req *http.Request, msg proto.Message) error {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return fmt.Errorf("failed to read body")
	}
	defer req.Body.Close()

	ct := req.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "application/x-protobuf"), strings.Contains(ct, "application/protobuf"):
		return proto.Unmarshal(body, msg)
	case strings.Contains(ct, "application/json"):
		return protojson.Unmarshal(body, msg)
	default:
		if err := proto.Unmarshal(body, msg); err != nil {
			if err2 := protojson.Unmarshal(body, msg); err2 != nil {
				return fmt.Errorf("unsupported content type")
			}
		}
		return nil
	}
}

func extractServiceName(attrs []*commonv1.KeyValue) string {
	for _, kv := range attrs {
		if kv.GetKey() == "service.name" {
			return kv.GetValue().GetStringValue()
		}
	}
	return "unknown"
}

type scopeHolder interface {
	GetName() string
	GetVersion() string
}

func extractScope(scope scopeHolder) (string, string) {
	if scope == nil {
		return "", ""
	}
	return scope.GetName(), scope.GetVersion()
}

func attributesToJSON(attrs []*commonv1.KeyValue) string {
	if len(attrs) == 0 {
		return "{}"
	}
	m := make(map[string]interface{}, len(attrs))
	for _, kv := range attrs {
		m[kv.GetKey()] = anyValueToInterface(kv.GetValue())
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func anyValueToInterface(v *commonv1.AnyValue) interface{} {
	if v == nil {
		return nil
	}
	switch v.Value.(type) {
	case *commonv1.AnyValue_StringValue:
		return v.GetStringValue()
	case *commonv1.AnyValue_IntValue:
		return v.GetIntValue()
	case *commonv1.AnyValue_DoubleValue:
		return v.GetDoubleValue()
	case *commonv1.AnyValue_BoolValue:
		return v.GetBoolValue()
	case *commonv1.AnyValue_ArrayValue:
		arr := v.GetArrayValue()
		result := make([]interface{}, len(arr.GetValues()))
		for i, val := range arr.GetValues() {
			result[i] = anyValueToInterface(val)
		}
		return result
	case *commonv1.AnyValue_KvlistValue:
		kvl := v.GetKvlistValue()
		m := make(map[string]interface{})
		for _, kv := range kvl.GetValues() {
			m[kv.GetKey()] = anyValueToInterface(kv.GetValue())
		}
		return m
	case *commonv1.AnyValue_BytesValue:
		return hex.EncodeToString(v.GetBytesValue())
	default:
		return fmt.Sprintf("%v", v)
	}
}

func anyValueToString(v *commonv1.AnyValue) string {
	if v == nil {
		return ""
	}
	switch v.Value.(type) {
	case *commonv1.AnyValue_StringValue:
		return v.GetStringValue()
	default:
		iface := anyValueToInterface(v)
		if s, ok := iface.(string); ok {
			return s
		}
		b, _ := json.Marshal(iface)
		return string(b)
	}
}

func eventsToJSON(events []*tracev1.Span_Event) string {
	if len(events) == 0 {
		return "[]"
	}
	var out []map[string]interface{}
	for _, e := range events {
		m := map[string]interface{}{
			"name":                     e.GetName(),
			"time_unix_nano":           e.GetTimeUnixNano(),
			"dropped_attributes_count": e.GetDroppedAttributesCount(),
		}
		if len(e.GetAttributes()) > 0 {
			attrs := make(map[string]interface{}, len(e.GetAttributes()))
			for _, kv := range e.GetAttributes() {
				attrs[kv.GetKey()] = anyValueToInterface(kv.GetValue())
			}
			m["attributes"] = attrs
		}
		out = append(out, m)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func linksToJSON(links []*tracev1.Span_Link) string {
	if len(links) == 0 {
		return "[]"
	}
	var out []map[string]interface{}
	for _, l := range links {
		m := map[string]interface{}{
			"trace_id":                 hex.EncodeToString(l.GetTraceId()),
			"span_id":                  hex.EncodeToString(l.GetSpanId()),
			"trace_state":              l.GetTraceState(),
			"dropped_attributes_count": l.GetDroppedAttributesCount(),
		}
		if len(l.GetAttributes()) > 0 {
			attrs := make(map[string]interface{}, len(l.GetAttributes()))
			for _, kv := range l.GetAttributes() {
				attrs[kv.GetKey()] = anyValueToInterface(kv.GetValue())
			}
			m["attributes"] = attrs
		}
		out = append(out, m)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// ensure NaN/Inf don't break JSON marshaling
func safeFloat(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}
