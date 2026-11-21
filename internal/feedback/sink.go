package feedback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Sink delivers one stored report off the machine. A nil sink means off.
type Sink interface {
	Kind() string
	Send(ctx context.Context, r Report) error
}

func buildSink(opt Options, client *http.Client) (Sink, error) {
	if opt.WebhookURL != "" && opt.GitHubRepo != "" {
		return nil, fmt.Errorf("both --feedback-webhook-url and --feedback-github-repo are set; configure at most one feedback sink")
	}
	if opt.WebhookURL != "" {
		if err := parseHTTPURL(opt.WebhookURL, "--feedback-webhook-url"); err != nil {
			return nil, err
		}
		return webhookSink{url: opt.WebhookURL, client: client}, nil
	}
	if opt.GitHubRepo == "" {
		return nil, nil
	}
	if opt.GitHubToken == "" {
		return nil, fmt.Errorf("--feedback-github-token is required when --feedback-github-repo is set")
	}
	owner, repo, err := splitRepo(opt.GitHubRepo)
	if err != nil {
		return nil, err
	}
	return githubSink{owner: owner, repo: repo, token: opt.GitHubToken, client: client}, nil
}

func splitRepo(s string) (owner, repo string, err error) {
	owner, repo, ok := strings.Cut(s, "/")
	if !ok || strings.Contains(repo, "/") || !repoPart(owner) || !repoPart(repo) {
		return "", "", fmt.Errorf("--feedback-github-repo must be owner/repo")
	}
	return owner, repo, nil
}

func repoPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

type webhookSink struct {
	url    string
	client *http.Client
}

func (w webhookSink) Kind() string { return "webhook" }

func (w webhookSink) Send(ctx context.Context, r Report) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook request failed: %s", redactURL(w.url))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook request failed: %s", redactURL(w.url))
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook rejected the report: %s", resp.Status)
	}
	return nil
}

type githubSink struct {
	owner  string
	repo   string
	token  string
	client *http.Client
}

func (g githubSink) Kind() string { return "github" }

func (g githubSink) Send(ctx context.Context, r Report) error {
	payload, err := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}{IssueTitle(r), Markdown([]Report{r})})
	if err != nil {
		return err
	}
	endpoint := "https://api.github.com/repos/" + g.owner + "/" + g.repo + "/issues"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("github request failed")
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ducktel")
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("github request failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github rejected the report: %s", resp.Status)
	}
	return nil
}
