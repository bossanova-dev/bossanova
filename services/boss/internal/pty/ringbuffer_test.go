package pty

import (
	"bytes"
	"testing"
)

func TestRingBuffer_WriteAndRead(t *testing.T) {
	rb := NewRingBuffer(8)

	// Write less than capacity.
	_, _ = rb.Write([]byte("hello"))
	got := rb.Bytes()
	if !bytes.Equal(got, []byte("hello")) {
		t.Fatalf("expected %q, got %q", "hello", got)
	}
}

func TestRingBuffer_WrapAround(t *testing.T) {
	rb := NewRingBuffer(8)

	_, _ = rb.Write([]byte("abcdefgh")) // fills exactly
	_, _ = rb.Write([]byte("ij"))       // wraps

	got := rb.Bytes()
	want := []byte("cdefghij")
	if !bytes.Equal(got, want) {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestRingBuffer_OverflowLargerThanBuffer(t *testing.T) {
	rb := NewRingBuffer(4)

	_, _ = rb.Write([]byte("abcdefghij")) // 10 bytes into 4-byte buffer

	got := rb.Bytes()
	want := []byte("ghij") // only last 4 bytes kept
	if !bytes.Equal(got, want) {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestRingBuffer_Reset(t *testing.T) {
	rb := NewRingBuffer(8)
	_, _ = rb.Write([]byte("hello"))
	rb.Reset()

	got := rb.Bytes()
	if len(got) != 0 {
		t.Fatalf("expected empty after reset, got %q", got)
	}
}

func TestRingBuffer_DefaultSize(t *testing.T) {
	if defaultBufSize != 512*1024 {
		t.Fatalf("expected defaultBufSize=512KB, got %d", defaultBufSize)
	}
}

func TestRingBuffer_MultipleSmallWrites(t *testing.T) {
	rb := NewRingBuffer(8)

	_, _ = rb.Write([]byte("ab"))
	_, _ = rb.Write([]byte("cd"))
	_, _ = rb.Write([]byte("ef"))
	_, _ = rb.Write([]byte("gh"))
	// Buffer is now full: "abcdefgh"
	_, _ = rb.Write([]byte("ij"))
	// Should be "cdefghij"

	got := rb.Bytes()
	want := []byte("cdefghij")
	if !bytes.Equal(got, want) {
		t.Fatalf("expected %q, got %q", want, got)
	}
}
