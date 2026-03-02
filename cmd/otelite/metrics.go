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

func metricsCmd() *cobra.Command {
	var (
		service    string
		since      time.Duration
		metricName string
		metricType string
		limit      int
	)

	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Query metrics with filters",
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, err := query.Open(dataDir)
			if err != nil {
				return fmt.Errorf("opening query engine: %w", err)
			}
			defer engine.Close()

			q := "SELECT timestamp, service_name, metric_name, metric_type, value_double, value_int, count, sum FROM metrics"

			var conditions []string
			if service != "" {
				conditions = append(conditions, fmt.Sprintf("service_name = '%s'", service))
			}
			if since > 0 {
				cutoff := time.Now().Add(-since).UnixMicro()
				conditions = append(conditions, fmt.Sprintf("timestamp >= %d", cutoff))
			}
			if metricName != "" {
				conditions = append(conditions, fmt.Sprintf("metric_name = '%s'", metricName))
			}
			if metricType != "" {
				conditions = append(conditions, fmt.Sprintf("metric_type = '%s'", strings.ToLower(metricType)))
			}

			if len(conditions) > 0 {
				q += " WHERE " + strings.Join(conditions, " AND ")
			}
			q += " ORDER BY timestamp DESC"
			q += fmt.Sprintf(" LIMIT %d", limit)

			results, columns, err := engine.Query(q)
			if err != nil {
				return fmt.Errorf("querying metrics: %w", err)
			}

			return cli.FormatResults(os.Stdout, results, columns, format)
		},
	}

	cmd.Flags().StringVar(&service, "service", "", "Filter by service name")
	cmd.Flags().DurationVar(&since, "since", 0, "Filter metrics newer than this duration (e.g. 1h, 30m)")
	cmd.Flags().StringVar(&metricName, "name", "", "Filter by metric name")
	cmd.Flags().StringVar(&metricType, "type", "", "Filter by metric type: gauge, sum, histogram, summary")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum number of data points to return")

	return cmd
}
