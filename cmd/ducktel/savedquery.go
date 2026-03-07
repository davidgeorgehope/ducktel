package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/cli"
	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/savedquery"
)

func savedQueryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "saved",
		Aliases: []string{"sq"},
		Short:   "Manage saved queries — the LLM's checklist for periodic diagnostics",
	}

	cmd.AddCommand(savedQueryCreateCmd())
	cmd.AddCommand(savedQueryListCmd())
	cmd.AddCommand(savedQueryShowCmd())
	cmd.AddCommand(savedQueryDeleteCmd())
	cmd.AddCommand(savedQueryRunCmd())
	cmd.AddCommand(savedQueryRunAllCmd())

	return cmd
}

func savedQueryCreateCmd() *cobra.Command {
	var (
		description string
		tags        []string
	)

	cmd := &cobra.Command{
		Use:   "create <name> <sql>",
		Short: "Save a query for periodic execution by an LLM agent",
		Long: `Save a SQL query with a name. ducktel never runs queries automatically.
The agent decides when to run them and what to do with the results.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := savedquery.NewStore(dataDir)
			q := savedquery.Query{
				Name:        args[0],
				Description: description,
				SQL:         args[1],
				Tags:        tags,
			}
			if err := store.Save(q); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "Saved query %q created.\n", args[0])
			return nil
		},
	}

	cmd.Flags().StringVar(&description, "description", "", "What this query checks")
	cmd.Flags().StringSliceVar(&tags, "tags", nil, "Tags for categorization (e.g. errors,latency,slo)")

	return cmd
}

func savedQueryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all saved queries",
		RunE: func(cmd *cobra.Command, args []string) error {
			store := savedquery.NewStore(dataDir)
			queries, err := store.List()
			if err != nil {
				return err
			}

			if format == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(queries)
			}

			// Table format
			columns := []string{"name", "description", "tags", "updated_at"}
			var results []map[string]interface{}
			for _, q := range queries {
				results = append(results, map[string]interface{}{
					"name":        q.Name,
					"description": q.Description,
					"tags":        strings.Join(q.Tags, ","),
					"updated_at":  q.UpdatedAt.Format(time.RFC3339),
				})
			}
			return cli.FormatResults(os.Stdout, results, columns, format)
		},
	}
}

func savedQueryShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show details of a saved query including its SQL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := savedquery.NewStore(dataDir)
			q, err := store.Get(args[0])
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(q)
		},
	}
}

func savedQueryDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a saved query",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := savedquery.NewStore(dataDir)
			if err := store.Delete(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "Saved query %q deleted.\n", args[0])
			return nil
		},
	}
}

func savedQueryRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <name>",
		Short: "Execute a saved query and return results",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := savedquery.NewStore(dataDir)
			q, err := store.Get(args[0])
			if err != nil {
				return err
			}

			engine, err := query.Open(dataDir)
			if err != nil {
				return fmt.Errorf("opening query engine: %w", err)
			}
			defer engine.Close()

			start := time.Now()
			results, columns, queryErr := engine.Query(q.SQL)
			duration := time.Since(start)

			result := savedquery.RunResult{
				Name:       q.Name,
				SQL:        q.SQL,
				RanAt:      time.Now().UTC(),
				DurationMs: float64(duration.Microseconds()) / 1000.0,
			}

			if queryErr != nil {
				result.Error = queryErr.Error()
			} else {
				result.RowCount = len(results)
				result.Columns = columns
				result.Results = results
			}

			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(result)
		},
	}
}

func savedQueryRunAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run-all",
		Short: "Execute all saved queries and return results — the LLM's heartbeat check",
		Long: `Runs every saved query and returns all results in a single JSON array.
Designed for an LLM agent's periodic check loop: one command, all the 
diagnostics, the agent reasons about what needs attention.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store := savedquery.NewStore(dataDir)
			queries, err := store.List()
			if err != nil {
				return err
			}

			if len(queries) == 0 {
				fmt.Fprintln(os.Stdout, "[]")
				return nil
			}

			engine, err := query.Open(dataDir)
			if err != nil {
				return fmt.Errorf("opening query engine: %w", err)
			}
			defer engine.Close()

			var allResults []savedquery.RunResult
			for _, q := range queries {
				start := time.Now()
				results, columns, queryErr := engine.Query(q.SQL)
				duration := time.Since(start)

				result := savedquery.RunResult{
					Name:       q.Name,
					SQL:        q.SQL,
					RanAt:      time.Now().UTC(),
					DurationMs: float64(duration.Microseconds()) / 1000.0,
				}

				if queryErr != nil {
					result.Error = queryErr.Error()
				} else {
					result.RowCount = len(results)
					result.Columns = columns
					result.Results = results
				}

				allResults = append(allResults, result)
			}

			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(allResults)
		},
	}
}
