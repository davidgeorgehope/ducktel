package savedquery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMCPStoreSaveListGet(t *testing.T) {
	store := NewMCPStore(t.TempDir())
	args := json.RawMessage(`{"tenant":"acme","service_name":"api"}`)
	if err := store.Save(MCPSavedQuery{
		Tenant: "acme", Name: "worker-spans", Tool: "span_search", Arguments: args,
		Description: "d",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get("acme", "worker-spans")
	if err != nil {
		t.Fatal(err)
	}
	if got.Tool != "span_search" || string(got.Arguments) != string(args) {
		t.Errorf("round-trip: %+v", got)
	}

	list, err := store.List("acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "worker-spans" {
		t.Fatalf("list = %+v", list)
	}
	listOther, err := store.List("other")
	if err != nil {
		t.Fatal(err)
	}
	if len(listOther) != 0 {
		t.Errorf("other tenant should be empty, got %+v", listOther)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.path), "saved_queries.json")); !os.IsNotExist(err) {
		t.Fatalf("MCP save wrote the SQL saved-query file: %v", err)
	}
}

func TestMCPStoreUpsertPreservesCreatedAt(t *testing.T) {
	store := NewMCPStore(t.TempDir())
	args1 := json.RawMessage(`{"tenant":"acme"}`)
	if err := store.Save(MCPSavedQuery{Tenant: "acme", Name: "q", Tool: "list_services", Arguments: args1}); err != nil {
		t.Fatal(err)
	}
	first, err := store.Get("acme", "q")
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Millisecond)
	args2 := json.RawMessage(`{"tenant":"acme","source":"logs"}`)
	if err := store.Save(MCPSavedQuery{Tenant: "acme", Name: "q", Tool: "list_services", Arguments: args2}); err != nil {
		t.Fatal(err)
	}
	list, err := store.List("acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want one record, got %d", len(list))
	}
	if !list[0].CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt changed")
	}
	if !list[0].UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("UpdatedAt did not advance")
	}
	if string(list[0].Arguments) != string(args2) {
		t.Errorf("arguments not updated")
	}
}

func TestMCPStoreAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	store := NewMCPStore(dir)
	path := filepath.Join(dir, "mcp_saved_queries.json")
	args := json.RawMessage(`{"tenant":"acme"}`)
	if err := store.Save(MCPSavedQuery{Tenant: "acme", Name: "q", Tool: "list_services", Arguments: args}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []MCPSavedQuery
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("file not valid JSON: %v", err)
	}
}

func TestMCPStoreConcurrentSave(t *testing.T) {
	store := NewMCPStore(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := string(rune('a' + n))
			args := json.RawMessage(`{"tenant":"acme"}`)
			if err := store.Save(MCPSavedQuery{Tenant: "acme", Name: name, Tool: "list_services", Arguments: args}); err != nil {
				t.Errorf("Save %s: %v", name, err)
			}
		}(i)
	}
	wg.Wait()
	list, err := store.List("acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 8 {
		t.Errorf("got %d records, want 8", len(list))
	}
}

func TestSeparateStoresDoNotDropSaves(t *testing.T) {
	dir := t.TempDir()
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := "acme"
			if i%2 == 1 {
				tenant = "other"
			}
			store := NewMCPStore(dir)
			name := fmt.Sprintf("q%02d", i)
			args := json.RawMessage(`{"tenant":"` + tenant + `"}`)
			if err := store.Save(MCPSavedQuery{Tenant: tenant, Name: name, Tool: "list_services", Arguments: args}); err != nil {
				t.Errorf("Save %s: %v", name, err)
			}
		}(i)
	}
	wg.Wait()

	store := NewMCPStore(dir)
	acme, err := store.List("acme")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.List("other")
	if err != nil {
		t.Fatal(err)
	}
	if len(acme) != n/2 || len(other) != n/2 {
		t.Fatalf("acme=%d other=%d, want %d each", len(acme), len(other), n/2)
	}
}
