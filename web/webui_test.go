package webui

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardAssets checks that the investigate tab's module and markup are
// embedded, and that the JS test file is not shipped in the binary.
func TestDashboardAssets(t *testing.T) {
	assets := FS()

	script, err := fs.ReadFile(assets, "investigate.js")
	if err != nil {
		t.Fatalf("investigate.js is not embedded: %v", err)
	}
	if !bytes.Contains(script, []byte("paramsFromState")) {
		t.Error("investigate.js does not look like the investigate module")
	}

	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Fatalf("index.html is not embedded: %v", err)
	}
	if !bytes.Contains(index, []byte("tab-investigate")) {
		t.Error("index.html has no tab-investigate section")
	}
	if !bytes.Contains(index, []byte(`data-tab="investigate"`)) {
		t.Error("index.html has no investigate tab button")
	}

	err = fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".test.mjs") || strings.Contains(path, ".test.") {
			t.Errorf("test file %q must not be embedded", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded assets: %v", err)
	}
}
