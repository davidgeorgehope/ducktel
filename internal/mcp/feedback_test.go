package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/davidgeorgehope/ducktel/internal/feedback"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func feedbackArgs(id, tenant string) map[string]any {
	return map[string]any{
		"report_id":    id,
		"tenant":       tenant,
		"tool_name":    "log_search",
		"category":     "confusing_output",
		"severity":     "low",
		"summary":      "rows hid the error",
		"details":      "the tool returned an empty list",
		"proposed_fix": "include the error string in the empty result",
		"example_call": `log_search(tenant="acme")`,
	}
}

func TestSubmitFeedbackTool(t *testing.T) {
	session := newTestServerWith(t, func(ds *Server) {
		svc, err := feedback.New(feedback.Options{DataDir: ds.dataDir})
		if err != nil {
			t.Fatal(err)
		}
		ds.SetFeedback(svc)
	})

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var desc, saveDesc string
	for _, tool := range tools.Tools {
		switch tool.Name {
		case "submit_feedback":
			desc = tool.Description
		case "save_query":
			saveDesc = tool.Description
		}
	}
	for _, phrase := range []string{
		"confusing, unhelpful, or wrong",
		"capability was missing",
		"concrete proposed fix",
		"ducktel.agent_feedback",
		"log_search",
		"logs dashboard",
		"unless a sink is configured",
		"does not scrub",
	} {
		if !strings.Contains(desc, phrase) {
			t.Errorf("description missing %q", phrase)
		}
	}
	if !strings.Contains(saveDesc, "submit_feedback") {
		t.Fatalf("save_query description = %q", saveDesc)
	}

	body, isErr := call(t, session, "submit_feedback", feedbackArgs("fb-1", "acme"))
	if isErr {
		t.Fatal(body)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	if res["report_id"] != "fb-1" || res["stored"] != true || res["service"] != feedback.ServiceName || res["sink"] != "off" {
		t.Fatalf("result = %s", body)
	}
	again, isErr := call(t, session, "submit_feedback", feedbackArgs("fb-1", "acme"))
	if isErr {
		t.Fatal(again)
	}
	logs, isErr := call(t, session, "log_search", map[string]any{
		"tenant": "acme", "service_name": feedback.ServiceName, "since_minutes": 60,
	})
	if isErr {
		t.Fatal(logs)
	}
	if n := countRows(t, logs); n != 1 {
		t.Fatalf("log_search rows = %d, want 1\n%s", n, logs)
	}
	if !strings.Contains(logs, "include the error string") || !strings.Contains(logs, "INFO") {
		t.Fatalf("log row = %s", logs)
	}
	other, isErr := call(t, session, "log_search", map[string]any{
		"tenant": "globex", "service_name": feedback.ServiceName, "since_minutes": 60,
	})
	if isErr {
		t.Fatal(other)
	}
	if n := firstArrayLen(t, other); n != 0 {
		t.Fatalf("globex rows = %d\n%s", n, other)
	}
}

func TestSubmitFeedbackUnwired(t *testing.T) {
	session := newTestServer(t)
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "submit_feedback" {
			found = true
		}
	}
	if !found {
		t.Fatal("submit_feedback was not registered")
	}
	body, isErr := call(t, session, "submit_feedback", feedbackArgs("fb-1", "acme"))
	if !isErr || !strings.Contains(body, "not configured") {
		t.Fatalf("unwired = %q err %v", body, isErr)
	}
}

func TestSubmitFeedbackWhileLockedWritesNothing(t *testing.T) {
	var dir string
	session := newTestServerWith(t, func(ds *Server) {
		dir = ds.dataDir
		svc, err := feedback.New(feedback.Options{DataDir: ds.dataDir})
		if err != nil {
			t.Fatal(err)
		}
		ds.SetFeedback(svc)
	})
	release, err := writer.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release() })

	body, isErr := call(t, session, "submit_feedback", feedbackArgs("locked", "acme"))
	if !isErr || !strings.Contains(body, "--feedback-url") {
		t.Fatalf("locked = %q err %v", body, isErr)
	}
	logs, isErr := call(t, session, "log_search", map[string]any{
		"tenant": "acme", "service_name": feedback.ServiceName, "since_minutes": 60,
	})
	if isErr {
		t.Fatal(logs)
	}
	if n := firstArrayLen(t, logs); n != 0 {
		t.Fatalf("locked write stored %d rows\n%s", n, logs)
	}
}

func firstArrayLen(t *testing.T, body string) int {
	t.Helper()
	var rows []map[string]any
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&rows); err != nil {
		t.Fatalf("response is not a JSON array: %v\n%s", err, body)
	}
	return len(rows)
}
