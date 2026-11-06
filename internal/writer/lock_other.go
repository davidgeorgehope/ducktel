//go:build !unix

package writer

import "os"

// No advisory locking on this platform: the single-writer rule is then only
// documented, not enforced.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) {}
