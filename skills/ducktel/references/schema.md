# ducktel Schema Reference

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
| metric_type | VARCHAR | gauge, sum, histogram, exponential_histogram, summary. Only gauge and sum populate `value_double`. See the note below. |
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

### Which metric types `metric_query` aggregates

`value_double` is populated for `gauge` and `sum` only, including integer sum points. `metric_query` reads `value_double` for those two types.

`histogram` and `exponential_histogram` leave `value_double` empty. `metric_query` still aggregates them. It reads `count`, `sum`, `min`, `max`, and, for an explicit-bucket histogram, `bucket_counts`. The most recent point's `metric_type` selects this path. A `count` of 0 means the window had no increment. `avg` on an empty window is null. Neither result means the histogram was ignored.

| metric_type | columns `metric_query` reads | aggregations |
|---|---|---|
| `gauge` | `value_double` | `avg`, `sum`, `min`, `max`, `count`, `p50`, `p95`, `p99` |
| `sum` | `value_double` | `avg`, `sum`, `min`, `max`, `count`, `p50`, `p95`, `p99` |
| `histogram` | `count`, `sum`, `min`, `max`, `bucket_counts` | `count`, `sum`, `avg` (sum/count), `min`, `max`, `p50`, `p95`, `p99`, `buckets` |
| `exponential_histogram` | `count`, `sum`, `min`, `max` | `count`, `sum`, `avg`, `min`, `max`. `p50`, `p95`, `p99`, and `buckets` return an error, because those need explicit bucket boundaries. |
| `summary` | `value_double`, which stays empty | `count` counts exported points. `avg`, `sum`, `min`, `max`, and percentiles do not see `count`, `sum`, or `quantile_values`. Read those columns with `ducktel query`. |

On a gauge or a sum, `count` counts exported points, not events. A gauge written on every interval is counted once per export.

`buckets` returns one row per bucket, with `le` (inclusive upper bound, null past the last bound) and `value` (observations). It requires an explicit-bucket histogram. `p50`, `p95`, and `p99` on a histogram are interpolated from the merged buckets. Cumulative series are turned into increments inside the window before those aggregations run. The histogram path lives in `internal/telemetry/histogram.go`.

`exemplars` returns the exemplars attached to the points (`timestamp`, `value`, `trace_id`, `span_id`), for any metric type that has them. It does not use the table above.

## Storage Layout

```
data/
  traces/YYYY-MM-DD/HH-MM.parquet
  logs/YYYY-MM-DD/HH-MM.parquet
  metrics/YYYY-MM-DD/HH-MM.parquet
```

A flush writes `HH-MM.parquet`. A second flush in the same minute adds a counter suffix. The clock in the name is the flush time, not the time on the rows.

Compaction rewrites closed files. For today and yesterday (UTC), an hour that started at least 2 hours ago becomes `HH.parquet`. A day strictly before yesterday becomes one `day.parquet`. The query engine builds each view from those files with `read_parquet` over an explicit list (`internal/query/engine.go`). A time-bounded query leaves out files that cannot hold rows in the window. All three names are eligible.

## JSON Field Access (DuckDB)

Attributes and other JSON fields can be queried using DuckDB's JSON functions:

```sql
-- Extract a string value
json_extract_string(attributes, '$."http.method"')

-- Extract a numeric value
CAST(json_extract(attributes, '$."http.status_code"') AS INTEGER)

-- Check if a key exists
json_extract(attributes, '$."error.type"') IS NOT NULL

-- Extract from resource attributes
json_extract_string(resource_attributes, '$."deployment.environment"')
```
