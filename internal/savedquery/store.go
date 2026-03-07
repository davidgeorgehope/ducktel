package savedquery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Query represents a saved query that an LLM agent can run periodically.
// There is no webhook, no notification system, no alerting infrastructure.
// The LLM runs these queries, reads the results, and decides what to do.
type Query struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	SQL         string    `json:"sql"`
	Tags        []string  `json:"tags,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RunResult captures the output of running a saved query.
type RunResult struct {
	Name      string                   `json:"name"`
	SQL       string                   `json:"sql"`
	RanAt     time.Time                `json:"ran_at"`
	RowCount  int                      `json:"row_count"`
	Columns   []string                 `json:"columns,omitempty"`
	Results   []map[string]interface{} `json:"results"`
	Error     string                   `json:"error,omitempty"`
	DurationMs float64                 `json:"duration_ms"`
}

// Store manages saved queries as a JSON file in the data directory.
// No database needed — it's just a file. Fits the ducktel philosophy.
type Store struct {
	path string
}

func NewStore(dataDir string) *Store {
	return &Store{
		path: filepath.Join(dataDir, "saved_queries.json"),
	}
}

func (s *Store) load() ([]Query, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading saved queries: %w", err)
	}
	var queries []Query
	if err := json.Unmarshal(data, &queries); err != nil {
		return nil, fmt.Errorf("parsing saved queries: %w", err)
	}
	return queries, nil
}

func (s *Store) save(queries []Query) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	data, err := json.MarshalIndent(queries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling queries: %w", err)
	}
	return os.WriteFile(s.path, data, 0644)
}

// Save creates or updates a saved query.
func (s *Store) Save(q Query) error {
	queries, err := s.load()
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	found := false
	for i, existing := range queries {
		if existing.Name == q.Name {
			q.CreatedAt = existing.CreatedAt
			q.UpdatedAt = now
			queries[i] = q
			found = true
			break
		}
	}
	if !found {
		q.CreatedAt = now
		q.UpdatedAt = now
		queries = append(queries, q)
	}

	return s.save(queries)
}

// Get retrieves a single saved query by name.
func (s *Store) Get(name string) (*Query, error) {
	queries, err := s.load()
	if err != nil {
		return nil, err
	}
	for _, q := range queries {
		if q.Name == name {
			return &q, nil
		}
	}
	return nil, fmt.Errorf("saved query %q not found", name)
}

// List returns all saved queries, sorted by name.
func (s *Store) List() ([]Query, error) {
	queries, err := s.load()
	if err != nil {
		return nil, err
	}
	sort.Slice(queries, func(i, j int) bool {
		return queries[i].Name < queries[j].Name
	})
	return queries, nil
}

// Delete removes a saved query by name.
func (s *Store) Delete(name string) error {
	queries, err := s.load()
	if err != nil {
		return err
	}

	filtered := queries[:0]
	found := false
	for _, q := range queries {
		if q.Name == name {
			found = true
			continue
		}
		filtered = append(filtered, q)
	}
	if !found {
		return fmt.Errorf("saved query %q not found", name)
	}

	return s.save(filtered)
}
