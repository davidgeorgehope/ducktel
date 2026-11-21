package feedback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/writer"
)

const (
	maxPerHour    = 20
	maxTenants    = 1024
	maxRemembered = 4096

	httpTimeout = 10 * time.Second
)

// ErrRateLimited is returned when the tenant already stored maxPerHour
// reports in the rolling hour. Invalid input never returns this error.
var ErrRateLimited = errors.New("feedback rate limit exceeded: 20 accepted reports per tenant per rolling hour; a restart clears this window")

// Result is the success payload. Stored is true only after the log is written.
type Result struct {
	ReportID   string `json:"report_id"`
	Stored     bool   `json:"stored"`
	Service    string `json:"service"`
	Sink       string `json:"sink"`
	SinkError  string `json:"sink_error,omitempty"`
	FlushError string `json:"flush_error,omitempty"`
	Note       string `json:"note,omitempty"`
}

// Options selects the one store and the optional sink.
// DataDir, Write, and Endpoint are mutually exclusive.
type Options struct {
	DataDir     string
	Write       func(context.Context, Report) error
	Endpoint    string
	Token       string
	WebhookURL  string
	GitHubRepo  string
	GitHubToken string
	// HTTPClient is used for the feedback POST and the sink. Nil uses a
	// client with a 10s timeout that does not follow redirects.
	HTTPClient *http.Client
}

// Accepted marks a report the store has taken. A later flush may still have
// failed. Submit remembers the id and does not append the report again.
type Accepted struct{ Err error }

func (e *Accepted) Error() string {
	if e == nil || e.Err == nil {
		return "feedback accepted"
	}
	return e.Err.Error()
}

func (e *Accepted) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Service accepts validated reports: rate limit, one store, then one sink.
type Service struct {
	store store
	sink  Sink

	mu       sync.Mutex
	tenants  map[string]*tenantWindow
	seen     map[memKey]memory
	inflight map[memKey]*flight
	seq      uint64
}

type memKey struct {
	tenant string
	id     string
}

type memory struct {
	report Report
	result Result
	at     time.Time
}

type tenantWindow struct {
	slots []slot
	last  time.Time
}

type slot struct {
	id uint64
	at time.Time
}

type flight struct {
	done chan struct{}
}

type store interface {
	Write(ctx context.Context, r Report) error
}

// New builds a service. Both sinks set is an error that names both flags.
func New(opt Options) (*Service, error) {
	opt.DataDir = strings.TrimSpace(opt.DataDir)
	opt.Endpoint = strings.TrimSpace(opt.Endpoint)
	opt.WebhookURL = strings.TrimSpace(opt.WebhookURL)
	opt.GitHubRepo = strings.TrimSpace(opt.GitHubRepo)
	opt.GitHubToken = strings.TrimSpace(opt.GitHubToken)

	client := opt.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout:       httpTimeout,
			CheckRedirect: refuseRedirect,
		}
	}
	sink, err := buildSink(opt, client)
	if err != nil {
		return nil, err
	}

	modes := 0
	if opt.DataDir != "" {
		modes++
	}
	if opt.Write != nil {
		modes++
	}
	if opt.Endpoint != "" {
		modes++
	}
	if modes != 1 {
		return nil, errors.New("feedback storage needs exactly one of a data directory, a local writer, or a feedback URL")
	}

	var st store
	switch {
	case opt.DataDir != "":
		st = &directStore{dir: opt.DataDir}
	case opt.Write != nil:
		st = funcStore{write: opt.Write}
	default:
		if err := parseHTTPURL(opt.Endpoint, "--feedback-url"); err != nil {
			return nil, err
		}
		st = remoteStore{url: opt.Endpoint, token: opt.Token, client: client}
	}

	return &Service{
		store:    st,
		sink:     sink,
		tenants:  make(map[string]*tenantWindow),
		seen:     make(map[memKey]memory),
		inflight: make(map[memKey]*flight),
	}, nil
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// SubmitJSON parses the body, then accepts it. A parse error does not
// consume the rate limit and does not store anything.
func (s *Service) SubmitJSON(ctx context.Context, body []byte) (Result, error) {
	report, err := Parse(body)
	if err != nil {
		return Result{}, err
	}
	return s.Submit(ctx, report)
}

// Submit stores a validated report. A later call with the same tenant and
// report_id returns the remembered result. If the sink failed, the retry
// sends the sink again and does not write a second log.
func (s *Service) Submit(ctx context.Context, r Report) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	key := memKey{tenant: r.Tenant, id: r.ReportID}
	for {
		s.mu.Lock()
		if e, ok := s.seen[key]; ok && e.result.SinkError == "" {
			res := e.result
			s.mu.Unlock()
			return res, nil
		}
		if f, busy := s.inflight[key]; busy {
			s.mu.Unlock()
			select {
			case <-f.done:
				continue
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		}
		if e, ok := s.seen[key]; ok {
			f := &flight{done: make(chan struct{})}
			s.inflight[key] = f
			report := e.report
			s.mu.Unlock()
			sinkErr := s.deliver(ctx, report)
			s.mu.Lock()
			if cur, ok := s.seen[key]; ok {
				cur.result.SinkError = sinkErr
				s.seen[key] = cur
				e = cur
			}
			delete(s.inflight, key)
			close(f.done)
			res := e.result
			s.mu.Unlock()
			return res, nil
		}

		slotID, ok := s.reserve(r.Tenant, time.Now())
		if !ok {
			s.mu.Unlock()
			return Result{}, ErrRateLimited
		}
		f := &flight{done: make(chan struct{})}
		s.inflight[key] = f
		s.mu.Unlock()

		err := s.store.Write(ctx, r)
		var taken *Accepted
		if err != nil && !errors.As(err, &taken) {
			s.mu.Lock()
			s.release(r.Tenant, slotID)
			delete(s.inflight, key)
			close(f.done)
			s.mu.Unlock()
			return Result{}, err
		}
		res := s.finish(ctx, r)
		if taken != nil {
			res.FlushError = taken.Error()
		}
		s.mu.Lock()
		s.remember(key, r, res)
		delete(s.inflight, key)
		close(f.done)
		s.mu.Unlock()
		return res, nil
	}
}

func (s *Service) finish(ctx context.Context, r Report) Result {
	res := Result{
		ReportID: r.ReportID,
		Stored:   true,
		Service:  ServiceName,
		Sink:     "off",
	}
	if s.sink != nil {
		res.Sink = s.sink.Kind()
	}
	res.SinkError = s.deliver(ctx, r)
	return res
}

func (s *Service) deliver(ctx context.Context, r Report) string {
	if s.sink == nil {
		return ""
	}
	if err := s.sink.Send(ctx, r); err != nil {
		return err.Error()
	}
	return ""
}

func (s *Service) reserve(tenant string, now time.Time) (uint64, bool) {
	w := s.tenants[tenant]
	if w == nil {
		if len(s.tenants) >= maxTenants {
			s.dropIdlest()
		}
		w = &tenantWindow{}
		s.tenants[tenant] = w
	}
	cutoff := now.Add(-time.Hour)
	kept := make([]slot, 0, len(w.slots))
	for _, sl := range w.slots {
		if !sl.at.Before(cutoff) {
			kept = append(kept, sl)
		}
	}
	w.slots = kept
	if len(w.slots) >= maxPerHour {
		return 0, false
	}
	s.seq++
	w.slots = append(w.slots, slot{id: s.seq, at: now})
	w.last = now
	return s.seq, true
}

func (s *Service) release(tenant string, id uint64) {
	w := s.tenants[tenant]
	if w == nil {
		return
	}
	kept := make([]slot, 0, len(w.slots))
	for _, sl := range w.slots {
		if sl.id != id {
			kept = append(kept, sl)
		}
	}
	w.slots = kept
}

func (s *Service) dropIdlest() {
	var oldest string
	var at time.Time
	first := true
	for tenant, w := range s.tenants {
		if first || w.last.Before(at) {
			oldest = tenant
			at = w.last
			first = false
		}
	}
	if oldest == "" {
		return
	}
	delete(s.tenants, oldest)
	for key := range s.seen {
		if key.tenant == oldest {
			delete(s.seen, key)
		}
	}
}

func (s *Service) remember(key memKey, r Report, res Result) {
	if _, ok := s.seen[key]; !ok && len(s.seen) >= maxRemembered {
		s.evictOldest()
	}
	s.seen[key] = memory{report: r, result: res, at: time.Now()}
}

func (s *Service) evictOldest() {
	var oldest memKey
	var at time.Time
	first := true
	for key, e := range s.seen {
		if first || e.at.Before(at) {
			oldest = key
			at = e.at
			first = false
		}
	}
	if !first {
		delete(s.seen, oldest)
	}
}

type funcStore struct {
	write func(context.Context, Report) error
}

func (f funcStore) Write(ctx context.Context, r Report) error {
	return f.write(ctx, r)
}

type directStore struct {
	dir string
	mu  sync.Mutex
}

func (s *directStore) Write(ctx context.Context, r Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	release, err := writer.Lock(s.dir)
	if err != nil {
		if errors.Is(err, writer.ErrLocked) {
			return fmt.Errorf("data directory is locked by another writer; set --feedback-url to the running receiver's POST /feedback (a second ducktel serve will not store this report): %w", err)
		}
		return err
	}
	defer release()

	w := writer.New(s.dir, time.Hour, 1000)
	w.AddLogs([]writer.LogRecord{r.LogRecord(time.Now())})
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing feedback log: %w", err)
	}
	return nil
}

type remoteStore struct {
	url    string
	token  string
	client *http.Client
}

func (s remoteStore) Write(ctx context.Context, r Report) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("feedback POST failed: %s", redactURL(s.url))
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("feedback POST failed: %s", redactURL(s.url))
	}
	defer resp.Body.Close()
	slurp, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode == http.StatusTooManyRequests {
		return ErrRateLimited
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(slurp))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("feedback POST rejected: %s", msg)
	}
	var parsed struct {
		FlushError string `json:"flush_error"`
	}
	if json.Unmarshal(slurp, &parsed) == nil && parsed.FlushError != "" {
		return &Accepted{Err: errors.New(parsed.FlushError)}
	}
	return nil
}

func parseHTTPURL(raw, flag string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s must be an http or https URL", flag)
	}
	return nil
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "redacted-url"
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String()
}
