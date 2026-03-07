# ducktel 🦆

**Observability for AI agents.** Single binary. OTLP in. SQL out.

Your coding agent can write code, run tests, and deploy services. But when something breaks in production, it's blind. It can't check dashboards. It can't click through Grafana. It can't read a flame graph.

ducktel gives agents eyes. It receives OpenTelemetry data, stores it as Parquet files, and exposes everything through SQL — the one query language every LLM already knows. An agent shells out to `ducktel query`, gets structured JSON back, and reasons about what's happening in your system.

No dashboards. No UI. No proprietary query language. Just telemetry and SQL.

```bash
# Start collecting telemetry
ducktel serve

# Agent investigates an incident
ducktel query "SELECT service_name, count(*) as errors FROM traces WHERE status_code = 'STATUS_CODE_ERROR' GROUP BY 1 ORDER BY 2 DESC" --format json

# Agent saves the diagnostic query for continuous monitoring
ducktel saved create "error-spike" "SELECT ..." --schedule "every 60s"

# Agent runs all saved queries on its heartbeat loop
ducktel saved run-all
```

## Why agents need their own observability tool

Observability tools were built for humans — dashboards to look at, alert rules to click through, visualizations to interpret. AI agents don't need any of that. They need:

1. **Telemetry in** — standard OTLP, no vendor lock-in
2. **SQL out** — structured results they can reason about
3. **A CLI** — something they can shell out to, not a browser they can't open

That's what ducktel is. Everything else — the dashboards, the query builders, the alert rule editors — was scaffolding for human cognition. Necessary when humans interpreted telemetry. Optional when an LLM does it.

## How it works

```
Your App (OTel SDK)  →  ducktel serve  →  Parquet files on disk
                                                    ↓
                         LLM Agent     ←  ducktel query (SQL → JSON)
```

1. **Receive** — OTLP/HTTP receiver accepts traces, logs, and metrics on port 4318
2. **Store** — data is buffered and flushed to date-partitioned Parquet files
3. **Query** — embedded DuckDB reads Parquet in-place, returns JSON to the agent

No database to manage. No cluster to configure. Just a binary and a directory of files.

## Install

```bash
go install github.com/davidgeorgehope/ducktel/cmd/ducktel@latest
```

Or build from source:

```bash
git clone https://github.com/davidgeorgehope/ducktel.git
cd ducktel
go build -o ducktel ./cmd/ducktel/
```

## Quick start

```bash
# Terminal 1: start the collector
ducktel serve

# Terminal 2: generate test data (built-in e-commerce topology)
ducktel testdata --duration 30s

# Terminal 3: query it
ducktel services
ducktel query "SELECT service_name, status_code, count(*) FROM traces GROUP BY 1,2 ORDER BY 3 DESC" --format table
```

## What an agent can do with ducktel

### Investigate an incident

```bash
# What services are reporting?
ducktel services --format json

# Where are the errors?
ducktel query "
  SELECT service_name, count(*) as error_count
  FROM traces
  WHERE status_code = 'STATUS_CODE_ERROR'
  GROUP BY service_name
  ORDER BY error_count DESC
" --format json

# What's failing?
ducktel query "
  SELECT span_name, count(*) as failures, avg(duration_ms) as avg_duration
  FROM traces
  WHERE service_name = 'payment-service'
    AND status_code = 'STATUS_CODE_ERROR'
  GROUP BY span_name
  ORDER BY failures DESC
" --format json

# Trace the root cause
ducktel query "
  SELECT span_name, parent_span_id, duration_ms, status_code
  FROM traces
  WHERE trace_id = '...'
  ORDER BY start_time
" --format json

# Correlate with logs
ducktel query "
  SELECT severity_text, body
  FROM logs
  WHERE trace_id = '...'
  ORDER BY timestamp
" --format json

# Cross-signal join: error spans with their log messages
ducktel query "
  SELECT t.service_name, t.span_name, t.duration_ms, l.body
  FROM traces t
  JOIN logs l ON t.trace_id = l.trace_id AND t.span_id = l.span_id
  WHERE t.status_code = 'STATUS_CODE_ERROR'
  ORDER BY t.start_time DESC
" --format json
```

Every step returns structured JSON. The agent reasons about each result and decides what to query next. No dashboards, no context-switching, no clicking — just systematic investigation.

### Monitor continuously with saved queries

The query that diagnosed a problem becomes the query that catches it next time. No translation layer, no alert rule syntax — just SQL.

```bash
# Agent found a payment issue. Save the diagnostic query:
ducktel saved create "payment-errors" \
  "SELECT service_name, count(*) as errors FROM traces WHERE service_name = 'payment-service' AND status_code = 'STATUS_CODE_ERROR' AND start_time >= epoch_us(now() - INTERVAL '5 minutes') GROUP BY 1" \
  --description "Payment service errors in last 5 minutes" \
  --schedule "every 60s" \
  --tags payment,errors

# Save a latency check too:
ducktel saved create "p99-latency" \
  "SELECT service_name, span_name, quantile_cont(duration_ms, 0.99) as p99_ms FROM traces GROUP BY 1,2 ORDER BY p99_ms DESC LIMIT 10" \
  --description "Top 10 slowest endpoints by P99" \
  --schedule "every 5m" \
  --tags latency

# On the agent's heartbeat loop — one command, all diagnostics:
ducktel saved run-all

# Manage saved queries
ducktel saved list
ducktel saved show "payment-errors"
ducktel saved run "payment-errors"
ducktel saved delete "payment-errors"
```

`run-all` is the key primitive. The agent runs it on a timer, gets a JSON array of all results, and decides what needs attention. ducktel never runs queries automatically — there's no scheduler, no webhooks, no notification system. The agent is the intelligence layer.

## CLI reference

### `ducktel serve`

Start the OTLP receiver.

```bash
ducktel serve [--port 4318] [--flush-interval 30s] [--data-dir ./data]
```

Accepts `POST /v1/traces`, `POST /v1/logs`, `POST /v1/metrics` — protobuf and JSON.

### `ducktel query`

Run arbitrary SQL against traces, logs, or metrics.

```bash
ducktel query "SELECT ..." [--format json|table|csv] [--data-dir ./data]
```

### `ducktel services`

List all services that have reported telemetry.

### `ducktel traces`

Browse traces with filters.

```bash
ducktel traces [--service name] [--since 1h] [--status error] [--limit 50]
```

### `ducktel logs`

Browse logs with filters.

```bash
ducktel logs [--service name] [--since 1h] [--severity error] [--search "timeout"]
```

### `ducktel metrics`

Browse metrics with filters.

```bash
ducktel metrics [--service name] [--name http.request.duration] [--since 30m]
```

### `ducktel schema`

Show the schema for any signal type.

```bash
ducktel schema traces|logs|metrics
```

### `ducktel saved`

Manage saved queries (also available as `ducktel sq`).

```bash
ducktel saved create <name> <sql> [--description "..."] [--schedule "every 60s"] [--tags a,b]
ducktel saved list [--format json|table]
ducktel saved show <name>
ducktel saved run <name>
ducktel saved run-all
ducktel saved delete <name>
```

### `ducktel testdata`

Generate synthetic OTLP telemetry for testing.

```bash
ducktel testdata [--duration 30s] [--trace-rate 2] [--error-rate 0.05] [--endpoint http://localhost:4318]

# Inject failures
ducktel testdata --scenario "payment-service:error_rate:0.8"
ducktel testdata --scenario "product-service:latency_spike:5.0"

# Custom topology via JSON config
ducktel testdata --config topology.json
```

Default topology: api-gateway → product-service → orders → payments, with Postgres and Redis dependencies.

### Output formats

All commands support `--format json` (default for agents), `--format table` (for humans), or `--format csv`.

## Sending telemetry

Point any OpenTelemetry SDK or collector at `http://localhost:4318`:

```yaml
# OTel Collector config
exporters:
  otlphttp:
    endpoint: http://localhost:4318
    tls:
      insecure: true

service:
  pipelines:
    traces:
      exporters: [otlphttp]
    logs:
      exporters: [otlphttp]
    metrics:
      exporters: [otlphttp]
```

## Storage

```
data/
  traces/
    2026-03-07/
      08-30.parquet
  logs/
    2026-03-07/
      08-30.parquet
  metrics/
    2026-03-07/
      08-30.parquet
```

Date-partitioned Parquet files. DuckDB reads them via glob at query time. Nothing is dropped — all OTLP fields are preserved. Your data is never trapped in a proprietary format.

## Schemas

<details>
<summary>Traces</summary>

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

</details>

<details>
<summary>Logs</summary>

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

</details>

<details>
<summary>Metrics</summary>

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

</details>

## The bigger picture

AI agents are becoming the primary operators of software systems. They deploy code, respond to incidents, scale infrastructure, and manage releases. But the tools they rely on for understanding system health — dashboards, alert UIs, visualization platforms — were designed for humans.

This creates a gap. The agent can `kubectl apply` but can't interpret a Grafana panel. It can read logs line by line but can't correlate a trace across services through a web UI. It can write perfect PromQL but has no way to execute it without a human-oriented platform in the middle.

Observability platforms monetized three layers: **ingestion**, **storage**, and **intelligence**. OpenTelemetry commoditized ingestion. Parquet and DuckDB commoditized storage. LLMs are commoditizing intelligence. What's left is the wiring — and ducktel is that wiring.

The emerging stack is simple:

```
OpenTelemetry SDKs → Commodity columnar storage → AI agent
```

ducktel makes this stack real in a single binary. No platform. No vendor. No lock-in. Just telemetry, SQL, and an agent that knows what to do with the results.

## Status

Early stage. The core ingest → store → query → saved queries loop works. Built-in test harness for generating synthetic data. Contributions welcome.

## License

MIT
