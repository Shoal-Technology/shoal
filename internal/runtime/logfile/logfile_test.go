package logfile

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kaiserern/shoal/internal/runtime"
)

func writeLines(t *testing.T, w *Writer, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		e := runtime.LogEntry{Time: time.Unix(int64(i), 0), Stream: "stdout", Line: fmt.Sprintf("line-%03d", i)}
		if err := w.WriteEntry(e); err != nil {
			t.Fatal(err)
		}
	}
}

func collect(t *testing.T, path string, maxFiles int, opts runtime.LogOptions) []string {
	t.Helper()
	var got []string
	err := Read(context.Background(), path, maxFiles, opts, nil, func(e runtime.LogEntry) error {
		got = append(got, e.Line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func want(from, to int) []string {
	var s []string
	for i := from; i < to; i++ {
		s = append(s, fmt.Sprintf("line-%03d", i))
	}
	return s
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	w, err := OpenWriter(path, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ts := time.Date(2026, 10, 8, 1, 2, 3, 4, time.UTC)
	if err := w.WriteEntry(runtime.LogEntry{Time: ts, Stream: "stderr", Line: "hello world"}); err != nil {
		t.Fatal(err)
	}
	var got []runtime.LogEntry
	if err := Read(context.Background(), path, 3, runtime.LogOptions{Tail: -1}, nil, func(e runtime.LogEntry) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantEntry := runtime.LogEntry{Time: ts, Stream: "stderr", Line: "hello world"}
	if len(got) != 1 || !got[0].Time.Equal(ts) || got[0].Stream != wantEntry.Stream || got[0].Line != wantEntry.Line {
		t.Fatalf("got %+v, want %+v", got, wantEntry)
	}
}

func TestRotationAndTail(t *testing.T) {
	// Each line is 37 bytes; a 200-byte limit keeps 5 lines per file, so 20
	// lines produce 4 files of which maxFiles are retained.
	tests := []struct {
		name     string
		maxFiles int
		tail     int
		want     []string
	}{
		{name: "all across 3 files", maxFiles: 3, tail: -1, want: want(5, 20)},
		{name: "tail spans files", maxFiles: 3, tail: 7, want: want(13, 20)},
		{name: "tail zero", maxFiles: 3, tail: 0, want: nil},
		{name: "single file truncates", maxFiles: 1, tail: -1, want: want(15, 20)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.log")
			w, err := OpenWriter(path, 200, tt.maxFiles)
			if err != nil {
				t.Fatal(err)
			}
			writeLines(t, w, 0, 20)
			w.Close()
			got := collect(t, path, tt.maxFiles, runtime.LogOptions{Tail: tt.tail})
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v\nwant %v", got, tt.want)
			}
		})
	}
}

func TestCopySplitsLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	w, err := OpenWriter(path, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", maxLineBytes+10)
	in := "a\n\nb\r\n" + long + "\nno-newline"
	if err := Copy(w, strings.NewReader(in), "stdout", time.Now); err != nil {
		t.Fatal(err)
	}
	w.Close()
	got := collect(t, path, 1, runtime.LogOptions{Tail: -1})
	wantLines := []string{"a", "", "b", long[:maxLineBytes], long[maxLineBytes:], "no-newline"}
	if !slices.Equal(got, wantLines) {
		t.Errorf("got %d lines %q..., want %d lines", len(got), got[:min(3, len(got))], len(wantLines))
	}
}

func TestFollowAcrossRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	w, err := OpenWriter(path, 200, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	writeLines(t, w, 0, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu  sync.Mutex
		got []string
	)
	done := make(chan error, 1)
	go func() {
		done <- Read(ctx, path, 3, runtime.LogOptions{Follow: true, Tail: -1}, nil, func(e runtime.LogEntry) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, e.Line)
			return nil
		})
	}()

	// Write slowly enough that the follower observes each rotation.
	for i := 2; i < 10; i++ {
		time.Sleep(3 * pollInterval / 2)
		writeLines(t, w, i, i+1)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 10 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, want(0, 10)) {
		t.Errorf("got %v, want %v", got, want(0, 10))
	}
}

// TestFollowEndsOnStop checks that closing stop ends a follow, and that output
// written just before (as a log driver flushes on exit) is still delivered.
func TestFollowEndsOnStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	w, err := OpenWriter(path, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	writeLines(t, w, 0, 2)

	stop := make(chan struct{})
	var got []string
	done := make(chan error, 1)
	go func() {
		done <- Read(context.Background(), path, 1, runtime.LogOptions{Follow: true, Tail: -1}, stop, func(e runtime.LogEntry) error {
			got = append(got, e.Line)
			return nil
		})
	}()
	time.Sleep(pollInterval)
	writeLines(t, w, 2, 4)
	close(stop)

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not end after stop was closed")
	}
	if !slices.Equal(got, want(0, 4)) {
		t.Errorf("got %v, want %v", got, want(0, 4))
	}
}

func TestRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	w, err := OpenWriter(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	writeLines(t, w, 0, 10)
	w.Close()
	if err := Remove(path, 3); err != nil {
		t.Fatal(err)
	}
	if got := collect(t, path, 3, runtime.LogOptions{Tail: -1}); len(got) != 0 {
		t.Errorf("expected no entries after Remove, got %v", got)
	}
	if err := Remove(path, 3); err != nil {
		t.Errorf("second Remove should be a no-op: %v", err)
	}
}
