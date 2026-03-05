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
ducktel serve                          # default: port 4318, flush every 30s
ducktel serve --port 9090              # custom port
ducktel serve --flush-interval 10s     # flush every 10 seconds
```

Endpoints: `POST /v1/traces`, `POST /v1/logs`, `POST /v1/metrics`, `GET /` (health).

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

## SQL Query Patterns

The query engine uses embedded DuckDB. Three views are available: `traces`, `logs`, `metrics`.

For schema details, see [references/schema.md](references/schema.md).
For common query patterns, see [references/queries.md](references/queries.md).

## Key Conventions

- **Timestamps** are Unix microseconds (`int64`). Use `epoch_us(ts)` in DuckDB to convert.
- **duration_ms** is precomputed as `float64` milliseconds.
- **Attributes** (attributes, resource_attributes, events, links, exemplars) are stored as JSON strings. Use DuckDB JSON functions to query: `json_extract_string(attributes, '$.http.method')`.
- **Status codes** are strings: `STATUS_CODE_OK`, `STATUS_CODE_ERROR`, `STATUS_CODE_UNSET`.
- **Span kinds** are strings: `SPAN_KIND_SERVER`, `SPAN_KIND_CLIENT`, `SPAN_KIND_PRODUCER`, `SPAN_KIND_CONSUMER`, `SPAN_KIND_INTERNAL`.
- **Metric types** are lowercase strings: `gauge`, `sum`, `histogram`, `summary`.

## Development

For project structure and development guide, see [references/development.md](references/development.md).
