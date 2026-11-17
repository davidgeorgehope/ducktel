---
name: ducktel
description: >
  Work with ducktel — a lightweight, single-binary OpenTelemetry backend for LLM agent diagnostics.
  Use when querying OTLP traces/logs/metrics via the ducktel CLI, writing DuckDB SQL against Parquet-stored
  telemetry, diagnosing issues from OTel data, developing the ducktel Go codebase, or running the OTLP receiver.
  TRIGGER when: code imports ducktel packages, user mentions ducktel CLI commands (serve, query, traces, logs,
  metrics, schema, services), or user works with the ducktel project directory.
---

# ducktel

A lightweight, single-binary OpenTelemetry backend. Receives OTLP/HTTP, stores to Parquet, queries via embedded DuckDB.

## Architecture

```
OTel-instrumented apps
    ↓ OTLP/HTTP (protobuf or JSON)
ducktel serve  (receiver → buffer → Parquet writer)
    ↓
data/{traces,logs,metrics}/YYYY-MM-DD/HH-MM.parquet
    ↓
ducktel query/traces/logs/metrics  (embedded DuckDB)
    ↓
JSON / table / CSV output → LLM agents or humans
```

## CLI Reference

All commands accept `--data-dir` (default: `./data`) and `--format json|table|csv` (default: `json`).

### `ducktel serve`

Start the OTLP HTTP receiver.

```bash
ducktel serve                          # default: localhost:4318, flush every 30s
ducktel serve --port 9090              # custom port
ducktel serve --host 0.0.0.0           # all interfaces (containers)
ducktel serve --flush-interval 10s     # flush every 10 seconds
ducktel serve --buffer-size 5000       # records per signal that force an early flush
ducktel serve --retention 30d          # delete date-partition directories older than 30 days
ducktel serve --auth-token secret      # require a bearer token on OTLP ingest
ducktel serve --flush-token secret2    # ...and a different one on POST /flush
```

Endpoints: `POST /v1/traces`, `POST /v1/logs`, `POST /v1/metrics`, `GET /health`,
and `POST /flush` (see Authentication).


#### Retention

Off by default — Parquet files accumulate indefinitely unless `--retention` is
set. For any long-lived or container deployment, prefer `--retention 30d` so disk
growth stays bounded. When set, an hourly background sweep deletes whole
`traces|logs|metrics/YYYY-MM-DD` directories older than the window:

```bash
ducktel serve --retention 30d     # bare day count
ducktel serve --retention 720h    # or any Go duration
```

- Day-granular, not per-record: a directory is either kept whole or removed
  whole. There is no partial-day pruning.
- **Compaction runs on the same hourly/startup schedule, always** (it does not
  need `--retention`): each past day's per-flush files merge into one
  `day.parquet` per signal, chronological order preserved. The current and
  previous UTC day are never touched, so a merge cannot race the writer. Crash-safe: an existing
  `day.parquet` means the merge finished, so leftover per-flush files are
  deleted rather than merged (merging would double rows). This is the
  structural fix for the "every query opens every file" pathology — a
  day becomes one file instead of ~2880.
- A directory name that doesn't parse as `YYYY-MM-DD` is left alone rather
  than guessed at.
- Runs once immediately on startup (so a long-stopped process catches up on
  backlog) and then hourly, independent of `--flush-interval`.
- Deletions are logged individually (`retention: removed <path> (older than
  <window>)`), so what got pruned and when is always visible in the process
  log, not silent.

#### Authentication

Disabled unless a token is configured. Set it with `--auth-token` or
`DUCKTEL_AUTH_TOKEN` (an explicit flag wins over the environment):

```bash
DUCKTEL_AUTH_TOKEN=secret ducktel serve --host 0.0.0.0
```

Senders must then present `Authorization: Bearer <token>`. Every OTel SDK
supports this via one environment variable, so no instrumentation changes are
needed:

```bash
export OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer secret"
```

- OTLP carries no credentials in the message body — per the spec, auth is
  transport-level, hence headers.
- `GET /health` stays unauthenticated so liveness/readiness probes keep working.
- A 401 is returned for a missing, malformed, or wrong credential. The scheme
  name is case-insensitive; the `Bearer ` prefix is required.
- **`POST /flush` is a separate credential.** It takes `--flush-token` /
  `DUCKTEL_FLUSH_TOKEN`, falling back to `DUCKTEL_MCP_TOKEN`, so the process that
  queries can also ask for a flush without being handed the write token. The
  ingest token is *not* accepted there. See the MCP section for the full model.
- **TLS is not terminated by ducktel.** If the token would cross an untrusted
  network, front the receiver with a proxy or ingress. OTLP/HTTP is
  request/response rather than a persistent connection, so a terminating proxy is
  a natural fit.
- Starting without a token while bound to a non-loopback address logs a warning.

### `ducktel query [sql]`

Execute raw DuckDB SQL against the `traces`, `logs`, or `metrics` views.

```bash
ducktel query "SELECT * FROM traces LIMIT 5"
ducktel query "SELECT service_name, count(*) FROM traces GROUP BY 1" --format table
```

### `ducktel traces`

Query traces with convenience filters.

| Flag | Description | Example |
|------|-------------|---------|
| `--service` | Filter by service name | `--service api-gateway` |
| `--since` | Spans newer than duration | `--since 1h`, `--since 30m` |
| `--status` | Filter by status: error, ok, unset | `--status error` |
| `--limit` | Max spans (default: 20) | `--limit 100` |

### `ducktel logs`

Query logs with convenience filters.

| Flag | Description | Example |
|------|-------------|---------|
| `--service` | Filter by service name | `--service payment-svc` |
| `--since` | Logs newer than duration | `--since 2h` |
| `--severity` | Filter: debug, info, warn, error, fatal | `--severity error` |
| `--search` | Case-insensitive body text search | `--search "timeout"` |
| `--limit` | Max logs (default: 50) | `--limit 200` |

### `ducktel metrics`

Query metrics with convenience filters.

| Flag | Description | Example |
|------|-------------|---------|
| `--service` | Filter by service name | `--service web-frontend` |
| `--since` | Metrics newer than duration | `--since 4h` |
| `--name` | Filter by metric name | `--name http.request.duration` |
| `--type` | Filter: gauge, sum, histogram, summary | `--type histogram` |
| `--limit` | Max points (default: 50) | `--limit 100` |

### `ducktel schema [view]`

Show column names and types. Default view: `traces`. Valid: `traces`, `logs`, `metrics`.

### `ducktel services`

List distinct service names from traces.

### `ducktel mcp`

Serve telemetry queries over the Model Context Protocol, for agent consumers.

Five generic, domain-agnostic tools. They know about spans, attributes, logs,
metrics and time, and attach no meaning to any attribute key — domain
interpretation is the caller's job.

| Tool | Purpose |
|------|---------|
| `trace_lookup(trace_id, tenant)` | every span of one trace, ordered by start time |
| `span_search(filters, service_name, since_minutes, tenant, limit)` | spans matching attribute key/value filters |
| `log_search(trace_id, severity, search, service_name, filters, since_minutes, tenant, limit)` | log records, newest first: exact case-insensitive severity, body substring, attribute filters; `trace_id` returns one trace's records across the whole history |
| `metric_query(metric_name, aggregation, since_minutes, group_by, tenant, filters)` | metric aggregation: avg, sum, min, max, count, p50, p95, p99 |
| `flush_buffer()` | make just-received telemetry queryable now (see Flush semantics) |

**Tenant isolation is mandatory.** `tenant` is a *required* parameter on every query
tool — it is in the JSON schema, so a client cannot omit it — and every query
hard-filters on the `tenant.id` resource attribute. A caller can restrict its scope
but never widen it. `flush_buffer` takes no arguments and touches no data.

**Dotted attribute keys work.** `span_search` takes filters as key/value pairs and
quotes the key internally, because DuckDB treats `$.service.name` as field "service"
then "name" and silently returns NULL. See the note under SQL Query Patterns.

**Time windows default to 60 minutes.** Pass `since_minutes` for a wider range; a wide
window scans every Parquet file in it, so it can be expensive.

#### Two transports, two trust domains

**stdio (default)** — the process boundary is the trust boundary, so no token is
needed:

```bash
ducktel mcp --data-dir /data
```

```json
{"command": "ducktel", "args": ["mcp", "--data-dir", "/data"]}
```

**HTTP (`--http`)** — for remote consumers, e.g. an agent on another host:

```bash
ducktel mcp --http --host 0.0.0.0 --port 4319 --data-dir /data \
  --auth-token "$DUCKTEL_MCP_TOKEN" --flush-url http://localhost:4318/flush
```

Served on `--base-path` (default `/mcp`), guarded by the same RFC 6750 bearer check
as OTLP. `/health` stays unauthenticated for probes.

**The MCP token is NOT the OTLP token, and the difference is the point.** They are
separate trust domains:

| token | grants | held by |
|-------|--------|---------|
| `DUCKTEL_AUTH_TOKEN` | **write** — OTLP ingest | senders |
| `DUCKTEL_MCP_TOKEN` | **read** — MCP queries, and `POST /flush` | consumers |

Use a different value for each. A compromised sender must not gain read access to
every tenant, and a read token must not be able to forge telemetry. Verified: the
read token is rejected (401) on `/v1/traces`, and the write token is rejected (401)
on `/mcp` and on `/flush`.

#### Flush semantics

The MCP server reads Parquet **files**, so telemetry still in the receiver's memory
buffer is invisible to every tool. Ingest → queryable latency is therefore up to one
flush interval. Three triggers:

1. the `--flush-interval` timer (default 30s)
2. the in-memory buffer reaching 1000 records of a signal — a busy instance
   flushes more often than the interval suggests
3. an explicit request: the `flush_buffer` tool in HTTP mode, which calls the
   writer's `POST /flush`

`--flush-url` is what enables the tool; without it the tool is not advertised at all.

**`POST /flush` is authorised by the MCP token, not the OTLP token.** The query
container is given only the read token, so it can ask the writer for a flush but can
never write telemetry. The writer holds both tokens precisely so it can accept that
call. It sits on the OTLP port, so wherever OTLP is exposed, `/flush` is reachable
too — token-guarded, and unable to write. Block the path at the ingress if that is
unwanted.

Clients that cannot call a tool can force a flush directly:

```bash
curl -X POST -H "Authorization: Bearer $DUCKTEL_MCP_TOKEN" https://otel.example.org/flush
```

**Anthropic's connector** takes the token as `authorization_token` and sends it as
`Authorization: Bearer`, so a static token works — the server compares it directly
and advertises no OAuth discovery endpoint. There is no custom-header option, so a
scheme other than Bearer would not be usable there.


## SQL Query Patterns

The query engine uses embedded DuckDB. Three views are available: `traces`, `logs`, `metrics`.

For schema details, see [references/schema.md](references/schema.md).
For common query patterns, see [references/queries.md](references/queries.md).

## Key Conventions

- **Timestamps** are Unix microseconds (`int64`). Use `epoch_us(ts)` in DuckDB to convert.
- **duration_ms** is precomputed as `float64` milliseconds.
- **Attributes** (attributes, resource_attributes, events, links, exemplars) are stored as JSON strings. Query them with DuckDB JSON functions — but **quote dotted keys**: `json_extract_string(attributes, '$."http.method"')`. DuckDB reads `$.http.method` as field `http` then field `method` and returns NULL, so the unquoted form silently matches nothing for every OTel semantic-convention key. `attributes ->> 'http.method'` also works. See [references/schema.md](references/schema.md).
- **Status codes** are strings: `STATUS_CODE_OK`, `STATUS_CODE_ERROR`, `STATUS_CODE_UNSET`.
- **Span kinds** are strings: `SPAN_KIND_SERVER`, `SPAN_KIND_CLIENT`, `SPAN_KIND_PRODUCER`, `SPAN_KIND_CONSUMER`, `SPAN_KIND_INTERNAL`.
- **Metric types** are lowercase strings: `gauge`, `sum`, `histogram`, `summary`.
- **`metric_query` on histograms** (`http.server.duration` and friends from the standard OTel instrumentations) aggregates observations: `count` and `sum` of observations, `avg` = sum/count, `min`/`max` as reported by the points, and `p50`/`p95`/`p99` interpolated from the merged buckets (explicit-bucket histograms only; exponential histograms support count/sum/avg/min/max). Cumulative series are turned into increments inside the window, so a long-running process is not counted from its start. The type is taken from the metric's most recent point.
- **`count` on a gauge or sum counts exported points, not events.** A gauge re-exported every interval is counted once per export, and percentiles over it weight each export equally. To count events, use a counter (`sum`) or a histogram.
- **`metric_query` grouping:** `group_by` is comma-separated; each key is a plain column (`service_name`, `metric_type`, …) or a data-point attribute written `attr.<key>` / `attributes.<key>` — e.g. `group_by: "attr.kind"` splits the aggregate by the metric point's `attributes` JSON key `kind`, and the result rows carry `attr.kind` as their column name. Attribute keys are validated against `[A-Za-z0-9_.-]`. `filters` matches attribute key/value pairs against data-point attributes OR resource attributes, same as `span_search`.
- **Filter values are bound, not interpolated.** The `traces`/`logs`/`metrics` commands pass filter values to DuckDB as parameters, so a service name or search term containing quotes is matched literally — it cannot alter the query. `--limit` is validated and capped at 10000.
- **`query` and `saved run` execute raw SQL by design.** There is no sandbox on `ducktel query "<sql>"`; it runs whatever you give it, including DDL. That is intentional (it is the SQL escape hatch for agents), so treat its input as trusted.

## Development

For project structure and development guide, see [references/development.md](references/development.md).
