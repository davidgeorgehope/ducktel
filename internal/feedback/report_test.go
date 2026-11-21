package feedback_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/davidgeorgehope/ducktel/internal/feedback"
)

func validMap() map[string]any {
	return map[string]any{
		"tenant":       "acme",
		"tool_name":    "log_search",
		"category":     "bug",
		"severity":     "high",
		"summary":      "rows hid the error",
		"details":      "the tool returned an empty list",
		"proposed_fix": "include the error string in the empty result",
		"example_call": `log_search(tenant="acme")`,
	}
}

func TestParseRejectsInvalidFields(t *testing.T) {
	cases := []struct {
		name  string
		field string
		edit  func(map[string]any)
	}{
		{"empty tenant", "tenant", func(m map[string]any) { m["tenant"] = "   " }},
		{"long tenant", "tenant", func(m map[string]any) { m["tenant"] = strings.Repeat("a", 257) }},
		{"empty tool", "tool_name", func(m map[string]any) { m["tool_name"] = "" }},
		{"long tool", "tool_name", func(m map[string]any) { m["tool_name"] = strings.Repeat("a", 129) }},
		{"bad category", "category", func(m map[string]any) { m["category"] = "Bug" }},
		{"bad severity", "severity", func(m map[string]any) { m["severity"] = "critical" }},
		{"empty summary", "summary", func(m map[string]any) { m["summary"] = "" }},
		{"long summary", "summary", func(m map[string]any) { m["summary"] = strings.Repeat("a", 201) }},
		{"summary byte length", "summary", func(m map[string]any) { m["summary"] = strings.Repeat("é", 101) }},
		{"empty details", "details", func(m map[string]any) { m["details"] = "  " }},
		{"long details", "details", func(m map[string]any) { m["details"] = strings.Repeat("a", 8001) }},
		{"empty proposed fix", "proposed_fix", func(m map[string]any) { m["proposed_fix"] = "" }},
		{"long proposed fix", "proposed_fix", func(m map[string]any) { m["proposed_fix"] = strings.Repeat("a", 8001) }},
		{"long example", "example_call", func(m map[string]any) { m["example_call"] = strings.Repeat("a", 4001) }},
		{"bad report id", "report_id", func(m map[string]any) { m["report_id"] = "has space" }},
		{"long report id", "report_id", func(m map[string]any) { m["report_id"] = strings.Repeat("a", 65) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validMap()
			tc.edit(m)
			body, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			_, err = feedback.Parse(body)
			var fe *feedback.FieldError
			if !errors.As(err, &fe) || fe.Field != tc.field {
				t.Fatalf("err = %v, want field %s", err, tc.field)
			}
		})
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	m := validMap()
	m["extra"] = true
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = feedback.Parse(body)
	var fe *feedback.FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want FieldError", err)
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("err = %v, want unknown field", err)
	}
}

func TestParseTrimsAndGeneratesID(t *testing.T) {
	m := validMap()
	m["tenant"] = "  acme  "
	m["summary"] = "  rows hid the error  "
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	first, err := feedback.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if first.Tenant != "acme" || first.Summary != "rows hid the error" {
		t.Fatalf("trim = %+v", first)
	}
	if err := checkID(first.ReportID); err != nil {
		t.Fatal(err)
	}
	second, err := feedback.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if second.ReportID == first.ReportID {
		t.Fatal("two empty ids produced the same report_id")
	}
}

func TestIssueTitleCutsOnRuneBoundary(t *testing.T) {
	short, err := feedback.ParseFields(feedback.Fields{
		ReportID: "id-1", Tenant: "acme", ToolName: "log_search",
		Category: "bug", Severity: "low", Summary: "short title",
		Details: "d", ProposedFix: "f",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := feedback.IssueTitle(short), "[ducktel] bug: short title"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}

	// 89 ASCII bytes plus a 2-byte rune. The 90-byte budget must drop the rune
	// rather than split it, and the title stays within 120 bytes.
	summary := strings.Repeat("a", 89) + "é"
	long, err := feedback.ParseFields(feedback.Fields{
		ReportID: "id-2", Tenant: "acme", ToolName: "log_search",
		Category: "missing_capability", Severity: "medium", Summary: summary,
		Details: "d", ProposedFix: "f",
	})
	if err != nil {
		t.Fatal(err)
	}
	title := feedback.IssueTitle(long)
	prefix := "[ducktel] missing_capability: "
	if len(prefix) != 30 {
		t.Fatalf("prefix length = %d, want 30", len(prefix))
	}
	if !strings.HasPrefix(title, prefix+strings.Repeat("a", 89)) || strings.Contains(title, "é") {
		t.Fatalf("title = %q", title)
	}
	if len(title) > 120 || !utf8.ValidString(title) {
		t.Fatalf("title length %d valid %v", len(title), utf8.ValidString(title))
	}
}

func TestLogSeverityMapping(t *testing.T) {
	for _, tc := range []struct {
		word   string
		text   string
		number int32
	}{
		{"low", "INFO", 9},
		{"medium", "WARN", 13},
		{"high", "ERROR", 17},
	} {
		r, err := feedback.ParseFields(feedback.Fields{
			ReportID: "id", Tenant: "acme", ToolName: "log_search",
			Category: "other", Severity: tc.word, Summary: "s", Details: "d", ProposedFix: "f",
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := r.LogRecord(time.Unix(0, 0))
		if rec.SeverityText != tc.text || rec.SeverityNumber != tc.number {
			t.Fatalf("%s -> %s %d", tc.word, rec.SeverityText, rec.SeverityNumber)
		}
		if rec.ServiceName != feedback.ServiceName || rec.ScopeName != "ducktel.feedback" || rec.Flags != 0 {
			t.Fatalf("record = %+v", rec)
		}
	}
}

func TestMarkdownIncludesTheReport(t *testing.T) {
	r, err := feedback.ParseFields(feedback.Fields{
		ReportID: "rep.1", Tenant: "acme", ToolName: "log_search",
		Category: "wrong_docs", Severity: "low", Summary: "docs skip the flag",
		Details: "the page never mentions it", ProposedFix: "add the flag to the page",
		ExampleCall: "ducktel mcp --feedback-url http://127.0.0.1:4318/feedback",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := feedback.Markdown([]feedback.Report{r})
	for _, want := range []string{r.Summary, r.Details, r.ProposedFix, r.ExampleCall, "rep.1", "wrong_docs"} {
		if !strings.Contains(text, want) {
			t.Fatalf("markdown missing %q\n%s", want, text)
		}
	}
}

func checkID(id string) error {
	if len(id) < 1 || len(id) > 64 {
		return errors.New("id length")
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return errors.New("id charset")
		}
	}
	return nil
}
