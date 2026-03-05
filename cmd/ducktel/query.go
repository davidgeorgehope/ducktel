package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/cli"
	"github.com/davidgeorgehope/ducktel/internal/query"
)

func queryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "query [sql]",
		Short: "Execute a SQL query against traces",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sql := strings.Join(args, " ")

			engine, err := query.Open(dataDir)
			if err != nil {
				return fmt.Errorf("opening query engine: %w", err)
			}
			defer engine.Close()

			results, columns, err := engine.Query(sql)
			if err != nil {
				return fmt.Errorf("query failed: %w", err)
			}

			return cli.FormatResults(os.Stdout, results, columns, format)
		},
	}
}
