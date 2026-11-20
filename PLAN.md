# What is left

This file lists work that is not in the tree. Current behavior is in [README.md](README.md), [llms.txt](llms.txt), and [AGENTS.md](AGENTS.md). Do not treat the items below as features you can call.

## Shipped

An older copy of this file called some of these future work. They are on `main`. MCP tools, histogram `metric_query`, compaction (`HH.parquet` for a closed hour, `day.parquet` for an older day), saved queries, bearer tokens, the optional dashboard, and ZSTD Parquet. Timestamps are Unix microseconds. The links above have the details.

## Not built

- An OTLP/gRPC receiver. Ingest is HTTP on port 4318. Nothing listens on 4317.
- An HTTP route that accepts raw SQL. `/api/*` runs the same structured queries as the MCP tools. `ducktel query` is still the SQL entry point.
- A scheduler for saved queries. An agent runs `ducktel saved run-all` itself.
- Tail-based sampling before write.
- Generated DuckDB views that join signals for you. You write the join in SQL. Examples are in [skills/ducktel/references/queries.md](skills/ducktel/references/queries.md).
- A flag to choose the Parquet compression codec.

## Try the OpenTelemetry demo

The [OpenTelemetry demo](https://github.com/open-telemetry/opentelemetry-demo) can export to a local receiver. Add an OTLP/HTTP exporter aimed at `http://host.docker.internal:4318`, then start ducktel and the demo.

```bash
ducktel serve
```

Query with the patterns in [skills/ducktel/references/queries.md](skills/ducktel/references/queries.md). `start_time` is Unix microseconds, and `status_code` is `STATUS_CODE_ERROR`, not `ERROR`.

```bash
ducktel query "SELECT service_name, status_code, count(*) FROM traces WHERE start_time >= epoch_us(now() - INTERVAL '5 minutes') GROUP BY 1, 2 ORDER BY 3 DESC"
ducktel query "SELECT service_name, span_name, status_message FROM traces WHERE status_code = 'STATUS_CODE_ERROR' ORDER BY start_time DESC LIMIT 20"
ducktel query "SELECT service_name, avg(duration_ms) AS avg_ms, percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) AS p95_ms FROM traces GROUP BY 1 ORDER BY avg_ms DESC"
```
