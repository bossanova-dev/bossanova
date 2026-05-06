package agent

import (
	"fmt"
	"testing"
)

// TestRingBufferOverflow verifies that the oldest entries are evicted when
// the ring buffer exceeds its capacity.
func TestRingBufferOverflow(t *testing.T) {
	rb := newRingBuffer(5) // Small buffer for testing.

	// Write 8 entries (3 more than capacity).
	for i := 0; i < 8; i++ {
		rb.add(OutputLine{Text: fmt.Sprintf("line-%d", i)})
	}

	lines := rb.lines()
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}

	// Should have lines 3-7 (oldest 0-2 evicted).
	for i, line := range lines {
		expected := fmt.Sprintf("line-%d", i+3)
		if line.Text != expected {
			t.Errorf("line %d: got %q, want %q", i, line.Text, expected)
		}
	}
}

// TestRingBufferUnderflow verifies partial buffers return correct results.
func TestRingBufferUnderflow(t *testing.T) {
	rb := newRingBuffer(100)

	rb.add(OutputLine{Text: "a"})
	rb.add(OutputLine{Text: "b"})
	rb.add(OutputLine{Text: "c"})

	lines := rb.lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	if lines[0].Text != "a" || lines[1].Text != "b" || lines[2].Text != "c" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

// TestRingBufferEmpty verifies empty buffer returns nil.
func TestRingBufferEmpty(t *testing.T) {
	rb := newRingBuffer(10)
	lines := rb.lines()
	if lines != nil {
		t.Errorf("expected nil for empty buffer, got %v", lines)
	}
}

// TestRingBufferExactCapacity verifies buffer at exact capacity.
func TestRingBufferExactCapacity(t *testing.T) {
	rb := newRingBuffer(3)

	rb.add(OutputLine{Text: "x"})
	rb.add(OutputLine{Text: "y"})
	rb.add(OutputLine{Text: "z"})

	lines := rb.lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	if lines[0].Text != "x" || lines[1].Text != "y" || lines[2].Text != "z" {
		t.Errorf("unexpected: %v", lines)
	}
}

// TestRingBuffer1000Overflow verifies the default buffer size behavior.
func TestRingBuffer1000Overflow(t *testing.T) {
	rb := newRingBuffer(DefaultRingBufferSize)

	// Write 1200 entries.
	for i := 0; i < 1200; i++ {
		rb.add(OutputLine{Text: fmt.Sprintf("entry-%d", i)})
	}

	lines := rb.lines()
	if len(lines) != DefaultRingBufferSize {
		t.Fatalf("expected %d lines, got %d", DefaultRingBufferSize, len(lines))
	}

	// Oldest should be entry-200, newest should be entry-1199.
	if lines[0].Text != "entry-200" {
		t.Errorf("oldest: got %q, want %q", lines[0].Text, "entry-200")
	}
	if lines[len(lines)-1].Text != "entry-1199" {
		t.Errorf("newest: got %q, want %q", lines[len(lines)-1].Text, "entry-1199")
	}
}
