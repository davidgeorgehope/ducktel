package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/feedback"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func TestFeedbackCLIHidesOtherTenantsAndSinceZero(t *testing.T) {
	dir := t.TempDir()
	old, err := feedback.ParseFields(feedback.Fields{
		ReportID: "old-1", Tenant: "acme", ToolName: "log_search",
		Category: "bug", Severity: "low", Summary: "ancient-acme-only",
		Details: "d", ProposedFix: "f",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := old.LogRecord(time.Now().Add(-10 * 24 * time.Hour))
	w := writer.New(dir, time.Hour, 1000)
	w.AddLogs([]writer.LogRecord{rec})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	svc, err := feedback.New(feedback.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := feedback.ParseFields(feedback.Fields{
		ReportID: "new-1", Tenant: "acme", ToolName: "log_search",
		Category: "bug", Severity: "high", Summary: "fresh-acme-only",
		Details: "details text", ProposedFix: "fix text", ExampleCall: "example text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}

	prevDir, prevFormat := dataDir, format
	t.Cleanup(func() {
		dataDir = prevDir
		format = prevFormat
	})
	dataDir = dir
	format = "json"

	week := runFeedback(t, "list", "--tenant", "acme")
	if strings.Contains(week, "ancient-acme-only") || !strings.Contains(week, "fresh-acme-only") {
		t.Fatalf("default since = %s", week)
	}
	all := runFeedback(t, "list", "--tenant", "acme", "--since", "0")
	if !strings.Contains(all, "ancient-acme-only") || !strings.Contains(all, "fresh-acme-only") {
		t.Fatalf("--since 0 = %s", all)
	}
	other := runFeedback(t, "list", "--tenant", "globex", "--since", "0")
	if strings.Contains(other, "acme-only") {
		t.Fatalf("globex list = %s", other)
	}

	md := runFeedback(t, "export", "--tenant", "acme", "--since", "0")
	for _, want := range []string{"fresh-acme-only", "details text", "fix text", "example text", "ancient-acme-only"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q\n%s", want, md)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(md), "[") {
		t.Fatalf("default export is JSON:\n%s", md)
	}
	js := runFeedback(t, "export", "--tenant", "acme", "--as", "json", "--since", "168h")
	if !strings.Contains(js, `"proposed_fix"`) || !strings.Contains(js, "fix text") || strings.Contains(js, "ancient-acme-only") {
		t.Fatalf("json export = %s", js)
	}
	if strings.Contains(js, `"body"`) || strings.Contains(js, "resource_attributes") {
		t.Fatalf("json export looks like a raw log row: %s", js)
	}
	hidden := runFeedback(t, "export", "--tenant", "globex", "--as", "json", "--since", "0")
	if strings.Contains(hidden, "acme-only") {
		t.Fatalf("globex export = %s", hidden)
	}
}

func TestMCPRejectsNonPositiveFeedbackGlobalLimit(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		cmd := mcpCmd()
		cmd.SetArgs([]string{"--feedback-global-limit", v})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--feedback-global-limit") {
			t.Fatalf("--feedback-global-limit %s: %v", v, err)
		}
	}
}

func TestMCPRejectsBothFeedbackSinks(t *testing.T) {
	prev := dataDir
	dataDir = t.TempDir()
	t.Cleanup(func() { dataDir = prev })
	cmd := mcpCmd()
	cmd.SetArgs([]string{
		"--feedback-webhook-url", "https://example.com/hook",
		"--feedback-github-repo", "acme/ducktel",
		"--feedback-github-token", "ghp_test",
	})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--feedback-webhook-url") || !strings.Contains(err.Error(), "--feedback-github-repo") {
		t.Fatalf("err = %v", err)
	}
}

func runFeedback(t *testing.T, args ...string) string {
	t.Helper()
	cmd := feedbackCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%s: %v", strings.Join(args, " "), err)
	}
	return buf.String()
}
