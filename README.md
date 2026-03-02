# otelite

A lightweight local OpenTelemetry backend. Receives OTLP traces, logs, and metrics over HTTP, stores them as partitioned Parquet files, and makes them queryable via embedded DuckDB through a CLI.

Single binary. No external dependencies. Designed for LLM agents to shell out to for telemetry diagnostics.

## Install

```bash
go install github.com/davidhope/otelite/cmd/otelite@latest
```

Or build from source:

```bash
go build -o otelite ./cmd/otelite/
```

## Usage

### Start the collector

```bash
otelite serve
```

Listens on `:4318` for OTLP/HTTP (protobuf and JSON). Accepts all three signal types:

- `POST /v1/traces`
- `POST /v1/logs`
- `POST /v1/metrics`

Data is buffered in memory and flushed to Parquet files under `./data/{traces,logs,metrics}/YYYY-MM-DD/`.

Options:

```
--port            Port to listen on (default 4318)
--flush-interval  How often to flush to disk (default 30s)
--data-dir        Storage directory (default ./data)
```

### Query with SQL

Run arbitrary SQL against any signal type:

```bash
otelite query "SELECT service_name, span_name, duration_ms FROM traces ORDER BY duration_ms DESC LIMIT 10"
otelite query "SELECT severity_text, body FROM logs WHERE severity_text = 'ERROR'"
otelite query "SELECT metric_name, value_double FROM metrics WHERE metric_type = 'gauge'"
```

### List services

```bash
otelite services
```

### Browse traces

```bash
otelite traces --service my-api --since 1h --status error --limit 50
```

### Browse logs

```bash
otelite logs --service my-api --since 1h --severity error
otelite logs --search "timeout" --limit 100
```

### Browse metrics

```bash
otelite metrics --service my-api --name http.request.duration --type histogram
otelite metrics --since 30m
```

### Show schema

```bash
otelite schema          # traces (default)
otelite schema logs
otelite schema metrics
```

### Output formats

All query commands support `--format json` (default), `--format table`, or `--format csv`:

```bash
otelite traces --since 30m --format table
otelite logs --severity error --format csv
```

## Schemas

### Traces

| Column | Type | Description |
|--------|------|-------------|
| trace_id | string | 32-hex-char trace identifier |
| span_id | string | 16-hex-char span identifier |
| parent_span_id | string | Parent span identifier |
| trace_state | string | W3C tracestate header |
| service_name | string | From resource `service.name` attribute |
| span_name | string | Operation name |
| span_kind | string | SPAN_KIND_SERVER, CLIENT, etc. |
| start_time | int64 | Start time in Unix microseconds |
| end_time | int64 | End time in Unix microseconds |
| duration_ms | float64 | Span duration in milliseconds |
| status_code | string | STATUS_CODE_OK, ERROR, or UNSET |
| status_message | string | Status description |
| attributes | string | Span attributes as JSON |
| resource_attributes | string | Resource attributes as JSON |
| scope_name | string | Instrumentation scope name |
| scope_version | string | Instrumentation scope version |
| events | string | Span events as JSON array |
| links | string | Span links as JSON array |

### Logs

| Column | Type | Description |
|--------|------|-------------|
| timestamp | int64 | Log time in Unix microseconds |
| observed_timestamp | int64 | When the log was observed |
| trace_id | string | Correlated trace ID (if any) |
| span_id | string | Correlated span ID (if any) |
| severity_number | int32 | Numeric severity (1-24) |
| severity_text | string | DEBUG, INFO, WARN, ERROR, FATAL |
| body | string | Log message body |
| attributes | string | Log attributes as JSON |
| resource_attributes | string | Resource attributes as JSON |
| service_name | string | From resource `service.name` attribute |
| scope_name | string | Instrumentation scope name |
| scope_version | string | Instrumentation scope version |
| flags | uint32 | Log record flags |
| event_name | string | Event category name |

### Metrics

| Column | Type | Description |
|--------|------|-------------|
| metric_name | string | Metric name |
| metric_description | string | Metric description |
| metric_unit | string | Unit of measurement |
| metric_type | string | gauge, sum, histogram, exponential_histogram, summary |
| timestamp | int64 | Data point time in Unix microseconds |
| start_timestamp | int64 | Collection start time |
| value_double | float64 | Value for gauge/sum (double) |
| value_int | int64 | Value for gauge/sum (int) |
| count | uint64 | Count for histogram/summary |
| sum | float64 | Sum for histogram/summary |
| min | float64 | Min for histogram |
| max | float64 | Max for histogram |
| bucket_counts | string | Histogram bucket counts as JSON |
| explicit_bounds | string | Histogram bucket bounds as JSON |
| quantile_values | string | Summary quantiles as JSON |
| attributes | string | Data point attributes as JSON |
| resource_attributes | string | Resource attributes as JSON |
| service_name | string | From resource `service.name` attribute |
| scope_name | string | Instrumentation scope name |
| scope_version | string | Instrumentation scope version |
| exemplars | string | Exemplars as JSON |
| flags | uint32 | Data point flags |
| is_monotonic | bool | Whether a sum is monotonic |
| aggregation_temporality | string | DELTA or CUMULATIVE |

## Sending telemetry

Point any OpenTelemetry SDK or collector at `http://localhost:4318` using the OTLP/HTTP exporter. All three signal types are supported.

## Storage layout

```
data/
  traces/
    2026-03-02/
      14-30.parquet
  logs/
    2026-03-02/
      14-30.parquet
  metrics/
    2026-03-02/
      14-30.parquet
```

Files are date-partitioned and named by minute. DuckDB auto-discovers all Parquet files via glob at query time. Nothing is dropped — all OTLP fields are preserved.

## Example queries

```sql
-- Slowest spans in the last hour
SELECT service_name, span_name, duration_ms
FROM traces
WHERE start_time >= epoch_us(now() - INTERVAL '1 hour')
ORDER BY duration_ms DESC
LIMIT 10;

-- Error rate by service
SELECT service_name,
       count(*) as total,
       count(*) FILTER (WHERE status_code = 'STATUS_CODE_ERROR') as errors
FROM traces
GROUP BY service_name;

-- Trace waterfall
SELECT span_name, parent_span_id, duration_ms,
       start_time - min(start_time) OVER (PARTITION BY trace_id) as offset_us
FROM traces
WHERE trace_id = '01020304050607080910111213141516'
ORDER BY start_time;

-- Recent error logs
SELECT timestamp, service_name, body
FROM logs
WHERE severity_text = 'ERROR'
ORDER BY timestamp DESC
LIMIT 20;

-- Logs correlated with a trace
SELECT severity_text, body
FROM logs
WHERE trace_id = '01020304050607080910111213141516'
ORDER BY timestamp;

-- P99 request duration from histograms
SELECT metric_name, service_name,
       sum / count as avg_ms,
       max as max_ms
FROM metrics
WHERE metric_name = 'http.request.duration'
ORDER BY timestamp DESC
LIMIT 10;

-- Cross-signal: find error spans and their logs
SELECT t.span_name, t.duration_ms, l.body
FROM traces t
JOIN logs l ON t.trace_id = l.trace_id AND t.span_id = l.span_id
WHERE t.status_code = 'STATUS_CODE_ERROR'
ORDER BY t.start_time DESC;
```
