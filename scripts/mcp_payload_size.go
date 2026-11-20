// Command mcp_payload_size prints the byte size of MCP tool results against
// a ducktel data directory. It is the before/after check for compact results.
//
//	go run ./scripts/mcp_payload_size.go /path/to/data
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/mcp"
	"github.com/davidgeorgehope/ducktel/internal/query"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mcp_payload_size <data-dir>")
		os.Exit(2)
	}
	engine, err := query.Open(os.Args[1])
	if err != nil {
		fatal(err)
	}
	defer engine.Close()

	rows, _, err := engine.Query(`SELECT trace_id, count(*) AS n FROM traces
		WHERE json_extract_string(resource_attributes, '$."tenant.id"') = 'acme'
		GROUP BY trace_id ORDER BY n DESC, trace_id ASC LIMIT 1`)
	if err != nil {
		fatal(err)
	}
	if len(rows) == 0 {
		fatal(fmt.Errorf("no acme traces in %s", os.Args[1]))
	}
	traceID, _ := rows[0]["trace_id"].(string)
	fmt.Fprintf(os.Stderr, "trace_id=%s spans=%v\n", traceID, rows[0]["n"])

	session := connect(engine, os.Args[1])
	defer session.Close()

	calls := []struct {
		name string
		args map[string]any
	}{
		{"span_search", map[string]any{"tenant": "acme", "since_minutes": 180, "limit": 100}},
		{"span_search", map[string]any{"tenant": "acme", "since_minutes": 180, "limit": 100, "include_attributes": true}},
		{"trace_lookup", map[string]any{"tenant": "acme", "trace_id": traceID, "detail": "spans"}},
		{"trace_lookup", map[string]any{"tenant": "acme", "trace_id": traceID, "detail": "spans", "include_attributes": false}},
		{"trace_lookup", map[string]any{"tenant": "acme", "trace_id": traceID, "detail": "summary"}},
		{"slow_requests", map[string]any{"tenant": "acme", "since_minutes": 180, "limit": 5}},
		{"log_search", map[string]any{"tenant": "acme", "since_minutes": 180, "limit": 100}},
		{"metric_query", map[string]any{"tenant": "acme", "metric_name": "http.server.request.count", "aggregation": "sum", "since_minutes": 180}},
	}

	fmt.Printf("tool\tbytes\trows\twhitespace\tattribute_values\terrors\n")
	for _, c := range calls {
		body, isErr := call(session, c.name, c.args)
		label := c.name
		if d, ok := c.args["detail"].(string); ok {
			label = c.name + " detail=" + d
		}
		if inc, ok := c.args["include_attributes"].(bool); ok {
			label += fmt.Sprintf(" include_attributes=%t", inc)
		}
		rowsN := rowCount(body)
		ws := jsonWhitespace(body)
		attr := attributeValueBytes(body)
		errN := 0
		if isErr {
			errN = 1
		}
		fmt.Printf("%s\t%d\t%d\t%d\t%d\t%d\n", label, len(body), rowsN, ws, attr, errN)
		if os.Getenv("MCP_PAYLOAD_DIR") != "" {
			name := strings.NewReplacer(" ", "_", "=", "-").Replace(label) + ".json"
			if err := os.WriteFile(filepath.Join(os.Getenv("MCP_PAYLOAD_DIR"), name), []byte(body), 0o644); err != nil {
				fatal(err)
			}
		}
	}
}

func connect(engine *query.Engine, dataDir string) *sdkmcp.ClientSession {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "measure", Version: "0"}, nil)
	mcp.NewServer(engine, dataDir, "", "").Register(srv)
	clientT, serverT := sdkmcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		fatal(err)
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "measure-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		fatal(err)
	}
	return session
}

func call(s *sdkmcp.ClientSession, name string, args map[string]any) (string, bool) {
	res, err := s.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		fatal(err)
	}
	var b []byte
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			b = append(b, tc.Text...)
		}
	}
	return string(b), res.IsError
}

func rowCount(body string) int {
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		return -1
	}
	return len(rows)
}

func jsonWhitespace(s string) int {
	n := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if inStr {
			if esc {
				esc = false
				continue
			}
			if r == '\\' {
				esc = true
				continue
			}
			if r == '"' {
				inStr = false
			}
			continue
		}
		if r == '"' {
			inStr = true
			continue
		}
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			n += size
		}
	}
	return n
}

func attributeValueBytes(body string) int {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		return 0
	}
	n := 0
	for _, row := range rows {
		raw, ok := row["attributes"]
		if !ok {
			continue
		}
		n += len(raw)
	}
	return n
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
