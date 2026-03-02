# otelite

A lightweight local OpenTelemetry backend. Receives OTLP traces, logs, and metrics over HTTP, stores them as partitioned Parquet files, and makes them queryable via embedded DuckDB through a CLI.

Single binary. No external dependencies. Designed for LLM agents to shell out to for telemetry diagnostics.

## Why This Exists

The observability industry is undergoing a huge disruption. A fundamental compression of the value chain that will reshape how software is monitored and debugged.

For the past decade, observability platforms have monetized three layers: **ingestion** (getting telemetry data in), **storage** (keeping it queryable), and **intelligence** (helping humans understand it). Vendors like Datadog, Dynatrace, and Splunk built billion-dollar businesses by bundling all three behind proprietary formats, query languages, and dashboards.

That bundle is coming apart.

### The forces at work

**From below, storage is commoditizing.** OpenTelemetry standardized the wire format. Parquet and Iceberg standardized columnar storage. DuckDB, ClickHouse, and object stores like S3 made it trivially cheap to store and query telemetry at scale. The marginal cost of storing a GB of logs is collapsing toward $0.023/month. When storage is a commodity, charging $2.50/GB for ingestion becomes indefensible — and enterprises are noticing. Research shows 96% of organizations are actively taking steps to control observability costs, with 70% focused on optimizing existing spend rather than expanding.

**From above, AI is eating the intelligence layer.** LLMs can write SQL. They can scan billions of log lines, correlate traces with metrics, perform root cause analysis, and define SLOs by analyzing historical patterns — all in seconds. They don't need dashboards, proprietary query languages, or pre-built visualizations. Give an LLM access to raw telemetry in an open format and a SQL interface, and it replicates what took observability platforms years and thousands of engineers to build.

The emerging stack looks like this:

```
OpenTelemetry SDKs → Commodity columnar storage → AI agent
```

Everything in between — the dashboards, the query builders, the alert rule editors, the visualization layers — becomes optional middleware. Not immediately worthless, but structurally threatened.

### What gets replaced

Traditional observability platforms provide value through:

- **Dashboards** → AI generates purpose-built visualizations on demand, used for a single investigation, then discarded. Persistent dashboards are artifacts of human cognitive limitation, not engineering necessity.
- **Query languages** → AI writes SQL (or whatever the storage engine speaks). Proprietary query languages like LogQL, PromQL, and SPL become friction, not features.
- **Alert rules** → AI performs continuous inference against raw data, identifying anomalies contextually rather than through static thresholds that humans forgot to update.
- **Root cause analysis** → AI traces causality across all three signal types natively, joining traces to logs to metrics without requiring humans to manually correlate.
- **SLO definition** → AI analyzes historical patterns, understands user impact, and sets thresholds that actually reflect system behavior rather than guesses.

### What remains

Ingest and store. That's it. You need something to receive telemetry and something to persist it in a queryable format. Everything above that layer is intelligence — and intelligence is exactly what LLMs do.

**otelite is a proof of concept for this thesis.** It is the minimal backend an AI agent needs to do observability: receive OTLP, write Parquet, expose SQL. No dashboards. No query language. No visualization engine. Just structured data and a query interface that any LLM can use by shelling out to a CLI.

## Design Principles

**OTel-native.** otelite speaks OTLP/HTTP natively — both protobuf and JSON. No proprietary agents, no custom SDKs, no vendor lock-in at the collection layer. If your application is instrumented with OpenTelemetry, otelite accepts it unchanged.

**Parquet-first storage.** Telemetry is flushed to date-partitioned Parquet files. Parquet is columnar, compressed, and universally supported. DuckDB, Spark, Pandas, Polars, and dozens of other tools can read it natively. Your data never gets trapped in a proprietary format.

**SQL is the interface.** Every query goes through DuckDB's SQL engine. This is a deliberate choice — SQL is the most widely understood query language on earth, and more importantly, it's the language LLMs are best at generating. No proprietary DSL to learn, no query builder to click through.

**CLI-first, agent-friendly.** otelite is designed to be called from a shell. An LLM agent investigating an incident can shell out to `otelite query`, get structured JSON back, reason about the results, and issue follow-up queries. The entire diagnostic loop — from anomaly detection to root cause analysis — can happen programmatically without a human touching a browser.

**Single binary, zero dependencies.** `go build` and you have everything. No databases to run, no message queues to configure, no clusters to manage. This matters for local development, CI/CD pipelines, edge deployments, and anywhere you want telemetry without the overhead of a platform.

**Nothing is dropped.** All OTLP fields are preserved in the Parquet schema. Resource attributes, scope metadata, span events, links, exemplars — everything. The schema is wide because the data model is rich, and AI agents can use all of it.

## Architecture

```
┌──────────────────────┐
│   OTel-instrumented  │
│     applications     │
└──────────┬───────────┘
           │ OTLP/HTTP (protobuf or JSON)
           ▼
┌──────────────────────┐
│    otelite serve     │
│                      │
│  ┌────────────────┐  │
│  │ OTLP Receiver  │  │
│  │ /v1/traces     │  │
│  │ /v1/logs       │  │
│  │ /v1/metrics    │  │
│  └───────┬────────┘  │
│          ▼           │
│  ┌────────────────┐  │
│  │ Memory Buffer  │  │
│  │ + Flush Timer  │  │
│  └───────┬────────┘  │
│          ▼           │
│  ┌────────────────┐  │
│  │ Parquet Writer │  │
│  └────────────────┘  │
└──────────────────────┘
           │
           ▼
┌──────────────────────┐
│   data/              │
│   ├── traces/        │
│   │   └── YYYY-MM-DD │
│   ├── logs/          │
│   │   └── YYYY-MM-DD │
│   └── metrics/       │
│       └── YYYY-MM-DD │
└──────────┬───────────┘
           │
           ▼
┌──────────────────────┐
│   otelite query      │
│   otelite traces     │
│   otelite logs       │     ◄── LLM agents shell out here
│   otelite metrics    │
│                      │
│  ┌────────────────┐  │
│  │ Embedded       │  │
│  │ DuckDB         │  │
│  │ (Parquet glob) │  │
│  └────────────────┘  │
└──────────────────────┘
```

## Install

```bash
go install github.com/davidgeorgehope/otelite/cmd/otelite@latest
```

Or build from source:

```bash
git clone https://github.com/davidgeorgehope/otelite.git
cd otelite
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

## Agent Integration

otelite is built for LLM agents. Here's how an agent might investigate an incident:

```bash
# Step 1: What services are reporting?
otelite services --format json

# Step 2: Any errors in the last hour?
otelite query "
  SELECT service_name, count(*) as error_count
  FROM traces
  WHERE status_code = 'STATUS_CODE_ERROR'
    AND start_time >= epoch_us(now() - INTERVAL '1 hour')
  GROUP BY service_name
  ORDER BY error_count DESC
" --format json

# Step 3: What's failing in the worst service?
otelite query "
  SELECT span_name, count(*) as failures, avg(duration_ms) as avg_duration
  FROM traces
  WHERE service_name = 'payment-service'
    AND status_code = 'STATUS_CODE_ERROR'
    AND start_time >= epoch_us(now() - INTERVAL '1 hour')
  GROUP BY span_name
  ORDER BY failures DESC
" --format json

# Step 4: Get a specific failing trace
otelite query "
  SELECT span_name, parent_span_id, duration_ms, status_code, attributes
  FROM traces
  WHERE trace_id = '...'
  ORDER BY start_time
" --format json

# Step 5: Correlate with logs
otelite query "
  SELECT timestamp, severity_text, body
  FROM logs
  WHERE trace_id = '...'
  ORDER BY timestamp
" --format json

# Step 6: Check if this is a latency regression
otelite query "
  SELECT metric_name, service_name,
         sum / count as avg_ms, max as max_ms
  FROM metrics
  WHERE metric_name = 'http.request.duration'
    AND service_name = 'payment-service'
  ORDER BY timestamp DESC
  LIMIT 20
" --format json
```

Every step returns structured JSON. The agent reasons about each result and decides what to query next. No dashboards opened. No humans clicking through UIs. No context-switching between tabs. Just an AI systematically narrowing from "something's wrong" to "here's the root cause and here's the evidence."

This is what observability looks like when the consumer of telemetry is an LLM, not a human staring at a screen.

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

## Storage Layout

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

## Sending Telemetry

Point any OpenTelemetry SDK or collector at `http://localhost:4318` using the OTLP/HTTP exporter. All three signal types are supported.

Example OTel Collector config:

```yaml
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

## Example Queries

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

## The Bigger Picture

otelite isn't trying to be Datadog. It's not trying to be Grafana. It's not trying to be a platform at all.

It's an answer to a question: *What's the minimum viable backend when the consumer of telemetry is an AI agent instead of a human?

The answer turns out to be surprisingly small. An OTLP receiver, a Parquet writer, and a SQL engine. That's the whole thing. Everything observability platforms spent a decade building on top of that foundation — the dashboards, the query builders, the alerting engines, the visualization layers — was scaffolding for human cognition. Necessary scaffolding, when humans were the ones interpreting telemetry. But scaffolding nonetheless.

When an LLM can write SQL, read structured output, reason about distributed systems, and iterate on queries faster than any human can click through a UI — the scaffolding becomes overhead.

This isn't a prediction about the distant future. The pieces exist today. OpenTelemetry standardized collection. Parquet and DuckDB commoditized storage and query. LLMs can do the reasoning. otelite just wires them together in the simplest possible way and gets out of the road.

## Status

Early stage. The core ingest → store → query loop works. Contributions welcome.

## License

MIT
