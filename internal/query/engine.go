package query

import (
	"database/sql"
	"fmt"
	"path/filepath"

	_ "github.com/marcboeker/go-duckdb"
)

type ColumnInfo struct {
	Name string
	Type string
}

type Engine struct {
	db      *sql.DB
	dataDir string
}

func Open(dataDir string) (*Engine, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("opening duckdb: %w", err)
	}

	e := &Engine{db: db, dataDir: dataDir}
	if err := e.CreateViews(); err != nil {
		db.Close()
		return nil, err
	}
	return e, nil
}

func (e *Engine) CreateViews() error {
	views := []struct {
		name      string
		signal    string
		emptyCols string
	}{
		{
			name:   "traces",
			signal: "traces",
			emptyCols: `'' as trace_id, '' as span_id, '' as parent_span_id,
				'' as trace_state,
				'' as service_name, '' as span_name, '' as span_kind,
				CAST(0 AS BIGINT) as start_time, CAST(0 AS BIGINT) as end_time,
				CAST(0.0 AS DOUBLE) as duration_ms,
				'' as status_code, '' as status_message, '' as attributes,
				'' as resource_attributes, '' as scope_name, '' as scope_version,
				'' as events, '' as links`,
		},
		{
			name:   "logs",
			signal: "logs",
			emptyCols: `CAST(0 AS BIGINT) as timestamp, CAST(0 AS BIGINT) as observed_timestamp,
				'' as trace_id, '' as span_id,
				CAST(0 AS INTEGER) as severity_number, '' as severity_text,
				'' as body, '' as attributes, '' as resource_attributes,
				'' as service_name, '' as scope_name, '' as scope_version,
				CAST(0 AS UINTEGER) as flags, '' as event_name`,
		},
		{
			name:   "metrics",
			signal: "metrics",
			emptyCols: `'' as metric_name, '' as metric_description, '' as metric_unit,
				'' as metric_type,
				CAST(0 AS BIGINT) as timestamp, CAST(0 AS BIGINT) as start_timestamp,
				CAST(0.0 AS DOUBLE) as value_double, CAST(0 AS BIGINT) as value_int,
				CAST(0 AS UBIGINT) as count, CAST(0.0 AS DOUBLE) as sum,
				CAST(0.0 AS DOUBLE) as min, CAST(0.0 AS DOUBLE) as max,
				'' as bucket_counts, '' as explicit_bounds, '' as quantile_values,
				'' as attributes, '' as resource_attributes,
				'' as service_name, '' as scope_name, '' as scope_version,
				'' as exemplars, CAST(0 AS UINTEGER) as flags,
				false as is_monotonic, '' as aggregation_temporality`,
		},
	}

	for _, v := range views {
		glob := filepath.Join(e.dataDir, v.signal, "**", "*.parquet")
		q := fmt.Sprintf(`CREATE OR REPLACE VIEW %s AS SELECT * FROM read_parquet('%s', union_by_name=true)`, v.name, glob)
		if _, err := e.db.Exec(q); err != nil {
			// No files yet — create empty view with correct schema
			empty := fmt.Sprintf(`CREATE OR REPLACE VIEW %s AS SELECT %s WHERE false`, v.name, v.emptyCols)
			if _, err2 := e.db.Exec(empty); err2 != nil {
				return fmt.Errorf("creating empty %s view: %w", v.name, err2)
			}
		}
	}
	return nil
}

func (e *Engine) Query(sqlStr string) ([]map[string]interface{}, []string, error) {
	rows, err := e.db.Query(sqlStr)
	if err != nil {
		return nil, nil, fmt.Errorf("executing query: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, fmt.Errorf("getting columns: %w", err)
	}

	var results []map[string]interface{}
	for rows.Next() {
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, nil, fmt.Errorf("scanning row: %w", err)
		}

		row := make(map[string]interface{}, len(columns))
		for i, col := range columns {
			val := values[i]
			if b, ok := val.([]byte); ok {
				val = string(b)
			}
			row[col] = val
		}
		results = append(results, row)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating rows: %w", err)
	}

	return results, columns, nil
}

func (e *Engine) Describe(view string) ([]ColumnInfo, error) {
	rows, err := e.db.Query(fmt.Sprintf("DESCRIBE %s", view))
	if err != nil {
		return nil, fmt.Errorf("describing %s: %w", view, err)
	}
	defer rows.Close()

	var cols []ColumnInfo
	for rows.Next() {
		var name, typ string
		var null, key, def, extra sql.NullString
		if err := rows.Scan(&name, &typ, &null, &key, &def, &extra); err != nil {
			return nil, fmt.Errorf("scanning column info: %w", err)
		}
		cols = append(cols, ColumnInfo{Name: name, Type: typ})
	}
	return cols, rows.Err()
}

// Schema describes the traces view (kept for backward compatibility).
func (e *Engine) Schema() ([]ColumnInfo, error) {
	return e.Describe("traces")
}

func (e *Engine) Close() error {
	return e.db.Close()
}
