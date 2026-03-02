package writer

type TraceSpan struct {
	TraceID            string  `parquet:"trace_id"`
	SpanID             string  `parquet:"span_id"`
	ParentSpanID       string  `parquet:"parent_span_id"`
	TraceState         string  `parquet:"trace_state"`
	ServiceName        string  `parquet:"service_name"`
	SpanName           string  `parquet:"span_name"`
	SpanKind           string  `parquet:"span_kind"`
	StartTime          int64   `parquet:"start_time"`
	EndTime            int64   `parquet:"end_time"`
	DurationMs         float64 `parquet:"duration_ms"`
	StatusCode         string  `parquet:"status_code"`
	StatusMessage      string  `parquet:"status_message"`
	Attributes         string  `parquet:"attributes"`
	ResourceAttributes string  `parquet:"resource_attributes"`
	ScopeName          string  `parquet:"scope_name"`
	ScopeVersion       string  `parquet:"scope_version"`
	Events             string  `parquet:"events"`
	Links              string  `parquet:"links"`
}

type LogRecord struct {
	Timestamp          int64  `parquet:"timestamp"`
	ObservedTimestamp  int64  `parquet:"observed_timestamp"`
	TraceID            string `parquet:"trace_id"`
	SpanID             string `parquet:"span_id"`
	SeverityNumber     int32  `parquet:"severity_number"`
	SeverityText       string `parquet:"severity_text"`
	Body               string `parquet:"body"`
	Attributes         string `parquet:"attributes"`
	ResourceAttributes string `parquet:"resource_attributes"`
	ServiceName        string `parquet:"service_name"`
	ScopeName          string `parquet:"scope_name"`
	ScopeVersion       string `parquet:"scope_version"`
	Flags              uint32 `parquet:"flags"`
	EventName          string `parquet:"event_name"`
}

type MetricPoint struct {
	MetricName             string  `parquet:"metric_name"`
	MetricDescription      string  `parquet:"metric_description"`
	MetricUnit             string  `parquet:"metric_unit"`
	MetricType             string  `parquet:"metric_type"`
	Timestamp              int64   `parquet:"timestamp"`
	StartTimestamp         int64   `parquet:"start_timestamp"`
	ValueDouble            float64 `parquet:"value_double"`
	ValueInt               int64   `parquet:"value_int"`
	Count                  uint64  `parquet:"count"`
	Sum                    float64 `parquet:"sum"`
	Min                    float64 `parquet:"min"`
	Max                    float64 `parquet:"max"`
	BucketCounts           string  `parquet:"bucket_counts"`
	ExplicitBounds         string  `parquet:"explicit_bounds"`
	QuantileValues         string  `parquet:"quantile_values"`
	Attributes             string  `parquet:"attributes"`
	ResourceAttributes     string  `parquet:"resource_attributes"`
	ServiceName            string  `parquet:"service_name"`
	ScopeName              string  `parquet:"scope_name"`
	ScopeVersion           string  `parquet:"scope_version"`
	Exemplars              string  `parquet:"exemplars"`
	Flags                  uint32  `parquet:"flags"`
	IsMonotonic            bool    `parquet:"is_monotonic"`
	AggregationTemporality string  `parquet:"aggregation_temporality"`
}
