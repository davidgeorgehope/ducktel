package feedback_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/davidgeorgehope/ducktel/internal/feedback"
)

func TestBothSinksFailConfig(t *testing.T) {
	_, err := feedback.New(feedback.Options{
		DataDir:     t.TempDir(),
		WebhookURL:  "https://example.com/hook",
		GitHubRepo:  "acme/ducktel",
		GitHubToken: "ghp_test",
	})
	if err == nil || !strings.Contains(err.Error(), "--feedback-webhook-url") || !strings.Contains(err.Error(), "--feedback-github-repo") {
		t.Fatalf("err = %v", err)
	}
}

func TestWebhookReceivesTheProposedFixUnchanged(t *testing.T) {
	fix := "ship the patch\nAuthorization: Bearer ghp_FAKE_DO_NOT_SCRUB"
	var got feedback.Report
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://hooks.example/feedback?token=supersecret" {
			t.Errorf("url = %s", r.URL.String())
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("json: %v", err)
		}
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	svc, err := feedback.New(feedback.Options{
		Write:      func(context.Context, feedback.Report) error { return nil },
		WebhookURL: "https://hooks.example/feedback?token=supersecret",
		HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	fields := validFields("w1", "acme")
	fields.ProposedFix = fix
	report, err := feedback.ParseFields(fields)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Submit(context.Background(), report)
	if err != nil || !res.Stored || res.Sink != "webhook" || res.SinkError != "" {
		t.Fatalf("result = %+v %v", res, err)
	}
	if got.ProposedFix != fix {
		t.Fatalf("proposed_fix = %q", got.ProposedFix)
	}
}

func TestGitHubIssueAndNon2xx(t *testing.T) {
	fields := validFields("g1", "acme")
	fields.Summary = "broken log line"
	report, err := feedback.ParseFields(fields)
	if err != nil {
		t.Fatal(err)
	}
	var (
		url    string
		auth   string
		accept string
		agent  string
		title  string
		body   string
	)
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		url = r.URL.String()
		auth = r.Header.Get("Authorization")
		accept = r.Header.Get("Accept")
		agent = r.Header.Get("User-Agent")
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Errorf("json: %v", err)
		}
		title = payload.Title
		body = payload.Body
		if len(payload.Labels) != 1 || payload.Labels[0] != "feedback" {
			t.Errorf("labels = %#v, want [feedback]", payload.Labels)
		}
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Status:     "403 Forbidden",
			Body:       io.NopCloser(strings.NewReader(`{"message":"nope"}`)),
			Header:     make(http.Header),
		}, nil
	})}
	svc, err := feedback.New(feedback.Options{
		Write:       func(context.Context, feedback.Report) error { return nil },
		GitHubRepo:  "acme/ducktel",
		GitHubToken: "ghp_test_token",
		HTTPClient:  client,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Submit(context.Background(), report)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stored || res.Sink != "github" || !strings.Contains(res.SinkError, "403") {
		t.Fatalf("result = %+v", res)
	}
	if strings.Contains(res.SinkError, "ghp_test_token") {
		t.Fatalf("sink error contains the token: %s", res.SinkError)
	}
	if url != "https://api.github.com/repos/acme/ducktel/issues" {
		t.Fatalf("url = %s", url)
	}
	if auth != "Bearer ghp_test_token" || accept != "application/vnd.github+json" || agent != "ducktel" {
		t.Fatalf("headers auth=%q accept=%q agent=%q", auth, accept, agent)
	}
	if title != "[ducktel] bug: broken log line" {
		t.Fatalf("title = %q", title)
	}
	for _, want := range []string{report.Summary, report.Details, report.ProposedFix, report.ExampleCall} {
		if !strings.Contains(body, want) {
			t.Fatalf("issue body missing %q\n%s", want, body)
		}
	}
}
