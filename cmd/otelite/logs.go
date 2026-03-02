package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/davidhope/otelite/internal/cli"
	"github.com/davidhope/otelite/internal/query"
)

func logsCmd() *cobra.Command {
	var (
		service  string
		since    time.Duration
		severity string
		search   string
		limit    int
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Query logs with filters",
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, err := query.Open(dataDir)
			if err != nil {
				return fmt.Errorf("opening query engine: %w", err)
			}
			defer engine.Close()

			q := "SELECT timestamp, service_name, severity_text, body, trace_id, span_id FROM logs"

			var conditions []string
			if service != "" {
				conditions = append(conditions, fmt.Sprintf("service_name = '%s'", service))
			}
			if since > 0 {
				cutoff := time.Now().Add(-since).UnixMicro()
				conditions = append(conditions, fmt.Sprintf("timestamp >= %d", cutoff))
			}
			if severity != "" {
				conditions = append(conditions, fmt.Sprintf("UPPER(severity_text) = '%s'", strings.ToUpper(severity)))
			}
			if search != "" {
				conditions = append(conditions, fmt.Sprintf("body ILIKE '%%%s%%'", search))
			}

			if len(conditions) > 0 {
				q += " WHERE " + strings.Join(conditions, " AND ")
			}
			q += " ORDER BY timestamp DESC"
			q += fmt.Sprintf(" LIMIT %d", limit)

			results, columns, err := engine.Query(q)
			if err != nil {
				return fmt.Errorf("querying logs: %w", err)
			}

			return cli.FormatResults(os.Stdout, results, columns, format)
		},
	}

	cmd.Flags().StringVar(&service, "service", "", "Filter by service name")
	cmd.Flags().DurationVar(&since, "since", 0, "Filter logs newer than this duration (e.g. 1h, 30m)")
	cmd.Flags().StringVar(&severity, "severity", "", "Filter by severity: debug, info, warn, error, fatal")
	cmd.Flags().StringVar(&search, "search", "", "Search log body text (case-insensitive)")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum number of logs to return")

	return cmd
}
