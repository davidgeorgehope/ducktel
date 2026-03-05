# ducktel Development Guide

## Project Structure

```
cmd/ducktel/
  main.go           # Cobra root command, global flags (--data-dir, --format)
  serve.go          # OTLP HTTP receiver command
  query.go          # Raw SQL query command
  traces.go         # Filtered trace queries
  logs.go           # Filtered log queries
  metrics.go        # Filtered metric queries
  schema.go         # Schema introspection
  services.go       # Distinct service listing

internal/
  receiver/
    receiver.go     # OTLP/HTTP server, protobuf/JSON parsing, OTLP→internal conversion
  writer/
    writer.go       # Buffered Parquet writer with periodic flush
    schema.go       # TraceSpan, LogRecord, MetricPoint struct definitions
  query/
    engine.go       # Embedded DuckDB, view creation, SQL execution
  cli/
    format.go       # JSON/table/CSV output formatting

integration_test.go # End-to-end tests (receiver → writer → query)
```

## Key Interfaces

```go
// Consumer is the interface the receiver sends data to (implemented by Writer)
type Consumer interface {
    Add(spans []writer.TraceSpan)
    AddLogs(records []writer.LogRecord)
    AddMetrics(points []writer.MetricPoint)
}
```

## Dependencies

- `go.opentelemetry.io/proto/otlp` — OTLP protobuf definitions
- `github.com/parquet-go/parquet-go` — Pure-Go Parquet writer
- `github.com/marcboeker/go-duckdb` — Go DuckDB bindings (CGo, needs C compiler)
- `github.com/spf13/cobra` — CLI framework
- `google.golang.org/protobuf` — Protobuf runtime

## Building

```bash
go build -o ducktel ./cmd/ducktel
```

Requires CGo for DuckDB bindings. Ensure a C compiler is available (gcc/clang).

## Testing

```bash
go test -v ./...                    # all tests
go test -v -run TestTraces ./       # single test
go test -v -run TestLogs ./         # log tests
go test -v -run TestMetrics ./      # metric tests
```

Integration tests (`integration_test.go`) spin up a real receiver and writer, send OTLP protobuf payloads, flush to Parquet, and query via DuckDB.

## Data Flow

1. **Receiver** (`internal/receiver`) listens on OTLP/HTTP endpoints
2. Incoming protobuf/JSON is unmarshaled to OTLP proto types
3. `convertSpans`/`convertLogs`/`convertMetrics` transforms OTLP proto → internal Go structs
4. Structs passed to **Writer** (`internal/writer`) via `Consumer` interface
5. Writer buffers in memory, flushes to date-partitioned Parquet files on interval or buffer full
6. **Query Engine** (`internal/query`) creates DuckDB views over Parquet globs
7. SQL queries execute against views, results returned as `[]map[string]interface{}`
8. **CLI formatter** (`internal/cli`) renders results as JSON/table/CSV

## Adding a New Signal Field

1. Add the field to the appropriate struct in `internal/writer/schema.go`
2. Populate it in the conversion function in `internal/receiver/receiver.go`
3. If needed, add it to the DuckDB view schema in `internal/query/engine.go` (the `createEmptyView` fallback)
4. Add test coverage in `integration_test.go`

## Adding a New CLI Subcommand

1. Create `cmd/ducktel/<name>.go` following the pattern in `traces.go`
2. Register it in `main.go` with `rootCmd.AddCommand(<name>Cmd())`
3. Use the `query.Engine` for data access and `cli.FormatResults` for output

## OTLP Content Types

The receiver accepts both:
- `application/x-protobuf` — binary protobuf (default for OTel SDKs)
- `application/json` — JSON-encoded protobuf (via `protojson.Unmarshal`)

## Parquet File Naming

Files are written to `{dataDir}/{signal}/YYYY-MM-DD/HH-MM.parquet` where the timestamp is the flush time (not the data time). Multiple flushes in the same minute append a counter suffix.
