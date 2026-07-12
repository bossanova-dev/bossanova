package tuidriver

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// syncBuffer is a thread-safe io.Writer wrapper so the readLoop goroutine and
// the test goroutine don't race on the underlying buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// scriptedPTY returns a zero-byte successful read before EOF. Although an
// *os.File normally blocks rather than returning this result, io.Reader permits
// it; readLoop must not forward a non-existent chunk to RawOutput.
type scriptedPTY struct {
	reads []struct {
		n   int
		err error
	}
}

func (p *scriptedPTY) Read(_ []byte) (int, error) {
	if len(p.reads) == 0 {
		return 0, io.EOF
	}
	r := p.reads[0]
	p.reads = p.reads[1:]
	return r.n, r.err
}

func (p *scriptedPTY) Write(b []byte) (int, error) { return len(b), nil }
func (p *scriptedPTY) Close() error                { return nil }

type writeCallRecorder struct{ calls []int }

func (r *writeCallRecorder) Write(b []byte) (int, error) {
	r.calls = append(r.calls, len(b))
	return len(b), nil
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestRawOutputTee verifies that when Options.RawOutput is set, the raw PTY
// bytes are teed to that writer in addition to feeding the VT emulator.
func TestRawOutputTee(t *testing.T) {
	raw := &syncBuffer{}
	d, err := New(Options{
		Command:   "/bin/sh",
		Args:      []string{"-c", "printf hello"},
		Width:     20,
		Height:    5,
		RawOutput: raw,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close() }()

	// Wait for the process output to flow through (the child exits quickly).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(raw.String(), "hello") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := raw.String(); !strings.Contains(got, "hello") {
		t.Fatalf("RawOutput did not capture PTY bytes; got %q", got)
	}
}

// TestReadLoopDoesNotForwardEmptySuccessfulRead verifies that a zero-byte,
// nil-error read is ignored. This is the exact boundary for `if n > 0`: the
// >= mutant would call RawOutput.Write with an empty chunk.
func TestReadLoopDoesNotForwardEmptySuccessfulRead(t *testing.T) {
	raw := &writeCallRecorder{}
	d := &Driver{
		pty: &scriptedPTY{reads: []struct {
			n   int
			err error
		}{{n: 0, err: nil}, {err: io.EOF}}},
		vt:     vt.NewEmulator(80, 24),
		rawOut: raw,
		done:   make(chan struct{}),
	}

	d.readLoop()

	if len(raw.calls) != 0 {
		t.Fatalf("RawOutput received empty successful read: write sizes %v", raw.calls)
	}
}
