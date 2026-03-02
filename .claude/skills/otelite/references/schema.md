# otelite Schema Reference

## traces

| Column | Type | Description |
|--------|------|-------------|
| trace_id | VARCHAR | 32-hex-char trace identifier |
| span_id | VARCHAR | 16-hex-char span identifier |
| parent_span_id | VARCHAR | Parent span ID (empty for root spans) |
| trace_state | VARCHAR | W3C tracestate header value |
| service_name | VARCHAR | From `service.name` resource attribute |
| span_name | VARCHAR | Operation name |
| span_kind | VARCHAR | SPAN_KIND_SERVER, CLIENT, PRODUCER, CONSUMER, INTERNAL |
| start_time | BIGINT | Start timestamp (Unix microseconds) |
| end_time | BIGINT | End timestamp (Unix microseconds) |
| duration_ms | DOUBLE | Precomputed duration in milliseconds |
| status_code | VARCHAR | STATUS_CODE_OK, STATUS_CODE_ERROR, STATUS_CODE_UNSET |
| status_message | VARCHAR | Optional status description |
| attributes | VARCHAR | Span attributes as JSON string |
| resource_attributes | VARCHAR | Resource attributes as JSON string |
| scope_name | VARCHAR | Instrumentation scope name |
| scope_version | VARCHAR | Instrumentation scope version |
| events | VARCHAR | Span events as JSON array string |
| links | VARCHAR | Span links as JSON array string |

## logs

| Column | Type | Description |
|--------|------|-------------|
| timestamp | BIGINT | Log timestamp (Unix microseconds) |
| observed_timestamp | BIGINT | When the log was observed (Unix microseconds) |
| trace_id | VARCHAR | Trace correlation ID |
| span_id | VARCHAR | Span correlation ID |
| severity_number | INTEGER | Numeric severity (1-24) |
| severity_text | VARCHAR | DEBUG, INFO, WARN, ERROR, FATAL |
| body | VARCHAR | Log message body |
| attributes | VARCHAR | Log attributes as JSON string |
| resource_attributes | VARCHAR | Resource attributes as JSON string |
| service_name | VARCHAR | From `service.name` resource attribute |
| scope_name | VARCHAR | Instrumentation scope name |
| scope_version | VARCHAR | Instrumentation scope version |
| flags | UINTEGER | Log record flags |
| event_name | VARCHAR | Event name (if log is an event) |

## metrics

| Column | Type | Description |
|--------|------|-------------|
| metric_name | VARCHAR | Metric instrument name (e.g. `http.request.duration`) |
| metric_description | VARCHAR | Human-readable description |
| metric_unit | VARCHAR | Unit (e.g. `ms`, `By`, `1`) |
| metric_type | VARCHAR | gauge, sum, histogram, summary |
| timestamp | BIGINT | Data point timestamp (Unix microseconds) |
| start_timestamp | BIGINT | Cumulative start time (Unix microseconds) |
| value_double | DOUBLE | Gauge/sum value (float) |
| value_int | BIGINT | Gauge/sum value (integer) |
| count | UBIGINT | Histogram/summary sample count |
| sum | DOUBLE | Histogram/summary sum of values |
| min | DOUBLE | Histogram min (if recorded) |
| max | DOUBLE | Histogram max (if recorded) |
| bucket_counts | VARCHAR | Histogram bucket counts as JSON array |
| explicit_bounds | VARCHAR | Histogram bucket boundaries as JSON array |
| quantile_values | VARCHAR | Summary quantiles as JSON array |
| attributes | VARCHAR | Data point attributes as JSON string |
| resource_attributes | VARCHAR | Resource attributes as JSON string |
| service_name | VARCHAR | From `service.name` resource attribute |
| scope_name | VARCHAR | Instrumentation scope name |
| scope_version | VARCHAR | Instrumentation scope version |
| exemplars | VARCHAR | Exemplars as JSON array string |
| flags | UINTEGER | Data point flags |
| is_monotonic | BOOLEAN | Whether sum is monotonically increasing |
| aggregation_temporality | VARCHAR | AGGREGATION_TEMPORALITY_DELTA or CUMULATIVE |

## Storage Layout

```
data/
  traces/YYYY-MM-DD/HH-MM.parquet
  logs/YYYY-MM-DD/HH-MM.parquet
  metrics/YYYY-MM-DD/HH-MM.parquet
```

DuckDB views use glob patterns (`data/traces/**/*.parquet`) to auto-discover all files.

## JSON Field Access (DuckDB)

Attributes and other JSON fields can be queried using DuckDB's JSON functions:

```sql
-- Extract a string value
json_extract_string(attributes, '$.http.method')

-- Extract a numeric value
CAST(json_extract(attributes, '$.http.status_code') AS INTEGER)

-- Check if a key exists
json_extract(attributes, '$.error.type') IS NOT NULL

-- Extract from resource attributes
json_extract_string(resource_attributes, '$.deployment.environment')
```
