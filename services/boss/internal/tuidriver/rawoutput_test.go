package tuidriver

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a thread-safe io.Writer wrapper so the readLoop goroutine and
// the test goroutine don't race on the underlying buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
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
