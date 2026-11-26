package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/events"
	"github.com/davidgeorgehope/ducktel/internal/feedback"
	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func TestEventsPollFeedbackAndSpans(t *testing.T) {
	session, dir, _ := newEventSession(t)

	init := session.InitializeResult()
	if init == nil || init.Capabilities == nil || init.Capabilities.Extensions[eventsExtension] == nil {
		t.Fatalf("capabilities = %#v", init)
	}

	list, err := sdkmcp.CallCustomMethod[*listParams, *listResult](context.Background(), session, "events/list", &listParams{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]eventDesc{}
	for _, ev := range list.Events {
		got[ev.Name] = ev
	}
	for _, name := range []string{"span.error", "feedback.submitted"} {
		ev, ok := got[name]
		if !ok {
			t.Fatalf("missing %s in %#v", name, list.Events)
		}
		if strings.Join(ev.Delivery, ",") != "poll,push" {
			t.Fatalf("%s delivery = %v", name, ev.Delivery)
		}
	}

	first, err := poll(t, session, "feedback.submitted", "acme", "null")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 0 {
		t.Fatalf("null cursor returned %#v", first.Events)
	}

	body, isErr := call(t, session, "submit_feedback", feedbackArgs("fb-1", "acme"))
	if isErr {
		t.Fatalf("submit_feedback: %s", body)
	}
	second, err := poll(t, session, "feedback.submitted", "acme", jsonString(first.Cursor))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 {
		t.Fatalf("events = %#v", second.Events)
	}
	ev := second.Events[0]
	if ev.Data["report_id"] != "fb-1" || ev.Data["summary"] != "rows hid the error" {
		t.Fatalf("data = %#v", ev.Data)
	}
	if _, ok := ev.Data["details"]; ok {
		t.Fatal("payload includes details")
	}
	if _, ok := ev.Data["proposed_fix"]; ok {
		t.Fatal("payload includes proposed_fix")
	}
	raw, err := json.Marshal(ev.Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > events.MaxDataBytes {
		t.Fatalf("payload is %d bytes", len(raw))
	}

	other, err := poll(t, session, "feedback.submitted", "globex", "null")
	if err != nil {
		t.Fatal(err)
	}
	again, err := poll(t, session, "feedback.submitted", "globex", jsonString(other.Cursor))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range again.Events {
		if row.Data["report_id"] == "fb-1" {
			t.Fatal("globex received acme feedback")
		}
	}

	body, isErr = call(t, session, "submit_feedback", feedbackArgs("fb-1", "acme"))
	if isErr {
		t.Fatalf("retry: %s", body)
	}
	third, err := poll(t, session, "feedback.submitted", "acme", jsonString(second.Cursor))
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Events) != 0 {
		t.Fatalf("retry emitted %#v", third.Events)
	}

	_, err = poll(t, session, "no.such", "acme", "null")
	var rpc *jsonrpc.Error
	if !errors.As(err, &rpc) || rpc.Code != -32023 {
		t.Fatalf("unknown name err = %v", err)
	}
	_, err = poll(t, session, "span.error", "", "null")
	if !errors.As(err, &rpc) || rpc.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("blank tenant err = %v", err)
	}
	tooMany := 101
	_, err = sdkmcp.CallCustomMethod[*pollParams, *pollResult](context.Background(), session, "events/poll", &pollParams{
		Name:      "span.error",
		Arguments: map[string]any{"tenant": "acme"},
		Cursor:    json.RawMessage("null"),
		MaxEvents: &tooMany,
	})
	if !errors.As(err, &rpc) || rpc.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("maxEvents err = %v", err)
	}
	_, err = sdkmcp.CallCustomMethod[*pollParams, *streamResult](context.Background(), session, "events/subscribe", &pollParams{
		Name:      "span.error",
		Arguments: map[string]any{"tenant": "acme"},
		Cursor:    json.RawMessage("null"),
	})
	if !errors.As(err, &rpc) || rpc.Code != -32026 {
		t.Fatalf("webhook err = %v", err)
	}

	spanCursor, err := poll(t, session, "span.error", "acme", "null")
	if err != nil {
		t.Fatal(err)
	}
	globexCursor, err := poll(t, session, "span.error", "globex", "null")
	if err != nil {
		t.Fatal(err)
	}
	writeSpans(t, dir)
	acme, err := poll(t, session, "span.error", "acme", jsonString(spanCursor.Cursor))
	if err != nil {
		t.Fatal(err)
	}
	if len(acme.Events) != 1 || acme.Events[0].Data["trace_id"] != "trace-acme" || acme.Events[0].Data["span_id"] != "span-acme" {
		t.Fatalf("acme spans = %#v", acme.Events)
	}
	for _, row := range acme.Events {
		if row.Data["trace_id"] == "trace-globex" {
			t.Fatal("acme received globex span")
		}
	}
	gx, err := poll(t, session, "span.error", "globex", jsonString(globexCursor.Cursor))
	if err != nil {
		t.Fatal(err)
	}
	if len(gx.Events) != 1 || gx.Events[0].Data["trace_id"] != "trace-globex" {
		t.Fatalf("globex spans = %#v", gx.Events)
	}
}

func TestEventsStreamDeliversFeedback(t *testing.T) {
	session, _, notes := newEventSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := sdkmcp.CallCustomMethod[*pollParams, *streamResult](ctx, session, "events/stream", &pollParams{
			Name:      "feedback.submitted",
			Arguments: map[string]any{"tenant": "acme"},
			Cursor:    json.RawMessage("null"),
		})
		done <- err
	}()

	active := waitNote(t, notes, "notifications/events/active")
	var activeBody struct {
		Meta map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(active.Params, &activeBody); err != nil {
		t.Fatal(err)
	}
	subID := activeBody.Meta[sdkmcp.MetaKeySubscriptionID]
	if subID == nil {
		t.Fatalf("active has no subscription id: %s", active.Params)
	}
	if id := notes.id(); id != nil && fmt.Sprint(id) != fmt.Sprint(subID) {
		t.Fatalf("subscription id %v, request id %v", subID, id)
	}

	body, isErr := call(t, session, "submit_feedback", feedbackArgs("fb-stream", "acme"))
	if isErr {
		t.Fatalf("submit_feedback: %s", body)
	}
	ev := waitNote(t, notes, "notifications/events/event")
	var eventBody struct {
		EventID string         `json:"eventId"`
		Data    map[string]any `json:"data"`
		Meta    map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(ev.Params, &eventBody); err != nil {
		t.Fatal(err)
	}
	if eventBody.Data["report_id"] != "fb-stream" {
		t.Fatalf("event = %s", ev.Params)
	}
	if fmt.Sprint(eventBody.Meta[sdkmcp.MetaKeySubscriptionID]) != fmt.Sprint(subID) {
		t.Fatalf("event subscription %v, active %v", eventBody.Meta[sdkmcp.MetaKeySubscriptionID], subID)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("events/stream did not return after cancel")
	}
}

func TestEventsStreamCap(t *testing.T) {
	session, _, notes := newEventSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, events.StreamsPerTenant)
	for i := 0; i < events.StreamsPerTenant; i++ {
		go func() {
			_, err := sdkmcp.CallCustomMethod[*pollParams, *streamResult](ctx, session, "events/stream", &pollParams{
				Name:      "feedback.submitted",
				Arguments: map[string]any{"tenant": "acme"},
				Cursor:    json.RawMessage("null"),
			})
			errc <- err
		}()
	}
	for i := 0; i < events.StreamsPerTenant; i++ {
		waitNote(t, notes, "notifications/events/active")
	}
	_, err := sdkmcp.CallCustomMethod[*pollParams, *streamResult](context.Background(), session, "events/stream", &pollParams{
		Name:      "feedback.submitted",
		Arguments: map[string]any{"tenant": "acme"},
		Cursor:    json.RawMessage("null"),
	})
	var rpc *jsonrpc.Error
	if !errors.As(err, &rpc) || rpc.Code != -32025 {
		t.Fatalf("17th stream err = %v", err)
	}
	cancel()
	deadline := time.After(5 * time.Second)
	for i := 0; i < events.StreamsPerTenant; i++ {
		select {
		case <-errc:
		case <-deadline:
			t.Fatal("stream did not return after cancel")
		}
	}
}

func poll(t *testing.T, session *sdkmcp.ClientSession, name, tenant, cursor string) (*pollResult, error) {
	t.Helper()
	return sdkmcp.CallCustomMethod[*pollParams, *pollResult](context.Background(), session, "events/poll", &pollParams{
		Name:      name,
		Arguments: map[string]any{"tenant": tenant},
		Cursor:    json.RawMessage(cursor),
	})
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func newEventSession(t *testing.T) (*sdkmcp.ClientSession, string, *noteTee) {
	t.Helper()
	dir := t.TempDir()
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "ducktel-test", Version: "test"}, SDKOptions())
	ds := NewServer(engine, dir, "", "")
	svc, err := feedback.New(feedback.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ds.SetFeedback(svc)
	ds.Register(srv)

	clientT, serverT := sdkmcp.NewInMemoryTransports()
	notes := &noteTee{notes: make(chan *jsonrpc.Request, 256)}
	ctx := context.Background()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil)
	for _, method := range []string{"events/list", "events/poll", "events/stream", "events/subscribe"} {
		if err := registerSend(client, method); err != nil {
			t.Fatal(err)
		}
	}
	session, err := client.Connect(ctx, &teeTransport{inner: clientT, tee: notes}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, dir, notes
}

func registerSend(client *sdkmcp.Client, method string) error {
	switch method {
	case "events/list":
		return sdkmcp.AddSendingCustomMethod[*listParams, *listResult](client, method)
	case "events/poll":
		return sdkmcp.AddSendingCustomMethod[*pollParams, *pollResult](client, method)
	case "events/stream", "events/subscribe":
		return sdkmcp.AddSendingCustomMethod[*pollParams, *streamResult](client, method)
	default:
		return fmt.Errorf("unknown method %s", method)
	}
}

func writeSpans(t *testing.T, dir string) {
	t.Helper()
	w := writer.New(dir, time.Hour, 100)
	now := time.Now()
	mk := func(tenant, trace, span, status string) writer.TraceSpan {
		return writer.TraceSpan{
			TraceID: trace, SpanID: span, ServiceName: "api", SpanName: "charge",
			SpanKind: "SPAN_KIND_SERVER", StartTime: now.UnixMicro(), EndTime: now.UnixMicro(),
			DurationMs: 5, StatusCode: status, StatusMessage: "card declined",
			Attributes: "{}", Events: "[]", Links: "[]",
			ResourceAttributes: `{"service.name":"api","tenant.id":"` + tenant + `"}`,
		}
	}
	w.Add([]writer.TraceSpan{
		mk("acme", "trace-acme", "span-acme", "STATUS_CODE_ERROR"),
		mk("globex", "trace-globex", "span-globex", "STATUS_CODE_ERROR"),
		mk("acme", "trace-ok", "span-ok", "STATUS_CODE_OK"),
	})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
}

type noteTee struct {
	mu       sync.Mutex
	notes    chan *jsonrpc.Request
	streamID any
}

func (n *noteTee) setStreamID(id any) {
	n.mu.Lock()
	n.streamID = id
	n.mu.Unlock()
}

func (n *noteTee) id() any {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.streamID
}

type teeTransport struct {
	inner sdkmcp.Transport
	tee   *noteTee
}

func (t *teeTransport) Connect(ctx context.Context) (sdkmcp.Connection, error) {
	c, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &teeConn{Connection: c, tee: t.tee}, nil
}

type teeConn struct {
	sdkmcp.Connection
	tee *noteTee
}

func (c *teeConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	m, err := c.Connection.Read(ctx)
	if err != nil {
		return m, err
	}
	if req, ok := m.(*jsonrpc.Request); ok && strings.HasPrefix(req.Method, "notifications/events/") {
		select {
		case c.tee.notes <- req:
		default:
		}
	}
	return m, nil
}

func (c *teeConn) Write(ctx context.Context, m jsonrpc.Message) error {
	if req, ok := m.(*jsonrpc.Request); ok && req.Method == "events/stream" && req.ID.IsValid() {
		c.tee.setStreamID(req.ID.Raw())
	}
	return c.Connection.Write(ctx, m)
}

func waitNote(t *testing.T, notes *noteTee, method string) *jsonrpc.Request {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case req := <-notes.notes:
			if req.Method == method {
				return req
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", method)
			return nil
		}
	}
}
