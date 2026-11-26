package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/events"
	"github.com/davidgeorgehope/ducktel/internal/feedback"
)

const eventsExtension = "io.modelcontextprotocol/events"

// SDKOptions advertises the Events extension. Tools are still added by the
// SDK when Register adds them.
func SDKOptions() *sdkmcp.ServerOptions {
	caps := &sdkmcp.ServerCapabilities{}
	caps.AddExtension(eventsExtension, map[string]any{})
	return &sdkmcp.ServerOptions{Capabilities: caps}
}

func (s *Server) registerEvents(srv *sdkmcp.Server) {
	mustRegister(sdkmcp.AddReceivingCustomMethod(srv, "events/list", s.eventsList))
	mustRegister(sdkmcp.AddReceivingCustomMethod(srv, "events/poll", s.eventsPoll))
	mustRegister(sdkmcp.AddReceivingCustomMethod(srv, "events/stream", s.eventsStream))
	mustRegister(sdkmcp.AddReceivingCustomMethod(srv, "events/subscribe", s.eventsWebhook))
	mustRegister(sdkmcp.AddReceivingCustomMethod(srv, "events/unsubscribe", s.eventsWebhook))
}

func mustRegister(err error) {
	if err != nil {
		panic(err)
	}
}

type listParams struct {
	sdkmcp.ParamsBase
	Cursor string `json:"cursor,omitempty"`
}

type eventDesc struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Delivery      []string       `json:"delivery"`
	InputSchema   map[string]any `json:"inputSchema"`
	PayloadSchema map[string]any `json:"payloadSchema"`
}

type listResult struct {
	sdkmcp.ResultBase
	ResultType string      `json:"resultType,omitempty"`
	Events     []eventDesc `json:"events"`
}

type pollParams struct {
	sdkmcp.ParamsBase
	Name        string          `json:"name"`
	Arguments   map[string]any  `json:"arguments"`
	Cursor      json.RawMessage `json:"cursor"`
	MaxAgeMs    *int64          `json:"maxAgeMs,omitempty"`
	MaxEvents   *int            `json:"maxEvents,omitempty"`
	HeartbeatMs *int64          `json:"heartbeatMs,omitempty"`
}

type wireEvent struct {
	EventID   string         `json:"eventId"`
	Name      string         `json:"name"`
	Timestamp string         `json:"timestamp"`
	Data      map[string]any `json:"data"`
	Cursor    string         `json:"cursor,omitempty"`
}

type pollResult struct {
	sdkmcp.ResultBase
	ResultType string      `json:"resultType,omitempty"`
	Events     []wireEvent `json:"events"`
	Cursor     string      `json:"cursor"`
	Truncated  bool        `json:"truncated"`
	HasMore    bool        `json:"hasMore"`
	NextPollMs int         `json:"nextPollMs"`
}

type streamResult struct {
	sdkmcp.ResultBase
	ResultType string `json:"resultType,omitempty"`
}

type noteActive struct {
	Cursor      string         `json:"cursor"`
	Truncated   bool           `json:"truncated"`
	HeartbeatMs int64          `json:"heartbeatMs"`
	Meta        map[string]any `json:"_meta"`
}

type noteEvent struct {
	EventID   string         `json:"eventId"`
	Name      string         `json:"name"`
	Timestamp string         `json:"timestamp"`
	Data      map[string]any `json:"data"`
	Cursor    string         `json:"cursor"`
	Meta      map[string]any `json:"_meta"`
}

type noteHeart struct {
	Cursor string         `json:"cursor"`
	Meta   map[string]any `json:"_meta"`
}

func (s *Server) eventsList(context.Context, *sdkmcp.ServerSession, *listParams) (*listResult, error) {
	return &listResult{ResultType: "complete", Events: eventCatalog()}, nil
}

func (s *Server) eventsWebhook(context.Context, *sdkmcp.ServerSession, *pollParams) (*streamResult, error) {
	raw, _ := json.Marshal(map[string]string{"feature": "deliveryMode", "value": "webhook"})
	return nil, &jsonrpc.Error{Code: -32026, Message: "Unsupported", Data: raw}
}

func (s *Server) eventsPoll(_ context.Context, _ *sdkmcp.ServerSession, params *pollParams) (*pollResult, error) {
	if params == nil {
		return nil, invalid("missing params")
	}
	f, err := parseFilter(params.Name, params.Arguments)
	if err != nil {
		return nil, err
	}
	maxEvents, err := boundMaxEvents(params.MaxEvents)
	if err != nil {
		return nil, err
	}
	nullCursor, after, err := decodeCursor(params.Cursor)
	if err != nil {
		return nil, err
	}
	s.scanSpans()
	var age time.Duration
	if nullCursor {
		after = s.events.Head(f.Tenant)
	} else {
		age = maxAge(params.MaxAgeMs)
	}
	page := s.events.Read(f, after, maxEvents, time.Now(), age)
	out := make([]wireEvent, len(page.Events))
	for i, ev := range page.Events {
		out[i] = wireEvent{
			EventID:   ev.EventID,
			Name:      string(ev.Name),
			Timestamp: ev.Timestamp.UTC().Format(time.RFC3339),
			Data:      ev.Data,
		}
	}
	return &pollResult{
		ResultType: "complete",
		Events:     out,
		Cursor:     events.FormatCursor(page.Cursor),
		Truncated:  page.Truncated && !nullCursor,
		HasMore:    page.HasMore,
		NextPollMs: events.NextPollMs,
	}, nil
}

func (s *Server) eventsStream(ctx context.Context, ss *sdkmcp.ServerSession, params *pollParams) (*streamResult, error) {
	if params == nil {
		return nil, invalid("missing params")
	}
	f, err := parseFilter(params.Name, params.Arguments)
	if err != nil {
		return nil, err
	}
	nullCursor, pos, err := decodeCursor(params.Cursor)
	if err != nil {
		return nil, err
	}
	if err := s.events.Acquire(f.Tenant); err != nil {
		var lim *events.LimitError
		if errors.As(err, &lim) {
			raw, _ := json.Marshal(map[string]any{"limit": "subscriptions", "max": lim.Max})
			return nil, &jsonrpc.Error{Code: -32025, Message: "ResourceExhausted", Data: raw}
		}
		return nil, err
	}
	defer s.events.Release(f.Tenant)

	subID, ok := subscriptionID(ctx)
	if !ok {
		return nil, internal("events/stream has no request id")
	}
	meta := map[string]any{sdkmcp.MetaKeySubscriptionID: subID}
	hb := grantHeartbeat(params.HeartbeatMs)

	s.scanSpans()
	truncated := false
	if nullCursor {
		pos = s.events.Head(f.Tenant)
	} else {
		page := s.events.Read(f, pos, 1, time.Now(), maxAge(params.MaxAgeMs))
		truncated = page.Truncated
		if truncated && len(page.Events) > 0 {
			pos = page.Events[0].Seq - 1
		} else if truncated {
			pos = page.Cursor
		}
	}
	if err := writeNote(ctx, ss, "notifications/events/active", noteActive{
		Cursor:      events.FormatCursor(pos),
		Truncated:   truncated,
		HeartbeatMs: hb,
		Meta:        meta,
	}); err != nil {
		return nil, err
	}

	scanTick := time.NewTicker(time.Second)
	defer scanTick.Stop()
	heart := time.NewTicker(time.Duration(hb) * time.Millisecond)
	defer heart.Stop()

	for {
		page := s.events.Read(f, pos, events.PollDefault, time.Now(), 0)
		if page.Truncated {
			gap := pos
			if len(page.Events) > 0 {
				gap = page.Events[0].Seq - 1
			} else {
				gap = page.Cursor
			}
			if gap != pos {
				pos = gap
				if err := writeNote(ctx, ss, "notifications/events/active", noteActive{
					Cursor:      events.FormatCursor(pos),
					Truncated:   true,
					HeartbeatMs: hb,
					Meta:        meta,
				}); err != nil {
					return nil, err
				}
			}
		}
		for _, ev := range page.Events {
			if err := writeNote(ctx, ss, "notifications/events/event", noteEvent{
				EventID:   ev.EventID,
				Name:      string(ev.Name),
				Timestamp: ev.Timestamp.UTC().Format(time.RFC3339),
				Data:      ev.Data,
				Cursor:    events.FormatCursor(ev.Seq),
				Meta:      meta,
			}); err != nil {
				return nil, err
			}
			pos = ev.Seq
			heart.Reset(time.Duration(hb) * time.Millisecond)
		}
		if page.HasMore {
			continue
		}
		select {
		case <-ctx.Done():
			res := &streamResult{ResultType: "complete"}
			res.SetMeta(meta)
			return res, nil
		case <-scanTick.C:
			s.scanSpans()
		case <-heart.C:
			if err := writeNote(ctx, ss, "notifications/events/heartbeat", noteHeart{
				Cursor: events.FormatCursor(pos),
				Meta:   meta,
			}); err != nil {
				return nil, err
			}
		case <-s.events.Fired():
		}
	}
}

// scanSpans files error spans that landed after the previous scan into the
// tenant ring named on each row. The first call only records the current
// high-water mark, so spans already stored are not events.
func (s *Server) scanSpans() {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.events == nil || s.core == nil {
		return
	}
	if !s.spanPrimed {
		start, id, ok, err := s.core.LatestErrorSpan()
		if err != nil {
			return
		}
		s.spanPrimed = true
		if ok {
			s.spanStart = start
			s.spanID = id
		}
		return
	}
	rows, err := s.core.ErrorSpansAfter(s.spanStart, s.spanID, 200)
	if err != nil {
		return
	}
	for _, row := range rows {
		at := time.UnixMicro(row.StartUS).UTC()
		s.events.PublishSpan(row.Tenant, row.TraceID, row.SpanID, row.Service, row.SpanName, row.StatusMessage, at)
		s.spanStart = row.StartUS
		s.spanID = row.SpanID
	}
}

func parseFilter(name string, args map[string]any) (events.Filter, error) {
	if !events.Known(events.Name(name)) {
		raw, _ := json.Marshal(map[string]string{"kind": "event"})
		return events.Filter{}, &jsonrpc.Error{Code: -32023, Message: "NotFound", Data: raw}
	}
	tenant, _ := stringArg(args, "tenant")
	if strings.TrimSpace(tenant) == "" {
		return events.Filter{}, invalid("tenant is required")
	}
	service, _ := stringArg(args, "service")
	severity, _ := stringArg(args, "severity")
	return events.Filter{
		Name:     events.Name(name),
		Tenant:   tenant,
		Service:  service,
		Severity: severity,
	}, nil
}

func stringArg(args map[string]any, key string) (string, bool) {
	if args == nil {
		return "", false
	}
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return s, true
}

func decodeCursor(raw json.RawMessage) (null bool, seq uint64, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return true, 0, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false, 0, invalid("cursor must be a string or null")
	}
	n, err := events.ParseCursor(s)
	if err != nil {
		return false, 0, invalid("cursor must be a decimal sequence")
	}
	return false, n, nil
}

func boundMaxEvents(n *int) (int, error) {
	if n == nil || *n == 0 {
		return events.PollDefault, nil
	}
	if *n < 0 || *n > events.PollMax {
		return 0, invalid("maxEvents must be from 1 to 100")
	}
	return *n, nil
}

func maxAge(ms *int64) time.Duration {
	if ms == nil || *ms <= 0 {
		return 0
	}
	return time.Duration(*ms) * time.Millisecond
}

func grantHeartbeat(ms *int64) int64 {
	if ms == nil || *ms == 0 {
		return events.HeartbeatDefault
	}
	if *ms < events.HeartbeatMin {
		return events.HeartbeatMin
	}
	if *ms > events.HeartbeatMax {
		return events.HeartbeatMax
	}
	return *ms
}

func invalid(msg string) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: msg}
}

func internal(msg string) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: msg}
}

func eventCatalog() []eventDesc {
	delivery := []string{"poll", "push"}
	tenant := map[string]any{
		"type":        "string",
		"description": "Tenant id. Only events for this tenant are delivered.",
	}
	service := map[string]any{
		"type":        "string",
		"description": "Exact service name. Omit it to receive every service.",
	}
	return []eventDesc{
		{
			Name:        string(events.SpanError),
			Description: "An error span flushed to this data directory after the subscriber's cursor. The payload is the trace id, span id, service, span name, status, and status message.",
			Delivery:    delivery,
			InputSchema: objectSchema(map[string]any{
				"tenant":   tenant,
				"service":  service,
				"severity": map[string]any{"type": "string", "description": "error, ERROR, or STATUS_CODE_ERROR. Any other value matches nothing."},
			}, "tenant"),
			PayloadSchema: objectSchema(map[string]any{
				"trace_id":  map[string]any{"type": "string"},
				"span_id":   map[string]any{"type": "string"},
				"service":   map[string]any{"type": "string"},
				"span_name": map[string]any{"type": "string"},
				"status":    map[string]any{"type": "string"},
				"summary":   map[string]any{"type": "string"},
			}, "trace_id", "span_id", "service", "status"),
		},
		{
			Name:        string(events.Feedback),
			Description: "A new submit_feedback report accepted by this MCP process. The payload is the report id, service, tool name, severity, and summary.",
			Delivery:    delivery,
			InputSchema: objectSchema(map[string]any{
				"tenant":   tenant,
				"service":  map[string]any{"type": "string", "description": "Exact service. Feedback is stored as ducktel.agent_feedback."},
				"severity": map[string]any{"type": "string", "description": "low, medium, or high."},
			}, "tenant"),
			PayloadSchema: objectSchema(map[string]any{
				"report_id": map[string]any{"type": "string"},
				"service":   map[string]any{"type": "string"},
				"tool_name": map[string]any{"type": "string"},
				"severity":  map[string]any{"type": "string"},
				"summary":   map[string]any{"type": "string"},
			}, "report_id", "service", "severity", "summary"),
		},
	}
}

func objectSchema(props map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// publishFeedback is the OnStored hook. A retry of the same report does not call it.
func (s *Server) publishFeedback(r feedback.Report) {
	if s.events == nil {
		return
	}
	s.events.PublishFeedback(r.Tenant, r.ReportID, r.ToolName, string(r.Severity), r.Summary, time.Now())
}
