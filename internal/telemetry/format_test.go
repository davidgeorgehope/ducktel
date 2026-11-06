package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMarshalRowsPreservesColumnOrder(t *testing.T) {
	rows := []map[string]any{{"z": 1, "a": 2}}
	out, err := MarshalRows(rows, []string{"z", "a"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"z": 1, "a": 2`) {
		t.Errorf("column order not preserved: %s", out)
	}
	var back []map[string]any
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Errorf("output is not valid JSON: %v\n%s", err, out)
	}
}

func TestMarshalRowsHandlesNil(t *testing.T) {
	out, err := MarshalRows(nil, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("nil rows should render as [], got %q", out)
	}
}

// TestMarshalRowsSurvivesNonFinite is a guard against the NaN/Inf class of bug:
// a single unencodable float must not fail the whole result set.
func TestMarshalRowsSurvivesNonFinite(t *testing.T) {
	nan := 0.0
	nan = nan / nan // NaN
	rows := []map[string]any{{"v": nan, "ok": 1.0}}
	out, err := MarshalRows(rows, []string{"v", "ok"})
	if err != nil {
		t.Fatalf("non-finite value failed the whole result: %v", err)
	}
	var back []map[string]any
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Errorf("output invalid: %v\n%s", err, out)
	}
	if back[0]["v"] != nil {
		t.Errorf("non-finite value should degrade to null, got %v", back[0]["v"])
	}
	if back[0]["ok"] != 1.0 {
		t.Errorf("sibling value lost: %v", back[0]["ok"])
	}
}

// TestSanitizeRows covers the alternative to MarshalRows's per-cell
// degradation: encoding/json.Marshal fails outright on NaN/Inf, so an HTTP
// layer sanitizes rows before marshaling normally.
func TestSanitizeRows(t *testing.T) {
	nan := 0.0
	nan = nan / nan
	rows := []map[string]any{{"v": nan, "ok": 1.0}}
	out := SanitizeRows(rows)
	if out[0]["v"] != nil {
		t.Errorf("non-finite value should become nil, got %v", out[0]["v"])
	}
	if out[0]["ok"] != 1.0 {
		t.Errorf("sibling value lost: %v", out[0]["ok"])
	}
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("sanitized rows should marshal cleanly: %v", err)
	}
}

func TestJSONPathQuotesDottedKeys(t *testing.T) {
	cases := map[string]string{
		"service.name":        `$."service.name"`,
		"http.request.method": `$."http.request.method"`,
		"simple":              `$."simple"`,
		`has"quote`:           `$."has\"quote"`,
	}
	for in, want := range cases {
		if got := jsonPath(in); got != want {
			t.Errorf("jsonPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteIdentAllowlist(t *testing.T) {
	if _, ok := quoteIdent("service_name"); !ok {
		t.Error("service_name should be allowed")
	}
	for _, bad := range []string{"evil", "service_name; DROP TABLE x", "", "1", "resource_attributes"} {
		if _, ok := quoteIdent(bad); ok {
			t.Errorf("%q should not be allowed for interpolation", bad)
		}
	}
}
