package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Flusher asks the writing process to persist its buffered records, so a
// query consumer (MCP tool or REST handler) can make just-received telemetry
// visible immediately instead of waiting out the flush interval.
//
// It authenticates with its own token, never the ingest token: the query side
// is allowed to request a flush but must never be able to write telemetry, so
// the two secrets stay separate. Shared by every query consumer so the
// request-building and error-shaping logic exists exactly once.
type Flusher struct {
	url        string
	token      string
	httpClient *http.Client
}

// NewFlusher builds a Flusher. An empty url disables it — Enabled reports this.
func NewFlusher(url, token string) *Flusher {
	return &Flusher{url: url, token: token, httpClient: &http.Client{Timeout: 30 * time.Second}}
}

// Enabled reports whether a flush endpoint was configured.
func (f *Flusher) Enabled() bool { return f.url != "" }

// Flush requests an immediate flush. Returns an error if disabled, unreachable,
// or rejected.
func (f *Flusher) Flush(ctx context.Context) error {
	if f.url == "" {
		return errors.New("flush is not configured: the server was started without a flush endpoint")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, nil)
	if err != nil {
		return fmt.Errorf("building flush request: %w", err)
	}
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("flush request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("flush rejected: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
