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

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
