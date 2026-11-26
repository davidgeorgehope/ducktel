// Package events is the in-memory log of tenant-scoped MCP events.
// One hub owns every ring. Subscribers only read.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// RingCap is how many occurrences one tenant keeps. Older ones are dropped.
	RingCap = 64
	// MaxTenants is how many tenant rings the process keeps.
	MaxTenants = 128
	// StreamsPerTenant is the live events/stream cap for one tenant.
	StreamsPerTenant = 16
	// MaxStreams is the live events/stream cap for the process.
	MaxStreams = 128
	// MaxDataBytes is the marshaled size of one occurrence's data object.
	MaxDataBytes = 512

	PollDefault = 50
	PollMax     = 100
	NextPollMs  = 5000

	HeartbeatDefault int64 = 30000
	HeartbeatMin     int64 = 5000
	HeartbeatMax     int64 = 60000
)

// Name is an event type the server offers.
type Name string

const (
	SpanError Name = "span.error"
	Feedback  Name = "feedback.submitted"
)

// Known reports whether name is an event type this process emits.
func Known(name Name) bool {
	return name == SpanError || name == Feedback
}

// Filter selects occurrences for one subscriber.
// An empty Service or Severity matches every value of that field.
type Filter struct {
	Name     Name
	Tenant   string
	Service  string
	Severity string
}

// Occurrence is one stored event. Seq is the tenant's position, not a wire field.
type Occurrence struct {
	EventID   string
	Name      Name
	Timestamp time.Time
	Data      map[string]any
	Seq       uint64
}

// Page is a read of one tenant ring.
type Page struct {
	Events    []Occurrence
	Cursor    uint64
	Truncated bool
	HasMore   bool
}

// LimitError is a stream cap. Max is the ceiling that was hit.
type LimitError struct{ Max int }

func (e *LimitError) Error() string {
	if e == nil {
		return "subscription limit"
	}
	return fmt.Sprintf("subscription limit %d", e.Max)
}

// ErrCursor is an unparsable cursor.
var ErrCursor = errors.New("invalid cursor")

// Hub is the single writer of the event rings.
type Hub struct {
	mu       sync.Mutex
	rings    map[string]*ring
	streams  int
	byTenant map[string]int
	fired    chan struct{}
}

type ring struct {
	items   []Occurrence
	head    uint64
	touched time.Time
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{
		rings:    map[string]*ring{},
		byTenant: map[string]int{},
		fired:    make(chan struct{}),
	}
}

// PublishSpan appends one error span for tenant. An empty tenant is dropped.
func (h *Hub) PublishSpan(tenant, traceID, spanID, service, spanName, summary string, at time.Time) {
	if strings.TrimSpace(tenant) == "" || traceID == "" || spanID == "" {
		return
	}
	data := map[string]any{
		"trace_id":  traceID,
		"span_id":   spanID,
		"service":   clip(service, 80),
		"span_name": clip(spanName, 80),
		"status":    "ERROR",
		"summary":   clip(summary, 200),
	}
	h.append(tenant, Occurrence{
		EventID:   traceID + "/" + spanID,
		Name:      SpanError,
		Timestamp: at.UTC(),
		Data:      fit(data),
	})
}

// PublishFeedback appends one accepted feedback report. An empty tenant is dropped.
func (h *Hub) PublishFeedback(tenant, reportID, tool, severity, summary string, at time.Time) {
	if strings.TrimSpace(tenant) == "" || reportID == "" {
		return
	}
	data := map[string]any{
		"report_id": reportID,
		"service":   "ducktel.agent_feedback",
		"tool_name": clip(tool, 80),
		"severity":  severity,
		"summary":   clip(summary, 200),
	}
	h.append(tenant, Occurrence{
		EventID:   "feedback/" + reportID,
		Name:      Feedback,
		Timestamp: at.UTC(),
		Data:      fit(data),
	})
}

func (h *Hub) append(tenant string, occ Occurrence) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.makeRoom(tenant)
	r := h.rings[tenant]
	if r == nil {
		r = &ring{}
		h.rings[tenant] = r
	}
	r.head++
	occ.Seq = r.head
	r.items = append(r.items, occ)
	if len(r.items) > RingCap {
		r.items = append([]Occurrence(nil), r.items[len(r.items)-RingCap:]...)
	}
	r.touched = time.Now()
	h.wakeLocked()
}

// makeRoom drops the least recently updated other ring when a new tenant
// would exceed MaxTenants. The caller holds h.mu.
func (h *Hub) makeRoom(tenant string) {
	if _, ok := h.rings[tenant]; ok || len(h.rings) < MaxTenants {
		return
	}
	var drop string
	var oldest time.Time
	for id, r := range h.rings {
		if drop == "" || r.touched.Before(oldest) {
			drop = id
			oldest = r.touched
		}
	}
	if drop != "" {
		delete(h.rings, drop)
	}
}

func (h *Hub) wakeLocked() {
	close(h.fired)
	h.fired = make(chan struct{})
}

// Fired returns a channel that is closed on the next append.
// Call it again after it fires. The channel is not closed twice.
func (h *Hub) Fired() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fired
}

// Head is the latest sequence for tenant, or 0 when the ring is empty.
func (h *Hub) Head(tenant string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rings[tenant]
	if r == nil {
		return 0
	}
	return r.head
}

// Read returns occurrences of f.Name for f.Tenant with Seq > after.
// A gap below the oldest retained occurrence sets Truncated.
// maxAge > 0 drops occurrences older than now-maxAge and sets Truncated when it drops any.
// maxEvents caps the batch. The cursor is the last returned sequence, or the
// ring head when the batch is complete, so a filtered-out occurrence is not read again.
func (h *Hub) Read(f Filter, after uint64, maxEvents int, now time.Time, maxAge time.Duration) Page {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rings[f.Tenant]
	head := uint64(0)
	if r != nil {
		head = r.head
	}
	if maxEvents <= 0 {
		maxEvents = PollDefault
	}
	page := Page{Cursor: head}
	if r == nil || len(r.items) == 0 || after >= head {
		return page
	}
	oldest := r.items[0].Seq
	if oldest > after+1 {
		page.Truncated = true
	}
	var cutoff time.Time
	if maxAge > 0 {
		cutoff = now.Add(-maxAge)
	}
	var out []Occurrence
	for _, ev := range r.items {
		if ev.Seq <= after {
			continue
		}
		if maxAge > 0 && ev.Timestamp.Before(cutoff) {
			page.Truncated = true
			continue
		}
		if ev.Name != f.Name || !match(ev, f) {
			continue
		}
		out = append(out, ev)
	}
	if len(out) > maxEvents {
		out = out[:maxEvents]
		page.HasMore = true
		page.Cursor = out[len(out)-1].Seq
	}
	page.Events = out
	return page
}

func match(ev Occurrence, f Filter) bool {
	if f.Service != "" && ev.Data["service"] != f.Service {
		return false
	}
	if f.Severity == "" {
		return true
	}
	switch ev.Name {
	case SpanError:
		return spanSeverity(f.Severity)
	case Feedback:
		got, _ := ev.Data["severity"].(string)
		return strings.EqualFold(got, f.Severity)
	default:
		return false
	}
}

func spanSeverity(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error", "status_code_error":
		return true
	default:
		return false
	}
}

// ParseCursor parses a decimal sequence. An empty string is not a cursor.
func ParseCursor(s string) (uint64, error) {
	if s == "" {
		return 0, ErrCursor
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, ErrCursor
	}
	return n, nil
}

// FormatCursor renders a sequence.
func FormatCursor(n uint64) string { return strconv.FormatUint(n, 10) }

// Acquire reserves one push stream. It returns *LimitError when a cap is hit.
func (h *Hub) Acquire(tenant string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byTenant[tenant] >= StreamsPerTenant {
		return &LimitError{Max: StreamsPerTenant}
	}
	if h.streams >= MaxStreams {
		return &LimitError{Max: MaxStreams}
	}
	h.byTenant[tenant]++
	h.streams++
	return nil
}

// Release returns one push stream. It is a no-op when the tenant has none.
func (h *Hub) Release(tenant string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byTenant[tenant] == 0 {
		return
	}
	h.byTenant[tenant]--
	if h.byTenant[tenant] == 0 {
		delete(h.byTenant, tenant)
	}
	if h.streams > 0 {
		h.streams--
	}
}

// Streams is the number of live push streams.
func (h *Hub) Streams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streams
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// fit shrinks summary, then service, then span_name until data marshals
// within MaxDataBytes.
func fit(data map[string]any) map[string]any {
	for {
		raw, err := json.Marshal(data)
		if err != nil || len(raw) <= MaxDataBytes {
			return data
		}
		if shrink(data, "summary") || shrink(data, "service") || shrink(data, "span_name") {
			continue
		}
		data["summary"] = ""
		return data
	}
}

func shrink(data map[string]any, key string) bool {
	s, _ := data[key].(string)
	if s == "" {
		return false
	}
	n := len(s) / 2
	if n < 1 {
		data[key] = ""
		return true
	}
	data[key] = s[:n]
	return true
}
