//go:build unix

package writer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLockIsExclusiveAndReleasable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data") // not created yet: Lock must create it

	release, err := Lock(dir)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	// flock locks belong to the open file description, so a second open in
	// the same process conflicts exactly like a second process would.
	if _, err := Lock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock while held: got %v, want ErrLocked", err)
	}

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	release2, err := Lock(dir)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	release2()
}

// TestLockConflictsAcrossProcesses holds the lock in a child process (flock(1)
// from util-linux) and checks this process is refused, then admitted once the
// child exits — the kernel releases the lock with the holder.
func TestLockConflictsAcrossProcesses(t *testing.T) {
	flock, err := exec.LookPath("flock")
	if err != nil {
		t.Skip("flock(1) not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, LockFileName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// The child takes the lock, then blocks reading stdin until we close it.
	cmd := exec.Command(flock, "-x", path, "sh", "-c", "echo held; read _ || true")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := stdout.Read(buf); err != nil {
		t.Fatalf("waiting for child to take the lock: %v", err)
	}

	if _, err := Lock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("Lock while another process holds it: got %v, want ErrLocked", err)
	}

	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v", err)
	}

	release, err := Lock(dir)
	if err != nil {
		t.Fatalf("Lock after holder exited: %v", err)
	}
	release()
}
