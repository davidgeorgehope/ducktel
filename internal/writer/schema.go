package writer

// The `dict` tag option turns on dictionary encoding for a column, which is
// what makes these files small and fast to read: telemetry values repeat
// heavily (service names, metric names, attribute JSON). It is left off the
// near-unique columns (trace and span ids, exemplars), where a dictionary only
// adds overhead. Column names, types and order are unchanged, so files
// written before this change stay readable alongside new ones.

type TraceSpan struct {
	TraceID            string  `parquet:"trace_id"`
	SpanID             string  `parquet:"span_id"`
	ParentSpanID       string  `parquet:"parent_span_id"`
	TraceState         string  `parquet:"trace_state,dict"`
	ServiceName        string  `parquet:"service_name,dict"`
	SpanName           string  `parquet:"span_name,dict"`
	SpanKind           string  `parquet:"span_kind,dict"`
	StartTime          int64   `parquet:"start_time"`
	EndTime            int64   `parquet:"end_time"`
	DurationMs         float64 `parquet:"duration_ms"`
	StatusCode         string  `parquet:"status_code,dict"`
	StatusMessage      string  `parquet:"status_message,dict"`
	Attributes         string  `parquet:"attributes,dict"`
	ResourceAttributes string  `parquet:"resource_attributes,dict"`
	ScopeName          string  `parquet:"scope_name,dict"`
	ScopeVersion       string  `parquet:"scope_version,dict"`
	Events             string  `parquet:"events,dict"`
	Links              string  `parquet:"links,dict"`
}

type LogRecord struct {
	Timestamp          int64  `parquet:"timestamp"`
	ObservedTimestamp  int64  `parquet:"observed_timestamp"`
	TraceID            string `parquet:"trace_id"`
	SpanID             string `parquet:"span_id"`
	SeverityNumber     int32  `parquet:"severity_number"`
	SeverityText       string `parquet:"severity_text,dict"`
	Body               string `parquet:"body,dict"`
	Attributes         string `parquet:"attributes,dict"`
	ResourceAttributes string `parquet:"resource_attributes,dict"`
	ServiceName        string `parquet:"service_name,dict"`
	ScopeName          string `parquet:"scope_name,dict"`
	ScopeVersion       string `parquet:"scope_version,dict"`
	Flags              uint32 `parquet:"flags"`
	EventName          string `parquet:"event_name,dict"`
}

type MetricPoint struct {
	MetricName             string  `parquet:"metric_name,dict"`
	MetricDescription      string  `parquet:"metric_description,dict"`
	MetricUnit             string  `parquet:"metric_unit,dict"`
	MetricType             string  `parquet:"metric_type,dict"`
	Timestamp              int64   `parquet:"timestamp"`
	StartTimestamp         int64   `parquet:"start_timestamp"`
	ValueDouble            float64 `parquet:"value_double"`
	ValueInt               int64   `parquet:"value_int"`
	Count                  uint64  `parquet:"count"`
	Sum                    float64 `parquet:"sum"`
	Min                    float64 `parquet:"min"`
	Max                    float64 `parquet:"max"`
	BucketCounts           string  `parquet:"bucket_counts,dict"`
	ExplicitBounds         string  `parquet:"explicit_bounds,dict"`
	QuantileValues         string  `parquet:"quantile_values,dict"`
	Attributes             string  `parquet:"attributes,dict"`
	ResourceAttributes     string  `parquet:"resource_attributes,dict"`
	ServiceName            string  `parquet:"service_name,dict"`
	ScopeName              string  `parquet:"scope_name,dict"`
	ScopeVersion           string  `parquet:"scope_version,dict"`
	Exemplars              string  `parquet:"exemplars"`
	Flags                  uint32  `parquet:"flags"`
	IsMonotonic            bool    `parquet:"is_monotonic"`
	AggregationTemporality string  `parquet:"aggregation_temporality,dict"`
}
