# ducktel 🦆

**Observability for AI agents.** Single binary. OTLP in. SQL out.

Observability tools were built for humans. Dashboards to stare at. Alert rules to click through. Flame graphs to squint at. That made sense when humans were the ones debugging production.

But agents are becoming the ones who deploy code, respond to incidents, and manage infrastructure. And the "AI for observability" tools on the market have it backwards — they're building AI teammates for human SREs. Copilots. Chatbots. Natural language wrappers around dashboards that still assume a human is driving.

**That's the wrong abstraction.** The agent doesn't need a teammate. It needs a tool. Something it can shell out to, get structured data back, and reason about autonomously. No browser. No UI. No human in the loop.

ducktel is that tool. It receives OpenTelemetry data, stores it as Parquet files, and exposes everything through SQL — the one query language every LLM already knows.

```bash
# Start collecting telemetry
ducktel serve

# Agent investigates an incident
ducktel query "SELECT service_name, count(*) as errors FROM traces WHERE status_code = 'STATUS_CODE_ERROR' GROUP BY 1 ORDER BY 2 DESC" --format json

# Agent saves the diagnostic query for continuous monitoring
ducktel saved create "error-spike" "SELECT ..."

# Agent runs all saved queries on its heartbeat loop
ducktel saved run-all
```

## Why this exists

Every "AI SRE" product today puts AI inside a human's observability platform. A chatbot in Datadog. A copilot in Grafana. A natural language interface on top of dashboards that were designed for human eyes.

This helps humans. It doesn't help agents.

An agent operating autonomously — deploying, scaling, remediating — needs something fundamentally different:

1. **Telemetry in** — standard OTLP, no vendor lock-in
2. **SQL out** — structured results it can reason about programmatically
3. **A CLI** — something it can shell out to, not a browser it can't open
4. **Saved queries** — a checklist it runs on its own schedule, not alert rules a human configures

ducktel is the observability backend built for that agent. Not a smarter dashboard. Not an AI teammate. A tool.

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
  \
  --tags payment,errors

# Save a latency check too:
ducktel saved create "p99-latency" \
  "SELECT service_name, span_name, quantile_cont(duration_ms, 0.99) as p99_ms FROM traces GROUP BY 1,2 ORDER BY p99_ms DESC LIMIT 10" \
  --description "Top 10 slowest endpoints by P99" \
  \
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
ducktel serve [--host localhost] [--port 4318] [--flush-interval 30s] [--buffer-size 1000] [--data-dir ./data]
ducktel serve --retention 30d    # recommended for long-lived / container deploys
ducktel serve --host 0.0.0.0 --auth-token "$DUCKTEL_AUTH_TOKEN"   # network-exposed, authenticated
```

Accepts `POST /v1/traces`, `POST /v1/logs`, `POST /v1/metrics` — protobuf and JSON. Also serves `GET /health` (always unauthenticated, for liveness/readiness probes) and `POST /flush` (forces an immediate flush of buffered records; see Authentication below).

Binds to `localhost` by default, so a local developer instance isn't silently exposed to the network — pass `--host 0.0.0.0` for containers/VMs.

#### Authentication

Disabled unless a token is configured. OTLP carries no credentials in the message body — auth is transport-level per spec — so senders present it as a header:

```bash
ducktel serve --host 0.0.0.0 --auth-token "$DUCKTEL_AUTH_TOKEN"

# any OTel SDK/collector supports this via one env var, no instrumentation changes needed
export OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer $DUCKTEL_AUTH_TOKEN"
```

`POST /flush` takes a **separate** token (`--flush-token` / `DUCKTEL_FLUSH_TOKEN`, falling back to `DUCKTEL_MCP_TOKEN`): it's a control action requested by the query side, not a data-ingest action, so the process that queries can trigger a flush without ever holding the write token. If the token would cross an untrusted network, put a TLS-terminating proxy in front — ducktel does not terminate TLS itself.

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
ducktel saved create <name> <sql> [--description "..."] [--tags a,b]
ducktel saved list [--format json|table]
ducktel saved show <name>
ducktel saved run <name>
ducktel saved run-all
ducktel saved delete <name>
```

### `ducktel mcp`

Serve telemetry queries over the Model Context Protocol, for agent consumers that speak MCP instead of shelling out to the CLI.

```bash
ducktel mcp --data-dir /data                          # stdio (default) — process boundary is the trust boundary
ducktel mcp --http --host 0.0.0.0 --port 4319 \
  --auth-token "$DUCKTEL_MCP_TOKEN" \
  --flush-url http://localhost:4318/flush              # network-exposed, authenticated
```

Register it with an MCP client, e.g. Claude Code:

```bash
claude mcp add ducktel -- ducktel mcp --data-dir /data
```

Five generic, domain-agnostic tools:

| Tool | Purpose |
|------|---------|
| `trace_lookup(trace_id, tenant)` | every span of one trace, ordered by start time |
| `span_search(filters, service_name, since_minutes, tenant, limit)` | spans matching attribute key/value filters |
| `log_search(trace_id, severity, search, service_name, filters, since_minutes, tenant, limit)` | log records, newest first: exact case-insensitive severity, body substring, attribute filters; `trace_id` returns one trace's records across the whole history |
| `metric_query(metric_name, aggregation, since_minutes, group_by, tenant, filters)` | metric aggregation: avg, sum, min, max, count, p50, p95, p99 — `group_by` accepts columns or `attr.<key>` data-point attributes; `filters` match attributes or resource attributes |
| `flush_buffer()` | make just-received telemetry queryable now (only advertised when `--flush-url` is set) |

**`tenant` is required on every query tool** — it's in the JSON schema, so a client cannot omit it — and every query hard-filters on the `tenant.id` resource attribute. A caller can narrow its scope but never widen it.

**The MCP token is a separate secret from the OTLP token.** `DUCKTEL_AUTH_TOKEN` grants write (OTLP ingest, held by senders); `DUCKTEL_MCP_TOKEN` grants read (MCP queries, and `POST /flush`, held by consumers). A compromised sender must not gain read access to every tenant, and a read token must not be able to forge telemetry — so use a different value for each. In HTTP mode, `--auth-token` is required; ducktel refuses to start without one rather than silently exposing every tenant's data.

See [`skills/ducktel/SKILL.md`](skills/ducktel/SKILL.md) for the full reference, including flush-latency semantics and dotted-attribute-key gotchas.

### `ducktel testdata`

Generate synthetic OTLP telemetry for testing.

```bash
ducktel testdata [--duration 30s] [--trace-rate 2] [--error-rate 0.05] [--endpoint http://localhost:4318]

# Drive a receiver that requires a token, and tag the data for the MCP tools
ducktel testdata --auth-token "$DUCKTEL_AUTH_TOKEN" --tenant acme

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
    # only needed if the receiver was started with --auth-token
    headers:
      Authorization: "Bearer ${DUCKTEL_AUTH_TOKEN}"

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

Retention is off by default (files accumulate forever) so upgrading an existing deployment never starts deleting data without an explicit opt-in. For any long-lived or container deployment, prefer `--retention 30d` so on-disk growth stays bounded. When enabled it is day-granular: an hourly background sweep deletes whole `YYYY-MM-DD` directories older than the window — no partial-day deletion. A date-shaped directory older than the cutoff is removed in full; anything else under a signal directory is left alone.

Compaction runs automatically, every hour and once at startup: each past day's per-flush files merge into a single `day.parquet` per signal (chronological order preserved, so timestamp filters prune well), replacing dozens of tiny files with one. The current and previous UTC day are never touched, so a merge cannot race the writer around midnight. The merge is crash-safe: an existing `day.parquet` means a previous merge completed, and any leftover per-flush files are deleted rather than merged (merging would double rows).

## Container

A linux/amd64 image is published to GHCR on every push to `main`:

```bash
podman pull ghcr.io/davidgeorgehope/ducktel:latest
podman run --rm -p 4318:4318 --memory=512m --memory-swap=512m \
  -v ducktel-data:/data ghcr.io/davidgeorgehope/ducktel:latest \
  serve --host 0.0.0.0 --port 4318 --data-dir /data --retention 30d
```

`--retention 30d` is recommended for container deploys; without it Parquet files accumulate indefinitely under `/data`.

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

There are two ways to bring AI into observability:

**1. AI as teammate.** Put a chatbot inside Datadog. Add natural language queries to Grafana. Build a copilot that helps SREs navigate dashboards faster. This is what every "AI SRE" startup is doing. It makes existing tools incrementally better for humans.

**2. AI as operator.** Give agents their own tools — purpose-built for programmatic access, structured output, and autonomous reasoning. No dashboards because there's no one to look at them. No alert UIs because the agent decides what matters. No visualization layer because the agent doesn't have eyes.

These are fundamentally different products for fundamentally different users. Option 1 optimizes for humans. Option 2 builds for agents.

ducktel is option 2.

The observability stack agents need is simple:

```
OpenTelemetry SDKs → Commodity columnar storage → AI agent
```

OpenTelemetry commoditized ingestion. Parquet and DuckDB commoditized storage. LLMs commoditized intelligence. What's missing is the wiring — a tool that connects telemetry to agents without a human-oriented platform in the middle.

That's ducktel. Single binary. No platform. No vendor. No lock-in.

## Status

Early stage. The core ingest → store → query → saved queries loop works, plus bearer-token auth on the OTLP and MCP endpoints and an MCP server for agent consumers with mandatory per-tenant isolation. Built-in test harness for generating synthetic data. Contributions welcome.

Not implemented (see `PLAN.md`): OTLP/gRPC, an HTTP SQL query API, and a web UI. Querying is via the CLI and the MCP server.

## License

MIT
