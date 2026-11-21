package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/cli"
	"github.com/davidgeorgehope/ducktel/internal/feedback"
	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
)

func feedbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feedback",
		Short: "Read agent feedback stored as logs",
	}
	cmd.AddCommand(feedbackListCmd())
	cmd.AddCommand(feedbackExportCmd())
	return cmd
}

func feedbackListCmd() *cobra.Command {
	var (
		tenant string
		since  time.Duration
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List feedback reports for one tenant",
		RunE: func(cmd *cobra.Command, args []string) error {
			rows, cols, err := loadFeedback(tenant, since, limit)
			if err != nil {
				return err
			}
			return cli.FormatResults(cmd.OutOrStdout(), rows, cols, format)
		},
	}
	feedbackFlags(cmd, &tenant, &since, &limit)
	return cmd
}

func feedbackExportCmd() *cobra.Command {
	var (
		tenant string
		since  time.Duration
		limit  int
		as     string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export feedback reports as markdown or JSON",
		Long: `Export feedback for one tenant.

The default is markdown a person can paste into a GitHub issue. --as json
prints a JSON array of the report fields.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch as {
			case "markdown", "json":
			default:
				return fmt.Errorf("--as must be markdown or json")
			}
			rows, _, err := loadFeedback(tenant, since, limit)
			if err != nil {
				return err
			}
			reports := reportsFrom(tenant, rows)
			out := cmd.OutOrStdout()
			if as == "json" {
				return cli.FormatResults(out, reportMaps(reports), reportColumns, "json")
			}
			_, err = io.WriteString(out, feedback.Markdown(reports))
			return err
		},
	}
	feedbackFlags(cmd, &tenant, &since, &limit)
	cmd.Flags().StringVar(&as, "as", "markdown", "Output form: markdown or json")
	return cmd
}

func feedbackFlags(cmd *cobra.Command, tenant *string, since *time.Duration, limit *int) {
	cmd.Flags().StringVar(tenant, "tenant", "", "Tenant id (required)")
	cmd.Flags().DurationVar(since, "since", 168*time.Hour, "Reports newer than this duration; 0 means no lower bound")
	cmd.Flags().IntVar(limit, "limit", 50, "Maximum reports to return (cap 1000)")
}

var reportColumns = []string{
	"report_id", "tenant", "tool_name", "category", "severity",
	"summary", "details", "proposed_fix", "example_call",
}

func loadFeedback(tenant string, since time.Duration, limit int) ([]map[string]any, []string, error) {
	if strings.TrimSpace(tenant) == "" {
		return nil, nil, errors.New("--tenant is required")
	}
	engine, err := query.Open(dataDir)
	if err != nil {
		return nil, nil, fmt.Errorf("opening query engine: %w", err)
	}
	defer engine.Close()

	var cutoff time.Time
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}
	rows, cols, err := telemetry.NewCore(engine).ListFeedback(tenant, cutoff, limit)
	if err != nil {
		return nil, nil, err
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	return rows, cols, nil
}

func reportsFrom(tenant string, rows []map[string]any) []feedback.Report {
	out := make([]feedback.Report, 0, len(rows))
	for _, row := range rows {
		out = append(out, feedback.Report{
			ReportID:    text(row["report_id"]),
			Tenant:      tenant,
			ToolName:    text(row["tool_name"]),
			Category:    feedback.Category(text(row["category"])),
			Severity:    feedback.Severity(text(row["severity"])),
			Summary:     text(row["summary"]),
			Details:     text(row["details"]),
			ProposedFix: text(row["proposed_fix"]),
			ExampleCall: text(row["example_call"]),
		})
	}
	return out
}

func reportMaps(reports []feedback.Report) []map[string]any {
	rows := make([]map[string]any, 0, len(reports))
	for _, r := range reports {
		rows = append(rows, map[string]any{
			"report_id":    r.ReportID,
			"tenant":       r.Tenant,
			"tool_name":    r.ToolName,
			"category":     string(r.Category),
			"severity":     string(r.Severity),
			"summary":      r.Summary,
			"details":      r.Details,
			"proposed_fix": r.ProposedFix,
			"example_call": r.ExampleCall,
		})
	}
	return rows
}

func text(v any) string {
	s, _ := v.(string)
	return s
}
