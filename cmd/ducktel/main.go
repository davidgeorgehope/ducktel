package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	dataDir string
	format  string
)

// clampLimit guards LIMIT clauses built by the filtered query commands. The
// value is interpolated (DuckDB cannot bind a LIMIT placeholder), so it must be
// validated as a bounded non-negative integer rather than trusted.
func clampLimit(n int) int {
	const maxLimit = 10000
	switch {
	case n < 0:
		return 0
	case n > maxLimit:
		return maxLimit
	default:
		return n
	}
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "ducktel",
		Short: "Lightweight local OpenTelemetry backend",
		Long:  "ducktel receives OTLP traces, logs, and metrics over HTTP, stores them as Parquet files, and makes them queryable via DuckDB.",
	}

	rootCmd.PersistentFlags().StringVar(&dataDir, "data-dir", "./data", "Directory for trace data storage")
	rootCmd.PersistentFlags().StringVar(&format, "format", "json", "Output format: json, table, csv")

	rootCmd.AddCommand(serveCmd())
	rootCmd.AddCommand(queryCmd())
	rootCmd.AddCommand(schemaCmd())
	rootCmd.AddCommand(servicesCmd())
	rootCmd.AddCommand(tracesCmd())
	rootCmd.AddCommand(logsCmd())
	rootCmd.AddCommand(metricsCmd())
	rootCmd.AddCommand(savedQueryCmd())
	rootCmd.AddCommand(testHarnessCmd())
	rootCmd.AddCommand(mcpCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
