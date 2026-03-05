# ducktel

Lightweight local OpenTelemetry backend that stores telemetry as Parquet files and makes them queryable via DuckDB. Designed to be trivially accessible by LLM agents.

## Architecture

```
Your App (OTel SDK)
    ↓ OTLP (gRPC/HTTP)
ducktel collector
    ↓
Parquet files on disk (partitioned by signal type + time)
    ↓
DuckDB (embedded, queries Parquet in-place)
    ↓
LLM agent (text-to-SQL)
```

## Core Components

### 1. OTLP Receiver
- Accept OTLP over gRPC (port 4317) and HTTP (port 4318)
- Standard OTel protocol — any OTel SDK can send to it
- Lightweight — no full collector needed, just the receiver

### 2. Parquet Writer
- Buffer incoming spans/logs/metrics in memory
- Flush to Parquet files on interval (e.g. every 30s) or buffer size threshold
- Partition scheme: `data/{traces,logs,metrics}/YYYY-MM-DD/HH-MM.parquet`
- Schema follows OTel semantic conventions directly
- Flatten nested attributes into columns where practical

### 3. Query Interface
- Embedded DuckDB reads Parquet files directly (no ingest/copy step)
- Expose a simple SQL query endpoint (HTTP API)
- Auto-discover available Parquet files and create views over them
- Views: `traces`, `logs`, `metrics` — matching OTel signal types

### 4. CLI (LLM-friendly)
- Single binary, all subcommands return structured output (JSON by default, table for humans)
- LLM agents just shell out to `ducktel` commands
- No server needed for querying — DuckDB is embedded, runs in the CLI process
- Subcommands:
  - `ducktel query <sql>` — run SQL, return results as JSON
  - `ducktel schema` — dump table schemas
  - `ducktel services` — list known services
  - `ducktel traces` — shorthand queries with filters (--service, --since, --status)
  - `ducktel logs` — same for logs
- Output format flag: `--format json|table|csv`
- Works as a tool in any agent framework that can call shell commands

## File Layout

```
ducktel/
├── cmd/
│   └── ducktel/          # CLI entrypoint
├── internal/
│   ├── receiver/         # OTLP gRPC/HTTP receiver
│   ├── writer/           # Parquet buffered writer
│   ├── query/            # DuckDB query engine
│   └── cli/              # CLI command definitions
├── data/                 # Default data directory (gitignored)
│   ├── traces/
│   ├── logs/
│   └── metrics/
├── go.mod
└── README.md
```

## Tech Choices

- **Language:** Go — good fit for the receiver (concurrency, OTel SDK support), single binary output
- **Parquet:** parquet-go or apache arrow-go for writing
- **DuckDB:** go-duckdb (CGo bindings) for querying
- **CLI:** cobra or just stdlib flag — keep it minimal

## Parquet Schemas

### Traces
| Column | Type | Source |
|--------|------|--------|
| trace_id | STRING | span.TraceID |
| span_id | STRING | span.SpanID |
| parent_span_id | STRING | span.ParentSpanID |
| service_name | STRING | resource.service.name |
| span_name | STRING | span.Name |
| span_kind | STRING | span.Kind |
| start_time | TIMESTAMP | span.StartTime |
| end_time | TIMESTAMP | span.EndTime |
| duration_ms | DOUBLE | computed |
| status_code | STRING | span.Status.Code |
| status_message | STRING | span.Status.Message |
| attributes | JSON | span.Attributes (flattened common ones as top-level cols too) |

### Logs
| Column | Type | Source |
|--------|------|--------|
| timestamp | TIMESTAMP | log.Timestamp |
| service_name | STRING | resource.service.name |
| severity | STRING | log.SeverityText |
| body | STRING | log.Body |
| trace_id | STRING | log.TraceID |
| span_id | STRING | log.SpanID |
| attributes | JSON | log.Attributes |

### Metrics
| Column | Type | Source |
|--------|------|--------|
| timestamp | TIMESTAMP | datapoint.Timestamp |
| service_name | STRING | resource.service.name |
| metric_name | STRING | metric.Name |
| metric_type | STRING | gauge/sum/histogram |
| value | DOUBLE | datapoint.Value |
| attributes | JSON | datapoint.Attributes |

## Retention

- Simple time-based: delete Parquet files older than N days
- Configured via CLI flag: `--retention 7d`
- Just deletes directories — no compaction, no GC

## CLI Usage

```bash
# Start the collector (receives OTLP, writes Parquet)
ducktel serve
ducktel serve --data-dir ./my-data --retention 30d

# Query with raw SQL (default output: JSON for LLM consumption)
ducktel query "SELECT service_name, count(*) FROM traces GROUP BY 1"
ducktel query "SELECT * FROM traces WHERE status_code = 'ERROR' ORDER BY start_time DESC LIMIT 10"

# Human-friendly table output
ducktel query --format table "SELECT service_name, count(*) FROM traces GROUP BY 1"

# Convenience subcommands (shortcuts for common queries)
ducktel schema                              # show available tables + columns
ducktel services                            # list services seen
ducktel traces --service checkout --since 1h --status error
ducktel logs --service api --severity error --since 30m
```

## MVP Scope (v0.1)

1. OTLP HTTP receiver (skip gRPC initially — simpler)
2. Parquet writer for traces only (most useful signal for debugging)
3. DuckDB query via CLI
4. CLI with `query`, `schema`, `services`, `traces` subcommands (JSON output by default)

## Testing with OTel Demo

The [OpenTelemetry Demo](https://github.com/open-telemetry/opentelemetry-demo) is a microservices e-commerce app with feature flags that inject real failures. Perfect test case for "can an LLM diagnose issues just from telemetry?"

### Setup

The demo's OTel Collector is configured via `src/otel-collector/otelcol-config-extras.yml`. Add ducktel as an additional exporter:

```yaml
exporters:
  otlphttp/ducktel:
    endpoint: http://host.docker.internal:4318

service:
  pipelines:
    traces:
      exporters: [spanmetrics, otlphttp/ducktel]
    logs:
      exporters: [otlphttp/ducktel]
```

Then run the demo alongside ducktel:

```bash
# Terminal 1: start ducktel
ducktel serve

# Terminal 2: start the OTel demo
cd opentelemetry-demo
docker compose up -d
```

Demo UI at `http://localhost:8080/`, feature flags at `http://localhost:8080/feature`.

### Feature Flags to Test

| Flag | What Breaks | What the LLM Should Find |
|------|------------|--------------------------|
| `productCatalogFailure` | GetProduct fails for product `OLJCESPC7Z` | ERROR spans on productcatalogservice for specific product ID |
| `paymentServiceFailure` | charge method errors | ERROR spans on paymentservice during checkout |
| `paymentServiceUnreachable` | checkout can't reach payment | ERROR spans on checkoutservice, connection errors |
| `cartServiceFailure` | EmptyCart fails | ERROR spans on cartservice |
| `adServiceFailure` | GetAds errors 1/10 requests | Intermittent errors on adservice (rate ~10%) |
| `recommendationServiceCacheFailure` | Exponential cache growth | Increasing latency on recommendationservice over time |
| `kafkaQueueProblems` | Queue overload + consumer delay | High latency on kafka-related spans |

### Test Script

```bash
#!/bin/bash
# 1. Start clean, let demo run for 5 min to collect baseline
ducktel serve &
sleep 300

# 2. Enable a failure flag via the flagd UI or API
# (toggle productCatalogFailure on)

# 3. Wait for more data
sleep 300

# 4. Ask the LLM to diagnose
# The LLM runs queries like:
ducktel query "SELECT service_name, status_code, count(*) FROM traces WHERE start_time > now() - INTERVAL '5 minutes' GROUP BY 1, 2 ORDER BY 3 DESC"
ducktel query "SELECT service_name, span_name, status_message FROM traces WHERE status_code = 'ERROR' ORDER BY start_time DESC LIMIT 20"
ducktel query "SELECT service_name, avg(duration_ms), p95(duration_ms) FROM traces GROUP BY 1 ORDER BY 2 DESC"
```

### Success Criteria

An LLM with access to `ducktel query` should be able to:
1. Identify which service is failing
2. Describe the error pattern (constant vs intermittent, specific endpoint vs all)
3. Correlate upstream/downstream impact (e.g. payment failure → checkout failure)
4. Detect latency degradation (cache leak, queue overload)

## Future / Nice-to-Have

- gRPC receiver
- Logs and metrics signals
- Web UI with basic trace viewer
- Auto-generated DuckDB views with common joins
- Tail-based sampling before writing
- Compression tuning for Parquet files
