package writer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LockFileName is the advisory lock file a writer holds inside its data
// directory for as long as it runs.
const LockFileName = ".ducktel.lock"

// ErrLocked is returned by Lock when another process already holds the data
// directory's writer lock.
var ErrLocked = errors.New("data directory is locked by another writer")

// Lock claims exclusive write access to dataDir. ducktel is a single-writer
// design: two writers on one directory race on per-minute file names and both
// run compaction and retention over the same days, silently losing telemetry.
// Failing fast at startup turns that misconfiguration into a clear error.
//
// The lock is advisory and held via the open file descriptor, so the kernel
// releases it when the process exits, crash included; a leftover lock file is
// harmless. Readers (query, mcp) never take it. Call the returned release
// function on shutdown.
func Lock(dataDir string) (release func() error, err error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	path := filepath.Join(dataDir, LockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, fmt.Errorf("%s: %w (is another `ducktel serve` running with this --data-dir?)", dataDir, ErrLocked)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	// Record the holder for whoever inspects the file; informational only.
	if err := f.Truncate(0); err == nil {
		fmt.Fprintf(f, "%d\n", os.Getpid())
	}
	return func() error {
		unlockFile(f)
		return f.Close()
	}, nil
}
