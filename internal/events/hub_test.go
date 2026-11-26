package events

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNullCursorIsTheHead(t *testing.T) {
	h := NewHub()
	if head := h.Head("acme"); head != 0 {
		t.Fatalf("head = %d", head)
	}
	h.PublishFeedback("acme", "fb-1", "log_search", "low", "rows hid the error", time.Now())
	page := h.Read(Filter{Name: Feedback, Tenant: "acme"}, h.Head("acme"), 50, time.Now(), 0)
	if len(page.Events) != 0 || page.Cursor != 1 {
		t.Fatalf("read at head = %+v", page)
	}
	page = h.Read(Filter{Name: Feedback, Tenant: "acme"}, 0, 50, time.Now(), 0)
	if len(page.Events) != 1 || page.Events[0].EventID != "feedback/fb-1" {
		t.Fatalf("events = %+v", page.Events)
	}
}

func TestTenantIsolation(t *testing.T) {
	h := NewHub()
	h.PublishSpan("acme", "trace-a", "span-a", "api", "GET", "declined", time.Now())
	h.PublishSpan("globex", "trace-g", "span-g", "api", "GET", "declined", time.Now())
	page := h.Read(Filter{Name: SpanError, Tenant: "acme"}, 0, 50, time.Now(), 0)
	if len(page.Events) != 1 || page.Events[0].Data["trace_id"] != "trace-a" {
		t.Fatalf("acme events = %+v", page.Events)
	}
	for _, ev := range page.Events {
		if strings.Contains(ev.EventID, "globex") || ev.Data["trace_id"] == "trace-g" {
			t.Fatalf("acme saw globex: %+v", ev)
		}
	}
	other := h.Read(Filter{Name: SpanError, Tenant: "globex"}, 0, 50, time.Now(), 0)
	if len(other.Events) != 1 || other.Events[0].Data["span_id"] != "span-g" {
		t.Fatalf("globex events = %+v", other.Events)
	}
}

func TestFilters(t *testing.T) {
	h := NewHub()
	now := time.Now()
	h.PublishSpan("acme", "t1", "s1", "api", "GET", "nope", now)
	h.PublishSpan("acme", "t2", "s2", "worker", "POST", "nope", now)
	h.PublishFeedback("acme", "fb", "log_search", "high", "broken", now)

	api := h.Read(Filter{Name: SpanError, Tenant: "acme", Service: "api"}, 0, 50, now, 0)
	if len(api.Events) != 1 || api.Events[0].Data["service"] != "api" {
		t.Fatalf("service filter = %+v", api.Events)
	}
	ok := h.Read(Filter{Name: SpanError, Tenant: "acme", Severity: "ERROR"}, 0, 50, now, 0)
	if len(ok.Events) != 2 {
		t.Fatalf("severity ERROR = %d", len(ok.Events))
	}
	none := h.Read(Filter{Name: SpanError, Tenant: "acme", Severity: "info"}, 0, 50, now, 0)
	if len(none.Events) != 0 {
		t.Fatalf("severity info = %+v", none.Events)
	}
	high := h.Read(Filter{Name: Feedback, Tenant: "acme", Severity: "HIGH"}, 0, 50, now, 0)
	if len(high.Events) != 1 {
		t.Fatalf("feedback severity = %+v", high.Events)
	}
}

func TestRingDropsTheOldest(t *testing.T) {
	h := NewHub()
	for i := 1; i <= RingCap+1; i++ {
		h.PublishFeedback("acme", strconv.Itoa(i), "log_search", "low", "s", time.Now())
	}
	page := h.Read(Filter{Name: Feedback, Tenant: "acme"}, 0, RingCap+5, time.Now(), 0)
	if !page.Truncated {
		t.Fatal("expected truncated")
	}
	if len(page.Events) != RingCap {
		t.Fatalf("len = %d", len(page.Events))
	}
	if page.Events[0].EventID == "feedback/1" {
		t.Fatal("dropped event was returned")
	}
	if page.Events[0].Seq != 2 || page.Events[len(page.Events)-1].Seq != uint64(RingCap+1) {
		t.Fatalf("seq range %d..%d", page.Events[0].Seq, page.Events[len(page.Events)-1].Seq)
	}
}

func TestPayloadBound(t *testing.T) {
	h := NewHub()
	huge := strings.Repeat("x", 10000)
	h.PublishSpan("acme", "trace", "span", huge, huge, huge, time.Now())
	h.PublishFeedback("acme", "fb", "tool", "low", huge, time.Now())
	for _, name := range []Name{SpanError, Feedback} {
		page := h.Read(Filter{Name: name, Tenant: "acme"}, 0, 10, time.Now(), 0)
		if len(page.Events) != 1 {
			t.Fatalf("%s events = %d", name, len(page.Events))
		}
		raw, err := json.Marshal(page.Events[0].Data)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > MaxDataBytes {
			t.Fatalf("%s data is %d bytes: %s", name, len(raw), raw)
		}
		if _, ok := page.Events[0].Data["details"]; ok {
			t.Fatal("payload includes details")
		}
	}
}

func TestBadCursor(t *testing.T) {
	if _, err := ParseCursor("nope"); err != ErrCursor {
		t.Fatalf("err = %v", err)
	}
	if _, err := ParseCursor(""); err != ErrCursor {
		t.Fatalf("empty err = %v", err)
	}
}

func TestStreamCap(t *testing.T) {
	h := NewHub()
	for i := 0; i < StreamsPerTenant; i++ {
		if err := h.Acquire("acme"); err != nil {
			t.Fatal(err)
		}
	}
	err := h.Acquire("acme")
	lim, ok := err.(*LimitError)
	if !ok || lim.Max != StreamsPerTenant {
		t.Fatalf("err = %v", err)
	}
	h.Release("acme")
	if err := h.Acquire("acme"); err != nil {
		t.Fatal(err)
	}
	if h.Streams() != StreamsPerTenant {
		t.Fatalf("streams = %d", h.Streams())
	}
}
