package receiver

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"testing"

	collectlogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collecttracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

func TestJSONTraceIDsAreHexDecoded(t *testing.T) {
	const body = `{"resourceSpans":[{"scopeSpans":[{"spans":[{` +
		`"traceId":"0102030405060708090a0b0c0d0e0f10",` +
		`"spanId":"0102030405060708",` +
		`"parentSpanId":"1112131415161718",` +
		`"name":"op",` +
		`"startTimeUnixNano":"1700000000000000001",` +
		`"endTimeUnixNano":"1700000000000000002"}]}]}]}`

	req := httptest.NewRequest("POST", "/v1/traces", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	msg := &collecttracev1.ExportTraceServiceRequest{}
	if err := unmarshalOTLP(req, msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	spans := msg.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]

	wantTrace := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	if !bytes.Equal(span.GetTraceId(), wantTrace) {
		t.Errorf("trace_id = %x, want %x (OTLP/JSON ids are hex, not base64)",
			span.GetTraceId(), wantTrace)
	}
	wantSpan := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if !bytes.Equal(span.GetSpanId(), wantSpan) {
		t.Errorf("span_id = %x, want %x", span.GetSpanId(), wantSpan)
	}
	wantParent := []byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}
	if !bytes.Equal(span.GetParentSpanId(), wantParent) {
		t.Errorf("parent_span_id = %x, want %x", span.GetParentSpanId(), wantParent)
	}

	// The rewrite must not disturb other fields: a 64-bit value above 2^53
	// would lose precision if it passed through float64.
	if got := span.GetStartTimeUnixNano(); got != 1700000000000000001 {
		t.Errorf("start_time_unix_nano = %d, want 1700000000000000001", got)
	}
}

func TestJSONLogTraceIDsAreHexDecoded(t *testing.T) {
	const body = `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{` +
		`"traceId":"0102030405060708090a0b0c0d0e0f10",` +
		`"spanId":"0102030405060708",` +
		`"body":{"stringValue":"hello"}}]}]}]}`

	req := httptest.NewRequest("POST", "/v1/logs", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	msg := &collectlogsv1.ExportLogsServiceRequest{}
	if err := unmarshalOTLP(req, msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	records := msg.GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	if got := records[0].GetTraceId(); !bytes.Equal(got, want) {
		t.Errorf("log trace_id = %x, want %x", got, want)
	}
	if got := records[0].GetBody().GetStringValue(); got != "hello" {
		t.Errorf("body = %q, want hello", got)
	}
}

// TestJSONBase64IDsStillAccepted pins the compatibility path: a sender that
// emits standard protobuf base64 (rather than the OTLP hex form) keeps
// working, because a value that is not valid hex is passed through untouched.
func TestJSONBase64IDsStillAccepted(t *testing.T) {
	idBytes := []byte("0123456789abcdef")
	enc := base64.StdEncoding.EncodeToString(idBytes)
	body := fmt.Sprintf(`{"resourceSpans":[{"scopeSpans":[{"spans":[{`+
		`"traceId":%q,"spanId":%q}]}]}]}`, enc, enc)

	req := httptest.NewRequest("POST", "/v1/traces", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	msg := &collecttracev1.ExportTraceServiceRequest{}
	if err := unmarshalOTLP(req, msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	span := msg.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
	if !bytes.Equal(span.GetTraceId(), idBytes) {
		t.Errorf("trace_id = %x, want %x", span.GetTraceId(), idBytes)
	}
}
