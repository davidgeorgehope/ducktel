package savedquery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// MCPSavedQuery is a tenant-scoped saved MCP tool invocation.
// It lives in a separate file from SQL saved queries (saved_queries.json).
type MCPSavedQuery struct {
	Tenant      string          `json:"tenant"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Tool        string          `json:"tool"`
	Arguments   json.RawMessage `json:"arguments"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// MCPStore persists saved MCP queries under the data directory.
//
// HTTP mode builds a new store for every session, so the lock is keyed by
// the file path and shared by every store in the process. An instance mutex
// would let two saves read the same file and the later write would drop the
// earlier record.
type MCPStore struct {
	path string
}

// fileMu serializes read-modify-write of one mcp_saved_queries.json.
var fileMu sync.Map

func (s *MCPStore) lock() *sync.Mutex {
	key, err := filepath.Abs(s.path)
	if err != nil {
		key = s.path
	}
	v, _ := fileMu.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// NewMCPStore opens the tenant-scoped MCP saved-query file in dataDir.
func NewMCPStore(dataDir string) *MCPStore {
	return &MCPStore{path: filepath.Join(dataDir, "mcp_saved_queries.json")}
}

func (s *MCPStore) load() ([]MCPSavedQuery, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading mcp saved queries: %w", err)
	}
	var queries []MCPSavedQuery
	if err := json.Unmarshal(data, &queries); err != nil {
		return nil, fmt.Errorf("parsing mcp saved queries: %w", err)
	}
	return queries, nil
}

func (s *MCPStore) saveAll(queries []MCPSavedQuery) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	data, err := json.Marshal(queries)
	if err != nil {
		return fmt.Errorf("marshaling mcp saved queries: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "mcp_saved_queries-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	return os.Rename(tmpPath, s.path)
}

// Save inserts or replaces one query for tenant+name. created_at is preserved on update.
func (s *MCPStore) Save(q MCPSavedQuery) error {
	mu := s.lock()
	mu.Lock()
	defer mu.Unlock()

	queries, err := s.load()
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	found := false
	for i, existing := range queries {
		if existing.Tenant == q.Tenant && existing.Name == q.Name {
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

	return s.saveAll(queries)
}

// Get returns one saved query for the tenant and name, or an error if missing.
func (s *MCPStore) Get(tenant, name string) (*MCPSavedQuery, error) {
	mu := s.lock()
	mu.Lock()
	defer mu.Unlock()

	queries, err := s.load()
	if err != nil {
		return nil, err
	}
	for _, q := range queries {
		if q.Tenant == tenant && q.Name == name {
			copy := q
			return &copy, nil
		}
	}
	return nil, fmt.Errorf("saved query %q not found", name)
}

// List returns saved queries for one tenant, sorted by name.
func (s *MCPStore) List(tenant string) ([]MCPSavedQuery, error) {
	mu := s.lock()
	mu.Lock()
	defer mu.Unlock()

	queries, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []MCPSavedQuery
	for _, q := range queries {
		if q.Tenant == tenant {
			out = append(out, q)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Names returns sorted query names for one tenant.
func (s *MCPStore) Names(tenant string) ([]string, error) {
	list, err := s.List(tenant)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(list))
	for i, q := range list {
		names[i] = q.Name
	}
	return names, nil
}
