package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/cli"
	"github.com/davidgeorgehope/ducktel/internal/query"
)

func servicesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "services",
		Short: "List distinct service names",
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, err := query.Open(dataDir)
			if err != nil {
				return fmt.Errorf("opening query engine: %w", err)
			}
			defer engine.Close()

			results, columns, err := engine.Query("SELECT DISTINCT service_name FROM traces ORDER BY 1")
			if err != nil {
				return fmt.Errorf("querying services: %w", err)
			}

			return cli.FormatResults(os.Stdout, results, columns, format)
		},
	}
}
