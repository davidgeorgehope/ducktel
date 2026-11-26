package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"unsafe"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// sessionConn reads the session's unexported connection.
// The SDK only sends notifications whose method is in its built-in table,
// so events/stream writes a JSON-RPC notification on that connection.
// The handler context is required: Streamable HTTP routes a write made
// during a request onto that request's SSE stream.
func sessionConn(ss *sdkmcp.ServerSession) sdkmcp.Connection {
	if ss == nil {
		return nil
	}
	field := reflect.ValueOf(ss).Elem().FieldByName("mcpConn")
	if !field.IsValid() {
		return nil
	}
	conn, _ := export(field).(sdkmcp.Connection)
	return conn
}

func writeNote(ctx context.Context, ss *sdkmcp.ServerSession, method string, params any) error {
	conn := sessionConn(ss)
	if conn == nil {
		return internal("mcp session has no connection")
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return conn.Write(ctx, &jsonrpc.Request{Method: method, Params: raw})
}

// subscriptionID is the JSON-RPC id of the events/stream request.
// The SDK stores it in the handler context under an unexported key.
func subscriptionID(ctx context.Context) (any, bool) {
	seen := map[context.Context]struct{}{}
	for i := 0; ctx != nil && i < 64; i++ {
		if _, ok := seen[ctx]; ok {
			return nil, false
		}
		seen[ctx] = struct{}{}
		v := reflect.ValueOf(ctx)
		if !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
			return nil, false
		}
		if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return nil, false
		}
		if keyF := v.FieldByName("key"); keyF.IsValid() {
			if valF := v.FieldByName("val"); valF.IsValid() {
				key := export(keyF)
				if key != nil {
					kt := reflect.TypeOf(key)
					if kt.Name() == "idContextKey" && strings.Contains(kt.PkgPath(), "modelcontextprotocol/go-sdk/mcp") {
						if id, ok := export(valF).(jsonrpc.ID); ok && id.IsValid() {
							return id.Raw(), true
						}
					}
				}
			}
		}
		parentF := v.FieldByName("Context")
		if !parentF.IsValid() {
			return nil, false
		}
		parent, _ := export(parentF).(context.Context)
		ctx = parent
	}
	return nil, false
}

func export(field reflect.Value) any {
	if !field.IsValid() || !field.CanAddr() {
		return nil
	}
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface()
}
