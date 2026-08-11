package pty

import (
	"syscall"
	"testing"
	"unsafe"
)

// TestFdSetRoundTripsEveryDescriptor pins the fd_set bit math against the
// platform's actual FdSet layout, across the descriptor range boss really uses.
//
// This is not a theoretical range. A live boss TUI holds ~30 descriptors
// numbered above 32 — one pty master per backgrounded attach, plus sockets and
// pipes — so the cancel pipe of a new attach routinely lands well past 32.
//
// The bug this catches: the helpers indexed Bits as if its elements were 64 bits
// wide (`Bits[fd/64]`, `1 << (fd%64)`), which is true on Linux but NOT on
// Darwin, where FdSet is `Bits [32]int32`. For fd >= 32 the shift count reaches
// the element width, and Go defines such a shift as zero — so fdSet silently
// became a no-op and fdIsSet always reported false. Nothing errored; the
// descriptor was simply never watched, which for the cancel pipe means the
// cancellation signal could never wake select and teardown blocked forever in
// wg.Wait().
//
// Deriving the element width from the type rather than hardcoding it keeps this
// honest on both platforms.
func TestFdSetRoundTripsEveryDescriptor(t *testing.T) {
	var probe syscall.FdSet
	bitsPerWord := 8 * int(unsafe.Sizeof(probe.Bits[0]))
	capacity := len(probe.Bits) * bitsPerWord
	t.Logf("platform FdSet: %d words x %d bits = %d descriptors", len(probe.Bits), bitsPerWord, capacity)

	// Every descriptor the set can hold must round-trip, with 32/33/63/64 called
	// out because those are exactly where the 64-bit assumption broke on Darwin.
	for _, fd := range []int{0, 1, 2, 31, 32, 33, 63, 64, 65, 127, capacity - 1} {
		if fd >= capacity {
			continue
		}
		var set syscall.FdSet
		fdSet(&set, fd)
		if !fdIsSet(&set, fd) {
			t.Errorf("fd %d: set then IsSet reported false — the descriptor would never be watched", fd)
		}
	}
}

// TestFdSetDoesNotAliasOtherDescriptors catches the other half of the same
// mistake: with the wrong word width, a high descriptor's bit can land in the
// word belonging to a DIFFERENT descriptor, so select would watch an unrelated
// open file instead. That fails toward a spurious wakeup rather than a hang, and
// is just as wrong.
func TestFdSetDoesNotAliasOtherDescriptors(t *testing.T) {
	var probe syscall.FdSet
	bitsPerWord := 8 * int(unsafe.Sizeof(probe.Bits[0]))
	capacity := len(probe.Bits) * bitsPerWord

	for _, fd := range []int{0, 32, 33, 63, 64, 100} {
		if fd >= capacity {
			continue
		}
		var set syscall.FdSet
		fdSet(&set, fd)
		for other := 0; other < capacity; other++ {
			if other == fd {
				continue
			}
			if fdIsSet(&set, other) {
				t.Fatalf("setting fd %d also marked fd %d — select would watch the wrong descriptor", fd, other)
			}
		}
	}
}

// TestFdSetIgnoresOutOfRange guards the bounds check. An fd at or past the set's
// capacity cannot be represented; writing it would corrupt adjacent memory, so
// the helpers must decline rather than index past the end.
func TestFdSetIgnoresOutOfRange(t *testing.T) {
	var probe syscall.FdSet
	bitsPerWord := 8 * int(unsafe.Sizeof(probe.Bits[0]))
	capacity := len(probe.Bits) * bitsPerWord

	for _, fd := range []int{-1, capacity, capacity + 1, capacity * 2} {
		var set syscall.FdSet
		fdSet(&set, fd) // must not panic or corrupt
		if fdIsSet(&set, fd) {
			t.Errorf("fd %d is outside the set's %d-descriptor capacity but reported as set", fd, capacity)
		}
		for i := range set.Bits {
			if set.Bits[i] != 0 {
				t.Fatalf("fd %d wrote into word %d despite being out of range", fd, i)
			}
		}
	}
}
