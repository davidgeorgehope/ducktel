package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/davidhope/otelite/internal/cli"
	"github.com/davidhope/otelite/internal/query"
)

func schemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema [view]",
		Short: "Show table schema (traces, logs, or metrics)",
		Long:  "Show the schema for a view. Defaults to traces. Valid views: traces, logs, metrics.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			view := "traces"
			if len(args) > 0 {
				view = args[0]
			}

			engine, err := query.Open(dataDir)
			if err != nil {
				return fmt.Errorf("opening query engine: %w", err)
			}
			defer engine.Close()

			cols, err := engine.Describe(view)
			if err != nil {
				return fmt.Errorf("getting schema: %w", err)
			}

			var results []map[string]interface{}
			for _, c := range cols {
				results = append(results, map[string]interface{}{
					"column": c.Name,
					"type":   c.Type,
				})
			}

			return cli.FormatResults(os.Stdout, results, []string{"column", "type"}, format)
		},
	}
}
