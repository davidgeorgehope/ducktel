package writer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- ParseRetention ---

func TestParseRetentionEmptyDisables(t *testing.T) {
	d, err := ParseRetention("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 0 {
		t.Errorf("empty string should parse to 0 (disabled), got %v", d)
	}
}

func TestParseRetentionDaysShorthand(t *testing.T) {
	d, err := ParseRetention("30d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := 30 * 24 * time.Hour; d != want {
		t.Errorf("ParseRetention(30d) = %v, want %v", d, want)
	}
}

func TestParseRetentionPlainGoDuration(t *testing.T) {
	d, err := ParseRetention("720h")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := 720 * time.Hour; d != want {
		t.Errorf("ParseRetention(720h) = %v, want %v", d, want)
	}
}

func TestParseRetentionRejectsGarbage(t *testing.T) {
	for _, s := range []string{"abc", "7x", "d", "-5d", "-1h"} {
		if _, err := ParseRetention(s); err == nil {
			t.Errorf("ParseRetention(%q) should error", s)
		}
	}
}

// --- pruneOldData ---

func mkDateDir(t *testing.T, dataDir, signal, date string) string {
	t.Helper()
	dir := filepath.Join(dataDir, signal, date)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "00-00.parquet"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPruneOldDataRemovesOnlyOldDirectories(t *testing.T) {
	dir := t.TempDir()
	old := mkDateDir(t, dir, "traces", time.Now().UTC().Add(-40*24*time.Hour).Format("2006-01-02"))
	recent := mkDateDir(t, dir, "traces", time.Now().UTC().Format("2006-01-02"))

	w := New(dir, time.Hour, 1000)
	w.SetRetention(30 * 24 * time.Hour)
	if err := w.pruneOldData(); err != nil {
		t.Fatalf("pruneOldData: %v", err)
	}

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old directory should have been removed: %s", old)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent directory should still exist: %v", err)
	}
}

func TestPruneOldDataDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	old := mkDateDir(t, dir, "traces", time.Now().UTC().Add(-400*24*time.Hour).Format("2006-01-02"))

	w := New(dir, time.Hour, 1000) // retention never set — must default to disabled
	if err := w.pruneOldData(); err != nil {
		t.Fatalf("pruneOldData: %v", err)
	}

	if _, err := os.Stat(old); err != nil {
		t.Errorf("retention disabled should never delete anything, but: %v", err)
	}
}

func TestPruneOldDataSkipsNonDateDirectories(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "traces", "not-a-date")
	if err := os.MkdirAll(garbage, 0o755); err != nil {
		t.Fatal(err)
	}

	w := New(dir, time.Hour, 1000)
	w.SetRetention(time.Hour) // aggressive — would delete "old" if it were checked
	if err := w.pruneOldData(); err != nil {
		t.Fatalf("pruneOldData: %v", err)
	}

	if _, err := os.Stat(garbage); err != nil {
		t.Errorf("non-date directory must never be touched: %v", err)
	}
}

func TestPruneOldDataHandlesMissingSignalDirs(t *testing.T) {
	dir := t.TempDir() // nothing written yet — no traces/logs/metrics dirs exist

	w := New(dir, time.Hour, 1000)
	w.SetRetention(24 * time.Hour)
	if err := w.pruneOldData(); err != nil {
		t.Errorf("missing signal directories should not be an error: %v", err)
	}
}

func TestPruneOldDataSweepsAllThreeSignals(t *testing.T) {
	dir := t.TempDir()
	oldDate := time.Now().UTC().Add(-40 * 24 * time.Hour).Format("2006-01-02")
	traces := mkDateDir(t, dir, "traces", oldDate)
	logs := mkDateDir(t, dir, "logs", oldDate)
	metrics := mkDateDir(t, dir, "metrics", oldDate)

	w := New(dir, time.Hour, 1000)
	w.SetRetention(30 * 24 * time.Hour)
	if err := w.pruneOldData(); err != nil {
		t.Fatalf("pruneOldData: %v", err)
	}

	for _, p := range []string{traces, logs, metrics} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed", p)
		}
	}
}
