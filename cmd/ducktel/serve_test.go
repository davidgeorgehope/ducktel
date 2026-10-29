package main

import (
	"strings"
	"testing"
)

// TestServeRejectsNonPositiveBufferSize pins the flag validation: a
// zero/negative buffer size would silently disable the threshold flush and
// leave only the timer, which is a configuration mistake worth failing on
// loudly rather than accepting.
func TestServeRejectsNonPositiveBufferSize(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		cmd := serveCmd()
		cmd.SetArgs([]string{"--buffer-size", v})
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("--buffer-size %s should be rejected", v)
		}
		if !strings.Contains(err.Error(), "buffer-size") {
			t.Errorf("error should name the flag, got: %v", err)
		}
	}
}
