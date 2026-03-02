# otelite SQL Query Patterns

All queries run via `otelite query "SQL"` or the embedded DuckDB engine. Three views: `traces`, `logs`, `metrics`.

## Trace Queries

```sql
-- Slowest spans
SELECT service_name, span_name, duration_ms, trace_id
FROM traces ORDER BY duration_ms DESC LIMIT 10

-- Error rate by service
SELECT service_name,
       count(*) AS total,
       count(*) FILTER (WHERE status_code = 'STATUS_CODE_ERROR') AS errors,
       round(100.0 * count(*) FILTER (WHERE status_code = 'STATUS_CODE_ERROR') / count(*), 2) AS error_pct
FROM traces GROUP BY service_name ORDER BY error_pct DESC

-- Root spans only (no parent)
SELECT * FROM traces WHERE parent_span_id = '' ORDER BY start_time DESC LIMIT 20

-- Trace waterfall (all spans for one trace, ordered)
SELECT span_name, parent_span_id, duration_ms,
       start_time - MIN(start_time) OVER () AS offset_us
FROM traces WHERE trace_id = '<TRACE_ID>' ORDER BY start_time

-- Spans by kind
SELECT span_kind, count(*) FROM traces GROUP BY span_kind

-- P99 latency by operation
SELECT span_name,
       percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms) AS p99_ms,
       count(*) AS total
FROM traces GROUP BY span_name HAVING count(*) > 10 ORDER BY p99_ms DESC

-- Attribute filtering (e.g. HTTP method)
SELECT span_name, duration_ms
FROM traces
WHERE json_extract_string(attributes, '$.http.method') = 'POST'
ORDER BY duration_ms DESC LIMIT 10

-- Spans with errors and their messages
SELECT service_name, span_name, status_message, duration_ms
FROM traces WHERE status_code = 'STATUS_CODE_ERROR'
ORDER BY start_time DESC LIMIT 20
```

## Log Queries

```sql
-- Recent errors
SELECT service_name, severity_text, body, trace_id
FROM logs WHERE severity_text = 'ERROR'
ORDER BY timestamp DESC LIMIT 20

-- Log volume by severity
SELECT severity_text, count(*) AS total
FROM logs GROUP BY severity_text ORDER BY total DESC

-- Search log body
SELECT timestamp, service_name, body
FROM logs WHERE body ILIKE '%timeout%'
ORDER BY timestamp DESC LIMIT 20

-- Logs correlated with a trace
SELECT timestamp, severity_text, body, span_id
FROM logs WHERE trace_id = '<TRACE_ID>'
ORDER BY timestamp ASC

-- Error logs with stack traces (body contains "panic" or "exception")
SELECT service_name, body
FROM logs WHERE severity_text = 'ERROR'
  AND (body ILIKE '%panic%' OR body ILIKE '%exception%')
ORDER BY timestamp DESC LIMIT 10
```

## Metric Queries

```sql
-- Latest gauge values
SELECT service_name, metric_name, value_double, timestamp
FROM metrics WHERE metric_type = 'gauge'
ORDER BY timestamp DESC LIMIT 20

-- Histogram stats
SELECT metric_name, count, sum, min, max,
       CASE WHEN count > 0 THEN sum / count ELSE 0 END AS avg
FROM metrics WHERE metric_type = 'histogram'
ORDER BY timestamp DESC LIMIT 20

-- All metrics for a service
SELECT metric_name, metric_type, count(*) AS data_points
FROM metrics WHERE service_name = 'my-service'
GROUP BY metric_name, metric_type ORDER BY data_points DESC

-- Monotonic counters (rates)
SELECT metric_name, value_double, value_int
FROM metrics WHERE is_monotonic = true
ORDER BY timestamp DESC LIMIT 20
```

## Cross-Signal Correlation

```sql
-- Find traces with errors, then get their logs
SELECT l.timestamp, l.severity_text, l.body, t.span_name, t.duration_ms
FROM traces t
JOIN logs l ON t.trace_id = l.trace_id AND t.span_id = l.span_id
WHERE t.status_code = 'STATUS_CODE_ERROR'
ORDER BY l.timestamp DESC LIMIT 20

-- Services with errors in both traces and logs
SELECT t.service_name,
       count(DISTINCT t.trace_id) AS error_traces,
       count(DISTINCT l.trace_id) AS error_logs
FROM traces t
LEFT JOIN logs l ON t.trace_id = l.trace_id AND l.severity_text = 'ERROR'
WHERE t.status_code = 'STATUS_CODE_ERROR'
GROUP BY t.service_name
```

## Time-Based Queries

Timestamps are Unix microseconds. Use DuckDB functions:

```sql
-- Last hour
WHERE start_time >= epoch_us(now() - INTERVAL '1 hour')

-- Specific date range
WHERE start_time >= epoch_us(TIMESTAMP '2026-01-15 00:00:00')
  AND start_time < epoch_us(TIMESTAMP '2026-01-16 00:00:00')

-- Convert timestamp to readable format
SELECT make_timestamp(start_time) AS ts, span_name FROM traces LIMIT 5

-- Throughput over time (spans per minute)
SELECT date_trunc('minute', make_timestamp(start_time)) AS minute,
       count(*) AS span_count
FROM traces GROUP BY 1 ORDER BY 1 DESC LIMIT 60
```

## Agent Diagnostic Workflow

Systematic investigation pattern for LLM agents:

```bash
# Step 1: What services exist?
otelite services --format json

# Step 2: Where are the errors?
otelite query "SELECT service_name, count(*) FILTER (WHERE status_code = 'STATUS_CODE_ERROR') AS errors, count(*) AS total FROM traces GROUP BY 1 ORDER BY errors DESC" --format json

# Step 3: Drill into error spans for top service
otelite traces --service <NAME> --status error --limit 10

# Step 4: Get a trace waterfall
otelite query "SELECT span_name, duration_ms, parent_span_id FROM traces WHERE trace_id = '<ID>' ORDER BY start_time"

# Step 5: Correlate with logs
otelite query "SELECT severity_text, body FROM logs WHERE trace_id = '<ID>' ORDER BY timestamp"

# Step 6: Check metrics for the service
otelite metrics --service <NAME> --since 1h
```
