package cli

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
)

func FormatResults(w io.Writer, results []map[string]interface{}, columns []string, format string) error {
	switch format {
	case "json":
		return formatJSON(w, results)
	case "table":
		return formatTable(w, results, columns)
	case "csv":
		return formatCSV(w, results, columns)
	default:
		return fmt.Errorf("unknown format: %s", format)
	}
}

func formatJSON(w io.Writer, results []map[string]interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

func formatTable(w io.Writer, results []map[string]interface{}, columns []string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	// Header
	for i, col := range columns {
		if i > 0 {
			fmt.Fprint(tw, "\t")
		}
		fmt.Fprint(tw, col)
	}
	fmt.Fprintln(tw)

	// Rows
	for _, row := range results {
		for i, col := range columns {
			if i > 0 {
				fmt.Fprint(tw, "\t")
			}
			fmt.Fprintf(tw, "%v", row[col])
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

func formatCSV(w io.Writer, results []map[string]interface{}, columns []string) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(columns); err != nil {
		return err
	}
	for _, row := range results {
		record := make([]string, len(columns))
		for i, col := range columns {
			record[i] = fmt.Sprintf("%v", row[col])
		}
		if err := cw.Write(record); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
