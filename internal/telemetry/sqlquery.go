package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	sqlQueryDefaultLimit = 200
	sqlQueryMaxLimit     = 1000
	sqlQueryTimeout      = 15 * time.Second

	sqlQueryHint = "SQL failed. The views are spans, logs, and metrics. Their columns are in the sql_query tool description. Call list_attribute_keys for attribute keys. Quote a dotted JSON key, for example json_extract_string(attributes, '$.\"http.method\"')."

	sqlRejectShape   = "only a single SELECT or WITH is allowed"
	sqlRejectVisible = "only spans, logs, and metrics are visible"
)

// SQLQuery runs one read-only statement against the tenant views spans, logs,
// and metrics. limit <= 0 uses 200. limit above 1000 is rejected.
func (c *Core) SQLQuery(ctx context.Context, tenant, statement string, limit int) ([]map[string]any, []string, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}
	if limit <= 0 {
		limit = sqlQueryDefaultLimit
	}
	if limit > sqlQueryMaxLimit {
		return nil, nil, invalidInput("limit %d is above the maximum of 1000", limit)
	}
	statement = stripOneSemicolon(statement)
	if statement == "" {
		return nil, nil, sqlRejected(sqlRejectShape)
	}

	ctx, cancel := context.WithTimeout(ctx, sqlQueryTimeout)
	defer cancel()

	raw, err := c.engine.SerializeSQL(ctx, statement)
	if err != nil {
		return nil, nil, sqlFailed(err)
	}
	if err := allowSQL(raw); err != nil {
		return nil, nil, err
	}
	rows, cols, err := c.engine.QueryTenant(ctx, tenant, statement, limit)
	if err != nil {
		return nil, nil, sqlFailed(err)
	}
	return rows, cols, nil
}

func stripOneSemicolon(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, ";") {
		s = strings.TrimSpace(strings.TrimSuffix(s, ";"))
	}
	return s
}

func sqlRejected(reason string) error {
	return invalidInput("%s %s", reason, sqlQueryHint)
}

func sqlFailed(err error) error {
	return fmt.Errorf("%s %s", err.Error(), sqlQueryHint)
}

// SQLQueryDescription is the sql_query tool text. Column names come from
// Describe so a new column shows up here.
func (c *Core) SQLQueryDescription() string {
	cols := map[string][]string{}
	if c != nil && c.engine != nil {
		ok := true
		for _, view := range []string{"traces", "logs", "metrics"} {
			info, err := c.engine.Describe(view)
			if err != nil {
				ok = false
				break
			}
			names := make([]string, len(info))
			for i, col := range info {
				names[i] = col.Name
			}
			cols[view] = names
		}
		if !ok {
			cols = nil
		}
	} else {
		cols = nil
	}
	return sqlQueryDescription(cols)
}

func sqlQueryDescription(cols map[string][]string) string {
	var b strings.Builder
	b.WriteString("Run one read-only SELECT or WITH for one tenant. ")
	b.WriteString("The only views are spans, logs, and metrics. ")
	b.WriteString("traces is not a view in this tool. ")
	b.WriteString("spans has the traces columns. ")
	b.WriteString("Timestamps are Unix microseconds.")
	if cols == nil {
		return b.String()
	}
	b.WriteString(" spans columns: ")
	b.WriteString(strings.Join(cols["traces"], ", "))
	b.WriteString(". logs columns: ")
	b.WriteString(strings.Join(cols["logs"], ", "))
	b.WriteString(". metrics columns: ")
	b.WriteString(strings.Join(cols["metrics"], ", "))
	b.WriteByte('.')
	return b.String()
}

func allowSQL(doc string) error {
	var root map[string]any
	if err := json.Unmarshal([]byte(doc), &root); err != nil {
		return sqlRejected(sqlRejectShape)
	}
	switch v := root["error"].(type) {
	case bool:
		if v {
			return sqlRejected(sqlRejectShape)
		}
	default:
		if _, ok := root["error"]; ok {
			return sqlRejected(sqlRejectShape)
		}
	}
	stmts, ok := root["statements"].([]any)
	if !ok || len(stmts) != 1 {
		return sqlRejected(sqlRejectShape)
	}
	stmt, ok := stmts[0].(map[string]any)
	if !ok {
		return sqlRejected(sqlRejectShape)
	}
	node, ok := stmt["node"].(map[string]any)
	if !ok {
		return sqlRejected(sqlRejectShape)
	}
	switch node["type"] {
	case "SELECT_NODE", "SET_OPERATION_NODE":
	default:
		return sqlRejected(sqlRejectShape)
	}
	return walkSQL(node, map[string]struct{}{}, false)
}

func walkSQL(v any, ctes map[string]struct{}, relation bool) error {
	switch n := v.(type) {
	case []any:
		for _, child := range n {
			if err := walkSQL(child, ctes, false); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		ctes = scopeWithCTEs(ctes, n)
		if name, ok := n["function_name"].(string); ok && forbiddenFunction(name) {
			return sqlRejected(sqlRejectVisible)
		}
		if relation {
			if err := checkRelation(n, ctes); err != nil {
				return err
			}
		}
		typ, _ := n["type"].(string)
		if typ == "SELECT_NODE" {
			ft, ok := n["from_table"]
			if !ok || ft == nil {
				return sqlRejected(sqlRejectVisible)
			}
		}
		for key, child := range n {
			childRelation := key == "from_table" || (typ == "JOIN" && (key == "left" || key == "right"))
			if err := walkSQL(child, ctes, childRelation); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

func scopeWithCTEs(parent map[string]struct{}, obj map[string]any) map[string]struct{} {
	cm, ok := obj["cte_map"].(map[string]any)
	if !ok {
		return parent
	}
	list, _ := cm["map"].([]any)
	if len(list) == 0 {
		return parent
	}
	next := make(map[string]struct{}, len(parent)+len(list))
	for k := range parent {
		next[k] = struct{}{}
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, _ := m["key"].(string)
		if key != "" {
			next[strings.ToLower(key)] = struct{}{}
		}
	}
	return next
}

func checkRelation(n map[string]any, ctes map[string]struct{}) error {
	typ, _ := n["type"].(string)
	switch typ {
	case "EMPTY", "SUBQUERY", "EXPRESSION_LIST", "SET_OPERATION_NODE":
		return nil
	case "JOIN":
		if n["left"] == nil || n["right"] == nil {
			return sqlRejected(sqlRejectVisible)
		}
		return nil
	case "BASE_TABLE":
		return checkBaseTable(n, ctes)
	case "TABLE_FUNCTION":
		return checkTableFunction(n)
	default:
		return sqlRejected(sqlRejectVisible)
	}
}

func checkBaseTable(n map[string]any, ctes map[string]struct{}) error {
	catalog, okC := n["catalog_name"].(string)
	schema, okS := n["schema_name"].(string)
	table, okT := n["table_name"].(string)
	if !okC || !okS || !okT || catalog != "" || schema != "" {
		return sqlRejected(sqlRejectVisible)
	}
	name := strings.ToLower(table)
	switch name {
	case "spans", "logs", "metrics":
		return nil
	}
	if _, ok := ctes[name]; ok {
		return nil
	}
	return sqlRejected(sqlRejectVisible)
}

func checkTableFunction(n map[string]any) error {
	fn, ok := n["function"].(map[string]any)
	if !ok {
		return sqlRejected(sqlRejectVisible)
	}
	name, _ := fn["function_name"].(string)
	switch strings.ToLower(name) {
	case "range", "generate_series", "unnest":
		return nil
	default:
		return sqlRejected(sqlRejectVisible)
	}
}

func forbiddenFunction(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "read_blob", "read_text", "glob":
		return true
	}
	return strings.HasPrefix(n, "read_") || strings.Contains(n, "_scan")
}
