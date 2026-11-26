package agenteval

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/feedback"
	"github.com/davidgeorgehope/ducktel/internal/mcp"
	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/receiver"
	"github.com/davidgeorgehope/ducktel/internal/testharness"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// evalSeed and evalN match the locked inputs in testharness/batch_test.go.
const (
	evalSeed int64 = 1
	evalN          = 8
)

func TestTasks(t *testing.T) {
	batch, engine, dir := seed(t)
	o := Derive(batch)
	if err := checkOracle(o); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Logf("most-error %v", o.MostErrorServices)
	t.Logf("slowest %v", o.SlowestRoots)
	t.Logf("payment %v", o.FailingPayment)
	t.Logf("metrics %v", o.MetricNames)
	t.Logf("warns %d", o.WarnCount)

	session := connect(t, engine, dir, "", "")
	measured := runTasks(t, session, o)
	for _, r := range measured {
		t.Logf("payload\t%s\t%d\t%d", r.Label, r.Bytes, r.Tokens)
	}
	checkBaseline(t, measured)
}

func TestFlushBuffer(t *testing.T) {
	var method, path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"flushed"}`))
	}))
	t.Cleanup(srv.Close)

	const token = "flush-secret"
	engine, dir := openEmpty(t)
	session := connect(t, engine, dir, srv.URL+"/flush", token)
	res := call(t, session, FlushCall, nil)
	if res.IsError {
		t.Fatalf("flush_buffer error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "flushed") {
		t.Fatalf("flush_buffer text %q does not say flushed", res.Text)
	}
	if method != http.MethodPost {
		t.Errorf("method %s, want POST", method)
	}
	if path != "/flush" {
		t.Errorf("path %s, want /flush", path)
	}
	if auth != "Bearer "+token {
		t.Errorf("Authorization %q, want Bearer token", auth)
	}
	t.Logf("payload\t%s\t%d\t%d", res.Label, res.Bytes, res.Tokens)
	checkBaseline(t, []Result{res})
}

func TestToolCoverage(t *testing.T) {
	called := map[string]bool{FlushCall.Tool: true}
	for _, task := range Tasks() {
		for _, c := range task.Calls {
			called[c.Tool] = true
		}
	}

	engine, dir := openEmpty(t)
	plain := connect(t, engine, dir, "", "")
	flushEngine, flushDir := openEmpty(t)
	withFlush := connect(t, flushEngine, flushDir, "http://127.0.0.1/flush", "tok")
	for _, session := range []*sdkmcp.ClientSession{plain, withFlush} {
		names := listToolNames(t, session)
		for _, name := range names {
			if !called[name] {
				t.Errorf("tool %s has no task call", name)
			}
		}
	}
}

func checkOracle(o Oracle) error {
	if len(o.SpanServices) == 0 {
		return fmt.Errorf("oracle service set is empty")
	}
	if len(o.FailingPayment) == 0 {
		return fmt.Errorf("oracle has no payment error")
	}
	if o.WarnCount == 0 {
		return fmt.Errorf("oracle has no WARN")
	}
	max := 0
	for _, n := range o.ErrorCount {
		if n > max {
			max = n
		}
	}
	if max == 0 {
		return fmt.Errorf("oracle error span max is 0")
	}
	return nil
}

func seed(t *testing.T) (testharness.Batch, *query.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Start()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	r, err := receiver.New("127.0.0.1", port, w, "", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = r.Start() }()

	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitHealth(t, addr)

	cfg := testharness.DefaultConfig()
	cfg.Endpoint = addr
	cfg.Tenant = tenantName
	cfg.ErrorRate = 0
	cfg.Scenarios = []testharness.Scenario{{
		Name:          "payment-service-error_rate",
		TargetService: paymentService,
		Type:          "error_rate",
		ErrorRate:     1,
	}}
	h := testharness.New(cfg)
	h.SetRand(rand.New(rand.NewSource(evalSeed)))
	batch, err := h.EmitBatch(evalN, time.Now())
	if err != nil {
		t.Fatalf("EmitBatch: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		engine.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.Stop(ctx)
		if err := w.Stop(); err != nil {
			t.Errorf("writer stop: %v", err)
		}
	})
	return batch, engine, dir
}

func openEmpty(t *testing.T) (*query.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine, dir
}

func connect(t *testing.T, engine *query.Engine, dataDir, flushURL, flushToken string) *sdkmcp.ClientSession {
	t.Helper()
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "ducktel-eval", Version: "eval"}, mcp.SDKOptions())
	ds := mcp.NewServer(engine, dataDir, flushURL, flushToken)
	fb, err := feedback.New(feedback.Options{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	ds.SetFeedback(fb)
	ds.Register(srv)
	clientT, serverT := sdkmcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "eval-client", Version: "eval"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func waitHealth(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		resp, err := http.Get(addr + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("receiver %s not healthy: %v", addr, last)
}

func runTasks(t *testing.T, session *sdkmcp.ClientSession, o Oracle) []Result {
	t.Helper()
	var measured []Result
	seen := map[string]bool{}
	for _, task := range Tasks() {
		var got []Result
		for _, c := range task.Calls {
			res := call(t, session, c, got)
			if seen[res.Label] {
				t.Fatalf("duplicate call label %s", res.Label)
			}
			seen[res.Label] = true
			got = append(got, res)
			measured = append(measured, res)
			if res.IsError {
				t.Fatalf("%s: %s returned an error: %s", task.Name, c.Tool, res.Text)
			}
		}
		if err := task.Grade(got, o); err != nil {
			t.Fatalf("%s: %v", task.Name, err)
		}
	}
	return measured
}

func call(t *testing.T, session *sdkmcp.ClientSession, c Call, prev []Result) Result {
	t.Helper()
	args := cloneArgs(c.Args)
	if c.Bind != nil {
		if err := c.Bind(prev, args); err != nil {
			t.Fatalf("%s: %v", c.Label, err)
		}
	}
	res, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      c.Tool,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool %s: %v", c.Tool, err)
	}
	var b strings.Builder
	for _, block := range res.Content {
		if text, ok := block.(*sdkmcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	text := b.String()
	return Result{
		Label:   c.Label,
		Tool:    c.Tool,
		Text:    text,
		IsError: res.IsError,
		Bytes:   len(text),
		Tokens:  Tokens(len(text)),
	}
}

func cloneArgs(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func listToolNames(t *testing.T, session *sdkmcp.ClientSession) []string {
	t.Helper()
	var names []string
	var cursor string
	for {
		res, err := session.ListTools(context.Background(), &sdkmcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		if res.NextCursor == "" {
			return names
		}
		cursor = res.NextCursor
	}
}

func checkBaseline(t *testing.T, measured []Result) {
	t.Helper()
	path := baselinePath()
	got := map[string]ceiling{}
	for _, r := range measured {
		got[r.Label] = ceiling{MaxBytes: r.Bytes, MaxTokens: r.Tokens}
	}
	if os.Getenv("UPDATE_BASELINE") == "1" {
		existing, err := readBaseline(path)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range got {
			existing[k] = v
		}
		if err := writeBaseline(path, existing); err != nil {
			t.Fatal(err)
		}
	}
	file, err := readBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	for label, m := range got {
		ceil, ok := file[label]
		if !ok {
			t.Errorf("baseline missing %s (%d bytes, %d tokens)", label, m.MaxBytes, m.MaxTokens)
			continue
		}
		if m.MaxBytes > ceil.MaxBytes || m.MaxTokens > ceil.MaxTokens {
			t.Errorf("%s is %d bytes / %d tokens, baseline max is %d / %d", label, m.MaxBytes, m.MaxTokens, ceil.MaxBytes, ceil.MaxTokens)
		}
	}
}
