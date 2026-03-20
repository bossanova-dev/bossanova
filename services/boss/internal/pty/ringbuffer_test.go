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

func TestRingBuffer_Tail_Basic(t *testing.T) {
	rb := NewRingBuffer(8)
	_, _ = rb.Write([]byte("hello"))

	got := rb.Tail(3)
	if !bytes.Equal(got, []byte("llo")) {
		t.Fatalf("expected %q, got %q", "llo", got)
	}
}

func TestRingBuffer_Tail_MoreThanWritten(t *testing.T) {
	rb := NewRingBuffer(8)
	_, _ = rb.Write([]byte("hi"))

	got := rb.Tail(10)
	if !bytes.Equal(got, []byte("hi")) {
		t.Fatalf("expected %q, got %q", "hi", got)
	}
}

func TestRingBuffer_Tail_Empty(t *testing.T) {
	rb := NewRingBuffer(8)
	got := rb.Tail(5)
	if got != nil {
		t.Fatalf("expected nil, got %q", got)
	}
}

func TestRingBuffer_Tail_AfterWrap(t *testing.T) {
	rb := NewRingBuffer(8)
	_, _ = rb.Write([]byte("abcdefgh")) // fills exactly
	_, _ = rb.Write([]byte("ij"))       // wraps: buffer is "ijcdefgh", pos=2

	got := rb.Tail(4)
	want := []byte("ghij")
	if !bytes.Equal(got, want) {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestRingBuffer_Tail_EntireBuffer(t *testing.T) {
	rb := NewRingBuffer(8)
	_, _ = rb.Write([]byte("abcdefgh"))
	_, _ = rb.Write([]byte("ij"))

	// Tail of full buffer size should return all content in order.
	got := rb.Tail(8)
	want := []byte("cdefghij")
	if !bytes.Equal(got, want) {
		t.Fatalf("expected %q, got %q", want, got)
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
