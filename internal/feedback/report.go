// Package feedback is an agent report stored as one OpenTelemetry log.
//
// Parse and ParseFields are the only constructors. Callers inside the
// process pass the resulting Report and do not validate it again.
package feedback

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/davidgeorgehope/ducktel/internal/writer"
)

const (
	// ServiceName is the log service and the service.name resource attribute.
	ServiceName = "ducktel.agent_feedback"
	// ScopeName is the instrumentation scope on the stored log.
	ScopeName = "ducktel.feedback"
)

// Category is why the agent filed the report.
type Category string

const (
	CategoryBug               Category = "bug"
	CategoryConfusingOutput   Category = "confusing_output"
	CategoryMissingCapability Category = "missing_capability"
	CategoryWrongDocs         Category = "wrong_docs"
	CategoryOther             Category = "other"
)

// Severity is the agent's own severity, not the OpenTelemetry number.
type Severity string

const (
	SeverityLow    Severity = "low"
	SeverityMedium Severity = "medium"
	SeverityHigh   Severity = "high"
)

type otelSeverity struct {
	text   string
	number int32
}

var severities = map[Severity]otelSeverity{
	SeverityLow:    {"INFO", 9},
	SeverityMedium: {"WARN", 13},
	SeverityHigh:   {"ERROR", 17},
}

var categories = map[Category]struct{}{
	CategoryBug:               {},
	CategoryConfusingOutput:   {},
	CategoryMissingCapability: {},
	CategoryWrongDocs:         {},
	CategoryOther:             {},
}

const (
	maxReportID = 64
	maxTenant   = 256
	maxToolName = 128
	maxSummary  = 200
	maxDetails  = 8000
	maxFix      = 8000
	maxExample  = 4000
	maxTitle    = 120
)

// Report is one validated feedback report.
type Report struct {
	ReportID    string   `json:"report_id"`
	Tenant      string   `json:"tenant"`
	ToolName    string   `json:"tool_name"`
	Category    Category `json:"category"`
	Severity    Severity `json:"severity"`
	Summary     string   `json:"summary"`
	Details     string   `json:"details"`
	ProposedFix string   `json:"proposed_fix"`
	ExampleCall string   `json:"example_call"`
}

// Fields is the raw text the HTTP body and the MCP arguments carry.
type Fields struct {
	ReportID    string `json:"report_id"`
	Tenant      string `json:"tenant"`
	ToolName    string `json:"tool_name"`
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Summary     string `json:"summary"`
	Details     string `json:"details"`
	ProposedFix string `json:"proposed_fix"`
	ExampleCall string `json:"example_call"`
}

// FieldError is rejected input. HTTP maps it to 400.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string {
	if e.Field == "" {
		return e.Reason
	}
	return e.Field + ": " + e.Reason
}

// Parse decodes one JSON object and rejects unknown fields.
func Parse(body []byte) (Report, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return Report{}, &FieldError{Field: "body", Reason: "JSON object is required"}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var fields Fields
	if err := dec.Decode(&fields); err != nil {
		return Report{}, &FieldError{Field: "body", Reason: "invalid JSON: " + err.Error()}
	}
	if dec.More() {
		return Report{}, &FieldError{Field: "body", Reason: "trailing data after JSON"}
	}
	return ParseFields(fields)
}

// ParseFields validates already-decoded fields. An empty report id is generated.
func ParseFields(fields Fields) (Report, error) {
	id := strings.TrimSpace(fields.ReportID)
	if id == "" {
		var err error
		id, err = newReportID()
		if err != nil {
			return Report{}, err
		}
	} else if err := checkReportID(id); err != nil {
		return Report{}, err
	}

	tenant, err := required("tenant", fields.Tenant, maxTenant)
	if err != nil {
		return Report{}, err
	}
	tool, err := required("tool_name", fields.ToolName, maxToolName)
	if err != nil {
		return Report{}, err
	}
	category := Category(strings.TrimSpace(fields.Category))
	if _, ok := categories[category]; !ok {
		return Report{}, &FieldError{Field: "category", Reason: "must be bug, confusing_output, missing_capability, wrong_docs, or other"}
	}
	severity := Severity(strings.TrimSpace(fields.Severity))
	if _, ok := severities[severity]; !ok {
		return Report{}, &FieldError{Field: "severity", Reason: "must be low, medium, or high"}
	}
	summary, err := required("summary", fields.Summary, maxSummary)
	if err != nil {
		return Report{}, err
	}
	details, err := required("details", fields.Details, maxDetails)
	if err != nil {
		return Report{}, err
	}
	fix, err := required("proposed_fix", fields.ProposedFix, maxFix)
	if err != nil {
		return Report{}, err
	}
	example := strings.TrimSpace(fields.ExampleCall)
	if len(example) > maxExample {
		return Report{}, &FieldError{Field: "example_call", Reason: "longer than 4000 bytes"}
	}

	return Report{
		ReportID:    id,
		Tenant:      tenant,
		ToolName:    tool,
		Category:    category,
		Severity:    severity,
		Summary:     summary,
		Details:     details,
		ProposedFix: fix,
		ExampleCall: example,
	}, nil
}

func required(field, value string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", &FieldError{Field: field, Reason: "required"}
	}
	if len(value) > max {
		return "", &FieldError{Field: field, Reason: "too long"}
	}
	return value, nil
}

func checkReportID(id string) error {
	if len(id) < 1 || len(id) > maxReportID {
		return &FieldError{Field: "report_id", Reason: "must be 1 to 64 bytes"}
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return &FieldError{Field: "report_id", Reason: "must match [A-Za-z0-9._-]"}
		}
	}
	return nil
}

func newReportID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// LogRecord is the one log this report stores. now is the timestamp.
func (r Report) LogRecord(now time.Time) writer.LogRecord {
	sev := severities[r.Severity]
	usec := now.UnixMicro()
	attrs, _ := json.Marshal(struct {
		ReportID    string `json:"report_id"`
		ToolName    string `json:"tool_name"`
		Category    string `json:"category"`
		Severity    string `json:"severity"`
		Summary     string `json:"summary"`
		Details     string `json:"details"`
		ProposedFix string `json:"proposed_fix"`
		ExampleCall string `json:"example_call"`
	}{r.ReportID, r.ToolName, string(r.Category), string(r.Severity), r.Summary, r.Details, r.ProposedFix, r.ExampleCall})
	resource, _ := json.Marshal(struct {
		Service string `json:"service.name"`
		Tenant  string `json:"tenant.id"`
	}{ServiceName, r.Tenant})
	return writer.LogRecord{
		Timestamp:          usec,
		ObservedTimestamp:  usec,
		SeverityNumber:     sev.number,
		SeverityText:       sev.text,
		Body:               r.plainBody(),
		Attributes:         string(attrs),
		ResourceAttributes: string(resource),
		ServiceName:        ServiceName,
		ScopeName:          ScopeName,
		Flags:              0,
	}
}

func (r Report) plainBody() string {
	var b strings.Builder
	b.WriteString(r.Summary)
	b.WriteString("\n\n")
	b.WriteString(r.Details)
	b.WriteString("\n\nProposed fix:\n")
	b.WriteString(r.ProposedFix)
	b.WriteString("\n\nExample call:\n")
	b.WriteString(r.ExampleCall)
	return b.String()
}

// Markdown renders reports the way `ducktel feedback export` prints them.
// One report is the GitHub issue body.
func Markdown(reports []Report) string {
	var b strings.Builder
	for i, r := range reports {
		if i > 0 {
			b.WriteString("\n---\n\n")
		}
		writeMarkdown(&b, r)
	}
	return b.String()
}

func writeMarkdown(b *strings.Builder, r Report) {
	b.WriteString("## ")
	b.WriteString(string(r.Category))
	b.WriteString(": ")
	b.WriteString(r.Summary)
	b.WriteString("\n\n- Report ID: `")
	b.WriteString(r.ReportID)
	b.WriteString("`\n- Tenant: `")
	b.WriteString(r.Tenant)
	b.WriteString("`\n- Tool: `")
	b.WriteString(r.ToolName)
	b.WriteString("`\n- Severity: ")
	b.WriteString(string(r.Severity))
	b.WriteString("\n\n### Details\n\n")
	b.WriteString(r.Details)
	b.WriteString("\n\n### Proposed fix\n\n")
	b.WriteString(r.ProposedFix)
	if r.ExampleCall != "" {
		b.WriteString("\n\n### Example call\n\n")
		b.WriteString(r.ExampleCall)
	}
	b.WriteString("\n")
}

// IssueTitle is the GitHub issue title, at most 120 bytes, cut on a rune boundary.
func IssueTitle(r Report) string {
	prefix := "[ducktel] " + string(r.Category) + ": "
	if len(prefix) >= maxTitle {
		return cutBytes(prefix, maxTitle)
	}
	return prefix + cutBytes(r.Summary, maxTitle-len(prefix))
}

func cutBytes(s string, max int) string {
	if len(s) <= max || max <= 0 {
		if max <= 0 {
			return ""
		}
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
