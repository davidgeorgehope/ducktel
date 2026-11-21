# ducktel 🦆

**Observability for AI agents.** Single binary. OTLP in. SQL and MCP out.

Observability tools were built for humans. Dashboards to stare at. Alert rules to click through. Flame graphs to squint at. That made sense when humans were the ones debugging production.

But agents are becoming the ones who deploy code, respond to incidents, and manage infrastructure. And the "AI for observability" tools on the market have it backwards — they're building AI teammates for human SREs. Copilots. Chatbots. Natural language wrappers around dashboards that still assume a human is driving.

**That's the wrong abstraction.** The agent doesn't need a teammate. It needs a tool. It can call the CLI or the MCP tools, get structured data back, and reason about it. An optional dashboard can show the same query to a person. It is off unless you pass `--dashboard`.

ducktel is that tool. It receives OpenTelemetry, stores Parquet, and answers through SQL and through MCP.

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

1. **Telemetry in.** Standard OTLP/HTTP.
2. **SQL and MCP out.** Structured results the agent can reason about.
3. **A CLI.** Something the agent can shell out to.
4. **Saved queries.** A checklist the agent runs on its own schedule. ducktel does not run it for you.

ducktel is the observability backend built for that agent. Not a smarter dashboard. Not an AI teammate. A tool.

## How it works

```
Your App (OTel SDK)  →  ducktel serve  →  Parquet files on disk
                                                    ↓
                         LLM Agent     ←  ducktel query, or ducktel mcp
```

1. **Receive.** The OTLP/HTTP receiver accepts traces, logs, and metrics on port 4318.
2. **Store.** Records are buffered, then flushed to date-partitioned Parquet files.
3. **Query.** Embedded DuckDB reads those files. The CLI, the MCP tools, and the optional dashboard all use that.

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

Every step returns structured JSON. The agent reads each result and chooses the next query.

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

Accepts `POST /v1/traces`, `POST /v1/logs`, `POST /v1/metrics` — protobuf and JSON. Also serves `GET /health` (always unauthenticated, for liveness/readiness probes), `POST /flush` (forces an immediate flush of buffered records; see Authentication below), and `POST /feedback` (stores one agent-feedback log, authorised with the flush token, not the OTLP ingest token).

Binds to `localhost` by default, so a local developer instance isn't silently exposed to the network — pass `--host 0.0.0.0` for containers/VMs.

#### Authentication

Disabled unless a token is configured. OTLP carries no credentials in the message body — auth is transport-level per spec — so senders present it as a header:

```bash
ducktel serve --host 0.0.0.0 --auth-token "$DUCKTEL_AUTH_TOKEN"

# any OTel SDK/collector supports this via one env var, no instrumentation changes needed
export OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer $DUCKTEL_AUTH_TOKEN"
```

`POST /flush` and `POST /feedback` take a **separate** token (`--flush-token` / `DUCKTEL_FLUSH_TOKEN`, falling back to `DUCKTEL_MCP_TOKEN`). `/flush` is a control action requested by the query side, not a data-ingest action, so the process that queries can trigger a flush without holding the OTLP ingest token. `/feedback` stores one feedback log and rejects the ingest token. It does not accept spans, metrics, or arbitrary logs. If the token would cross an untrusted network, put a TLS-terminating proxy in front — ducktel does not terminate TLS itself.

### `ducktel query`

Run arbitrary SQL against traces, logs, or metrics.

```bash
ducktel query "SELECT ..." [--format json|table|csv] [--data-dir ./data]
```

### `ducktel services`

List service names seen in traces. Logs-only and metrics-only names are not included.

### `ducktel traces`

Browse traces with filters.

```bash
ducktel traces [--service name] [--since 1h] [--status error] [--limit 20]
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

### `ducktel feedback`

List or export agent feedback stored as logs for one tenant. Reports are logs under service `ducktel.agent_feedback`.

```bash
ducktel feedback list --tenant acme [--since 168h] [--limit 50]
ducktel feedback export --tenant acme [--since 168h] [--limit 50] [--as markdown|json]
```

`list` uses the global `--format`. `export` defaults to markdown a person can paste into a GitHub issue. `--as json` prints a JSON array of the report fields. `--since 0` means no time lower bound. The default window is 168 hours. The default limit is 50 and the cap is 1000. A row for another tenant is not returned.

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

Sixteen tools. They know about spans, attributes, logs, metrics, and time, and they attach no meaning to an attribute key. `flush_buffer` is registered only when `--flush-url` is set. The other fifteen, including `sql_query` and `submit_feedback`, are always registered. `save_query` will not store `submit_feedback`.

| Tool | Purpose |
|------|---------|
| `list_tenants()` | tenant ids that have stored telemetry. Takes no arguments and returns only tenant ids |
| `list_services(tenant, source)` | service names for one tenant. `source` omitted or `traces` reads spans; `logs` reads log records |
| `list_metric_names(tenant)` | metric names stored for one tenant |
| `list_attribute_keys(tenant, signal, service_name, since_minutes, limit)` | attribute keys seen in a sample of the window (default 60 minutes, at most 2000 rows per signal), not every key ever stored. Each row is `signal` (`traces`, `logs`, or `metrics`), `source` (`attributes` or `resource_attributes`), and `key`. A missing key is not proof it never existed |
| `trace_lookup(trace_id, tenant, detail, limit, include_attributes)` | by default a compact summary of one trace: root span, span count, duration, services, groups of repeated spans with total time and `self_ms` (time not spent in child spans, so the groups add up), `untraced_ms` (root time no child span covers), slowest spans, error spans, WARN-and-above logs; `detail=spans` returns every span, ordered by start time, with attributes unless `include_attributes` is false. The result can be large |
| `span_search(filters, service_name, since_minutes, tenant, limit, min_ms, max_ms, include_attributes)` | spans matching attribute key/value filters, optionally by duration. Attributes are omitted unless `include_attributes` is true |
| `compare_spans(service_name, span_name, span_kind, filters, since_minutes, tenant, min_ms, max_ms, limit)` | why the slow spans differ. Compares a duration range (default, slower than the scope's p95) with the other spans in that scope, and returns the attribute values whose share differs most. `span_kind` defaults to `server` when `span_name` is omitted. Default 15 rows, cap 50 |
| `log_search(trace_id, severity, search, service_name, filters, since_minutes, tenant, limit)` | log records, newest first: exact case-insensitive severity, body substring, attribute filters; `trace_id` returns one trace's records across the whole history |
| `metric_query(metric_name, aggregation, since_minutes, group_by, step, tenant, filters)` | metric aggregation: avg, sum, min, max, count, p50, p95, p99 — `group_by` accepts columns (the OTel key `service.name` names the `service_name` column), `attr.<key>` data-point attributes and `resource.<key>` resource attributes such as `resource.host.name`; `step` (30s, 1m, 5m, 1h) returns a time series with a leading `time` column; `filters` match attributes or resource attributes. Histograms are aggregated over observations (count, sum, avg, min, max, percentiles from buckets, `buckets` for the distribution; cumulative series become increments). `exemplars` returns the concrete requests senders attached to the points (`trace_id`, `span_id`, value), slowest first, narrowed by `min_value`/`max_value`. On a gauge, `count` counts exported points, not events |
| `slow_requests(metric_name, service_name, span_name, filters, since_minutes, tenant, min_value, max_value, limit)` | the slowest requests in one call, each with a brief trace summary (root, span count, untraced time, top repeated span groups with their self time, the 3 slowest spans, errors, first WARN-or-above log). With `metric_name` it follows that metric's exemplars, with min/max in the metric's unit; otherwise it returns the slowest root spans, with min/max in milliseconds. Default 5, cap 20 |
| `list_saved_queries(tenant)` | saved MCP queries for one tenant: `name`, `description`, `tool`, and `arguments`, sorted by name |
| `save_query(tenant, name, tool, arguments, description)` | save a read tool invocation for this tenant after checking the tool and the arguments. The same name replaces the previous query. Will not store `submit_feedback` |
| `run_saved_query(tenant, name, since_minutes, until_minutes)` | run one saved query by name. Optional `since_minutes` and `until_minutes` override the stored window for this run only. An unknown name lists the names saved for that tenant |
| `sql_query(sql, tenant, limit)` | one read-only SELECT or WITH. The views are `spans`, `logs`, and `metrics`. `traces` is not a view. Timestamps are Unix microseconds. `limit` defaults to 200 and is rejected above 1000. No default time window |
| `submit_feedback(tenant, tool_name, category, severity, summary, details, proposed_fix)` | report a tool that was confusing, unhelpful, wrong, or missing, with a concrete proposed fix. Stored as a log for this tenant under service `ducktel.agent_feedback`. `log_search` and the logs dashboard show it. Nothing is sent off the machine unless a sink is configured |
| `flush_buffer()` | make just-received telemetry queryable now (only advertised when `--flush-url` is set) |

`span_search`, `compare_spans`, `log_search`, `slow_requests`, and `metric_query` also take `until_minutes` (default 0, meaning now). It must be less than `since_minutes`. `list_attribute_keys` samples `since_minutes` (default 60) and does not take `until_minutes`. `trace_lookup`, `sql_query`, `flush_buffer`, `list_tenants`, `list_services`, and `list_metric_names` have no time window. Saved queries are per tenant, in a file separate from `ducktel saved` SQL. `run_saved_query` can override that window for one run. A tool with no window rejects the override.

**Time series and resource grouping.** `step` turns `metric_query` into a dashboard time series: one row per (time bucket, group), with a leading `time` column holding the bucket start in Unix microseconds (UTC), ordered by time then groups. `resource.<key>` groups by an OpenTelemetry resource attribute, so questions like CPU per host work: `metric_query(metric_name="system.cpu.utilization", aggregation="avg", group_by="resource.host.name", step="1m", since_minutes=360)`. At most 1500 buckets are allowed per query; `exemplars` ignores `step`.

**Start with `slow_requests` for "why is X slow".** It returns the slowest requests already carrying a brief trace summary, so one call answers the first question. Use `compare_spans` when you want the attribute values that are more common on those slow spans than on the rest. Follow a `trace_id` with `trace_lookup` for the full summary, and use `trace_lookup(detail=spans)` only when every span is really needed. Set `include_attributes` to false on that call to drop the attributes JSON.

**From a number to a request.** A histogram bucket leads to the requests in it:
`metric_query(aggregation="exemplars", min_value=lo, max_value=le)` gives trace ids
the SDK attached to those observations (the standard OTel SDKs do this for sampled
requests), `span_search(min_ms=lo, max_ms=le)` gives every span of that duration,
and `trace_lookup` / `log_search(trace_id=…)` open one of them. In the dashboard
this is a click on a distribution bar, then on a request.

**Dashboard links in tool results.** With `--dashboard-url` (env
`DUCKTEL_DASHBOARD_URL`, e.g. `https://otel.example.com/dashboard/`), each query
result ends with a `dashboard_url:` line that opens the same query in the
dashboard, so an agent can show a person where it found something. Discovery
lists do not add a link. The dashboard's
deep links use the tools' own parameter names, so the link is the query.

**Trace summaries and slow requests over REST.** The dashboard's `/api` routes
wrap the same core functions:

- `GET /api/traces/{trace_id}/summary?tenant=` returns the compact summary object.
- `GET /api/slow?tenant=&metric_name=&service_name=&span_name=&filter=k:v&since_minutes=&min_value=&max_value=&limit=` returns the `slow_requests` array.

The parameters mean the same as in the MCP tools.

**`tenant` is required on every query tool and on `list_services`, `list_metric_names`, and `list_attribute_keys`** — it's in the JSON schema, so a client cannot omit it — and those tools hard-filter on the `tenant.id` resource attribute. A caller can narrow its scope but never widen it. `list_tenants` takes no tenant and returns only tenant ids.

**The MCP token is a separate secret from the OTLP token.** `DUCKTEL_AUTH_TOKEN` grants OTLP ingest (held by senders); `DUCKTEL_MCP_TOKEN` grants read (MCP queries, and `POST /flush`, held by consumers). The flush token also authorises `POST /feedback`, which stores one feedback log and does not accept spans, metrics, or arbitrary logs. A compromised sender must not gain read access to every tenant, and the ingest token must not be able to file feedback — so use a different value for each. In HTTP mode, `--auth-token` is required; ducktel refuses to start without one rather than silently exposing every tenant's data.

**Agent feedback stays on the machine unless you turn a sink on.** `submit_feedback` writes a log for the calling tenant. With no `--feedback-url`, the MCP process writes its own data directory. To store into a running receiver, pass `--feedback-url http://127.0.0.1:4318/feedback`. That route flushes the log before it returns success. A flush error still keeps the `report_id`, so a retry does not write a second log. A second `ducktel serve` on that directory will not store the report; it exits because the directory is locked. The sink is off by default. `--feedback-webhook-url` or `--feedback-github-repo` (with `--feedback-github-token`) sends the agent-written text off the machine. ducktel does not scrub that text. Setting both sinks fails at startup. At most 20 accepted reports per tenant are stored per rolling hour, in memory, and a restart clears the window.

See [`skills/ducktel/SKILL.md`](skills/ducktel/SKILL.md) for the full reference, including flush-latency semantics and dotted-attribute-key gotchas.

#### Optional dashboard (`--dashboard`)

For the occasional human look at the same data, `--http --dashboard` also serves a small read-only dashboard on the same port:

```bash
ducktel mcp --http --port 4319 --auth-token "$READ_TOKEN" --dashboard
# open http://localhost:4319/dashboard and paste the read token
```

- **Off by default.** Without `--dashboard` nothing below is mounted.
- **Read-only views.** Traces (search plus waterfall), logs and metrics, per tenant. The JSON API is a thin wrapper over the same query core the MCP tools use. Routes, from `internal/webapi/handlers.go`: `GET /api/traces`, `GET /api/traces/{trace_id}`, `GET /api/traces/{trace_id}/summary`, `GET /api/slow`, `GET /api/compare`, `GET /api/logs`, `GET /api/metrics`, `GET /api/tenants`, `GET /api/services`, `GET /api/metric-names`, `GET /api/config`, and `POST /api/flush`.
- **Same token, same tenant rule.** Every `/api/*` route requires the MCP read token; every query is filtered by `tenant.id`, exactly like the MCP tools.
- **No new dependencies.** Plain HTML/CSS/JS embedded in the binary; no build step, no CDN.
- **Flush button, only with `--flush-url`.** `POST /api/flush` makes the last `--flush-interval` of buffered telemetry queryable now. It is the same request the `flush_buffer` tool sends, with the same read token; it writes no telemetry and changes no stored data. Without `--flush-url` the button is hidden and the route returns 404.
- **Cheap to leave open.** Picker lists (tenants, services, metric names) are cached for five minutes, so page loads do not rescan every file. Each `/api` request is logged with path, parameters, status, size and duration.

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

Date-partitioned Parquet files. At query time the engine passes the selected files to DuckDB with `read_parquet`. It does not glob the directory. Nothing is dropped. All OTLP fields are preserved. Your data is never trapped in a proprietary format.

Retention is off by default (files accumulate forever) so upgrading an existing deployment never starts deleting data without an explicit opt-in. For any long-lived or container deployment, prefer `--retention 30d` so on-disk growth stays bounded. A maintenance pass runs at startup and then every 10 minutes. It does not follow `--flush-interval`. When `--retention` is set, that pass deletes whole `YYYY-MM-DD` directories older than the window. Deletion is day-granular. A date-shaped directory older than the cutoff is removed in full. Anything else under a signal directory is left alone.

Compaction runs on that same pass, including when `--retention` is off. Today and yesterday (UTC), each hour that started at least 2 hours ago merges from `HH-MM.parquet` into `HH.parquet`. Days strictly before yesterday merge into one `day.parquet` per signal, in name order, so a timestamp filter can skip row groups. If `HH.parquet` or `day.parquet` already exists, leftover source files are deleted rather than merged again.

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

**2. AI as operator.** Give agents their own tools, built for programmatic access and structured output. `ducktel mcp --dashboard` can show that same query to a person. There is no alert UI. The agent decides what matters.

These are fundamentally different products for fundamentally different users. Option 1 optimizes for humans. Option 2 builds for agents.

ducktel is option 2.

The observability stack agents need is simple:

```
OpenTelemetry SDKs → Commodity columnar storage → AI agent
```

OpenTelemetry commoditized ingestion. Parquet and DuckDB commoditized storage. LLMs commoditized intelligence. What's missing is the wiring — a tool that connects telemetry to agents without a human-oriented platform in the middle.

That's ducktel. Single binary. No platform. No vendor. No lock-in.

## Status

Ingest, Parquet storage, DuckDB query, and saved queries work. Bearer-token auth, the MCP server, histogram aggregation in `metric_query`, compaction, and the optional dashboard work too. `ducktel testdata` generates synthetic telemetry. Contributions welcome.

Not implemented yet (see [PLAN.md](PLAN.md)): an OTLP/gRPC receiver, and an HTTP route that runs raw SQL. Query with the CLI, the MCP tools, or the dashboard `/api` routes.

## License

MIT
