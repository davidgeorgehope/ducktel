# Agents

ducktel is one Go binary. It receives OTLP/HTTP, writes Parquet, and queries that Parquet with embedded DuckDB.

To query stored telemetry, use [llms.txt](llms.txt). That file lists the MCP tools and the CLI commands. Argument details are in [skills/ducktel/SKILL.md](skills/ducktel/SKILL.md).

## Build

```bash
go build -o ducktel ./cmd/ducktel
```

The DuckDB bindings use CGo. You need a C compiler. The Go version is the `go` line in `go.mod`.

The container image is linux/amd64 only. `go-duckdb` vendors a glibc `libduckdb.a` for that architecture. See `Containerfile`.

## Test

CI in `.github/workflows/ci.yml` runs these commands, in this order.

```bash
go build ./...
go vet ./...
gofmt -l .
go test -race -count=1 -timeout 5m ./...
node --check web/dashboard/*.js
node --test web/dashboard/*.test.mjs
```

`gofmt -l .` must print nothing. Compaction tests are the slow part of `go test`.

The dashboard has no build step and no npm install.

## Layout

- `cmd/ducktel` holds the cobra commands (`serve`, `query`, `traces`, `logs`, `metrics`, `schema`, `services`, `saved`, `testdata`, `mcp`).
- `internal/receiver` accepts OTLP/HTTP and converts it to the writer structs.
- `internal/writer` buffers records, writes Parquet, deletes old date directories, and compacts past days.
- `internal/query` opens DuckDB and defines the `traces`, `logs`, and `metrics` views.
- `internal/telemetry` is the query behavior. MCP and the dashboard API both call it.
- `internal/mcp` registers the MCP tools and adapts their arguments.
- `internal/webapi` registers the `/api` routes.
- `internal/httpauth` checks bearer tokens.
- `internal/savedquery` stores SQL saved queries in `saved_queries.json` and tenant-scoped MCP saved queries in `mcp_saved_queries.json`.
- `internal/cli` formats CLI results as json, table, or csv.
- `internal/testharness` generates the synthetic OTLP that `ducktel testdata` sends.
- `web/dashboard` is the static dashboard. `web/webui.go` embeds it.
- `skills/ducktel` is the agent skill and the schema and SQL references.

## Conventions

Put a change to query results in `internal/telemetry`. Keep `internal/mcp` and `internal/webapi` as adapters. Both surfaces should keep calling the same core function.

Tools stay generic. Do not teach a tool what a particular attribute key means. `tenant` is the exception. Every query tool requires it, and every query filters on the `tenant.id` resource attribute. `list_tenants` takes no tenant and returns only tenant ids. `list_services`, `list_metric_names`, and `list_attribute_keys` require `tenant`.

Timestamps stored in Parquet are Unix microseconds.

One `ducktel serve` process owns a data directory. A second process for that directory exits instead of writing the same files.

When you add, remove, or rename a tool, change `Register` in `internal/mcp/server.go`. Put the same names in `llms.txt`, `README.md`, `skills/ducktel/SKILL.md`, and `ducktel mcp --help`.

`flush_buffer` is registered only when a flush URL is configured. Document that condition with the tool. Do not describe it as always present, and do not drop it.

What is still absent is listed in [PLAN.md](PLAN.md). Do not document a feature that is not on `main`.
