package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLogTailSendsCompleteLinesOnce checks the log plumbing of a builder:
// a line still being written waits for its newline, nothing is sent twice,
// and the password never leaves the node.
func TestLogTailSendsCompleteLinesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "build.log")
	tail := &logTail{path: path}
	now := time.Unix(1700000000, 0)
	secrets := []string{"hunter2"}

	tail.read(now, secrets) // no file yet
	if len(tail.pending) != 0 {
		t.Fatalf("pending %d lines before the log exists", len(tail.pending))
	}

	write := func(s string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	write("cloning https://bob:hunter2@h/x\n")
	write("step 1/3\r\nstep 2")
	tail.read(now, secrets)
	lines := tail.pending
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(lines), lines)
	}
	if got := lines[0].GetLine(); got != "cloning https://bob:[redacted]@h/x" {
		t.Errorf("line 0 = %q", got)
	}
	if got := lines[1].GetLine(); got != "step 1/3" {
		t.Errorf("line 1 = %q (CR must be trimmed)", got)
	}
	tail.pending = nil

	write("/3\n")
	tail.read(now, secrets)
	if len(tail.pending) != 1 || tail.pending[0].GetLine() != "step 2/3" {
		t.Fatalf("completed line = %v, want step 2/3", tail.pending)
	}
	tail.pending = nil

	tail.read(now, secrets)
	if len(tail.pending) != 0 {
		t.Fatalf("lines were sent twice: %v", tail.pending)
	}
}
