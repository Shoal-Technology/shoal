// Package logfile stores container output as size-rotated line files and
// reads them back with tail and follow support.
//
// Each line is "<RFC3339Nano timestamp> <stream> <text>\n". Rotated files are
// named <path>.1 (newest) ... <path>.<maxFiles-1> (oldest).
package logfile

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"example.com/shoal/internal/runtime"
)

const (
	// maxLineBytes splits longer lines into several entries.
	maxLineBytes = 16 * 1024
	// pollInterval is how often a follower checks for new data.
	pollInterval = 200 * time.Millisecond
)

// Writer appends log entries to a file, rotating when it exceeds maxSize.
// It is safe for concurrent use.
type Writer struct {
	mu       sync.Mutex
	path     string
	maxSize  int64
	maxFiles int
	f        *os.File
	size     int64
}

// OpenWriter opens (or creates) path for appending.
// maxFiles is the total number of files kept, including the active one.
func OpenWriter(path string, maxSize int64, maxFiles int) (*Writer, error) {
	if maxFiles < 1 {
		maxFiles = 1
	}
	w := &Writer{path: path, maxSize: maxSize, maxFiles: maxFiles}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat log file: %w", err)
	}
	w.f, w.size = f, fi.Size()
	return nil
}

// WriteEntry appends one entry as a single line.
func (w *Writer) WriteEntry(e runtime.LogEntry) error {
	line := e.Time.UTC().Format(time.RFC3339Nano) + " " + e.Stream + " " + e.Line + "\n"
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxSize > 0 && w.size > 0 && w.size+int64(len(line)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return err
		}
	}
	n, err := io.WriteString(w.f, line)
	w.size += int64(n)
	if err != nil {
		return fmt.Errorf("write log file: %w", err)
	}
	return nil
}

func (w *Writer) rotate() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("close log file: %w", err)
	}
	if w.maxFiles == 1 {
		if err := os.Truncate(w.path, 0); err != nil {
			return fmt.Errorf("truncate log file: %w", err)
		}
		return w.open()
	}
	for i := w.maxFiles - 1; i >= 1; i-- {
		src := w.path
		if i > 1 {
			src = fmt.Sprintf("%s.%d", w.path, i-1)
		}
		if err := os.Rename(src, fmt.Sprintf("%s.%d", w.path, i)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate log file: %w", err)
		}
	}
	return w.open()
}

// Close closes the active file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// Copy reads lines from r and writes them as entries tagged with stream until
// r returns EOF.
func Copy(w *Writer, r io.Reader, stream string, now func() time.Time) error {
	br := bufio.NewReaderSize(r, maxLineBytes)
	for {
		line, isPrefix, err := br.ReadLine()
		if len(line) > 0 || (err == nil && !isPrefix) {
			if werr := w.WriteEntry(runtime.LogEntry{Time: now(), Stream: stream, Line: string(line)}); werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read %s: %w", stream, err)
		}
	}
}

// Remove deletes the active and all rotated files for path.
func Remove(path string, maxFiles int) error {
	var errs []error
	for i := 0; i < max(maxFiles, 1); i++ {
		name := path
		if i > 0 {
			name = fmt.Sprintf("%s.%d", path, i)
		}
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Read calls fn for log entries stored at path (including rotated files).
// With opts.Follow it keeps reading new entries, across rotations, until ctx
// is cancelled or stop is closed (both return nil), or fn returns an error.
// After stop is closed it reads once more, to pick up output the writer
// flushed after the process exited. A nil stop never fires.
func Read(ctx context.Context, path string, maxFiles int, opts runtime.LogOptions, stop <-chan struct{}, fn func(runtime.LogEntry) error) error {
	emit := fn
	var tail *ring
	if opts.Tail >= 0 {
		tail = newRing(opts.Tail)
		emit = func(e runtime.LogEntry) error { tail.add(e); return nil }
	}

	for i := max(maxFiles, 1) - 1; i >= 1; i-- {
		if err := readFile(fmt.Sprintf("%s.%d", path, i), emit); err != nil {
			return err
		}
	}

	f, err := os.Open(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("open log file: %w", err)
	}
	// f is replaced on rotation; close whichever file is open at the end.
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	var lr *lineReader
	if f != nil {
		lr = &lineReader{r: bufio.NewReader(f)}
		if err := lr.drain(emit); err != nil {
			return err
		}
	}

	if tail != nil {
		for _, e := range tail.entries() {
			if err := fn(e); err != nil {
				return err
			}
		}
	}
	if !opts.Follow {
		return nil
	}

	poll := func() error {
		if lr != nil {
			if err := lr.drain(fn); err != nil {
				return err
			}
		}
		// Reopen when the file was rotated (or created after we started).
		st, err := os.Stat(path)
		if err != nil {
			return nil
		}
		if f != nil {
			cur, err := f.Stat()
			if err == nil && os.SameFile(cur, st) {
				return nil
			}
			if err := lr.drain(fn); err != nil {
				return err
			}
			f.Close()
		}
		nf, err := os.Open(path)
		if err != nil {
			f, lr = nil, nil
			return nil
		}
		f, lr = nf, &lineReader{r: bufio.NewReader(nf)}
		return lr.drain(fn)
	}

	t := time.NewTicker(pollInterval)
	defer t.Stop()
	finishing := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-stop:
			stop, finishing = nil, true // one last poll on the next tick
			continue
		case <-t.C:
		}
		if err := poll(); err != nil || finishing {
			return err
		}
	}
}

func readFile(name string, fn func(runtime.LogEntry) error) error {
	f, err := os.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer f.Close()
	return (&lineReader{r: bufio.NewReader(f)}).drain(fn)
}

// lineReader yields complete lines, keeping a trailing partial line until
// the rest of it is written.
type lineReader struct {
	r       *bufio.Reader
	partial string
}

func (l *lineReader) drain(fn func(runtime.LogEntry) error) error {
	for {
		s, err := l.r.ReadString('\n')
		if err != nil {
			l.partial += s
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read log file: %w", err)
		}
		line := l.partial + s
		l.partial = ""
		if err := fn(parseLine(strings.TrimSuffix(line, "\n"))); err != nil {
			return err
		}
	}
}

func parseLine(s string) runtime.LogEntry {
	ts, rest, ok1 := strings.Cut(s, " ")
	stream, text, ok2 := strings.Cut(rest, " ")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if !ok1 || !ok2 || err != nil {
		return runtime.LogEntry{Line: s}
	}
	return runtime.LogEntry{Time: t, Stream: stream, Line: text}
}

// ring keeps the last n entries.
type ring struct {
	buf   []runtime.LogEntry
	n     int
	start int
}

func newRing(n int) *ring { return &ring{n: n} }

func (r *ring) add(e runtime.LogEntry) {
	if r.n == 0 {
		return
	}
	if len(r.buf) < r.n {
		r.buf = append(r.buf, e)
		return
	}
	r.buf[r.start] = e
	r.start = (r.start + 1) % r.n
}

func (r *ring) entries() []runtime.LogEntry {
	return append(append([]runtime.LogEntry{}, r.buf[r.start:]...), r.buf[:r.start]...)
}
