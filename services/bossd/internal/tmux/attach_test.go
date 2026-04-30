package tmux

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ─── ring buffer unit tests ──────────────────────────────────────────────

func TestRingBuffer_BasicWriteRead(t *testing.T) {
	r := newRingBuffer(64)
	r.Write([]byte("hello"))
	if r.Len() != 5 {
		t.Fatalf("Len=%d, want 5", r.Len())
	}
	data, lost := r.ReadChunk(64)
	if lost {
		t.Errorf("lost=true on a fresh buffer")
	}
	if string(data) != "hello" {
		t.Errorf("got %q, want %q", data, "hello")
	}
	if r.Len() != 0 {
		t.Errorf("Len after read = %d, want 0", r.Len())
	}
}

func TestRingBuffer_EmptyReadReturnsNil(t *testing.T) {
	r := newRingBuffer(8)
	data, lost := r.ReadChunk(64)
	if data != nil || lost {
		t.Errorf("ReadChunk on empty buffer = (%v, %v), want (nil, false)", data, lost)
	}
}

func TestRingBuffer_DropOldestSetsLostFlag(t *testing.T) {
	r := newRingBuffer(8)
	r.Write([]byte("AAAAAAAA")) // fill the buffer (8 bytes)
	r.Write([]byte("BBBB"))     // 4 bytes more — drops 4 oldest

	if r.Len() != 8 {
		t.Fatalf("Len=%d, want 8", r.Len())
	}
	data, lost := r.ReadChunk(64)
	if !lost {
		t.Errorf("lost=false after overflow, want true")
	}
	if string(data) != "AAAABBBB" {
		t.Errorf("got %q, want %q", data, "AAAABBBB")
	}
}

func TestRingBuffer_LostFlagDoesNotPersistPastOneEmit(t *testing.T) {
	r := newRingBuffer(4)
	r.Write([]byte("ABCD"))
	r.Write([]byte("EF")) // drops 2 oldest -> lost=true

	data, lost := r.ReadChunk(2)
	if !lost {
		t.Errorf("first ReadChunk: lost=false, want true")
	}
	if string(data) != "CD" {
		t.Errorf("first data=%q, want %q", data, "CD")
	}

	// Next chunk should NOT carry the lost flag.
	data, lost = r.ReadChunk(2)
	if lost {
		t.Errorf("second ReadChunk: lost=true, want false (flag must not persist)")
	}
	if string(data) != "EF" {
		t.Errorf("second data=%q, want %q", data, "EF")
	}
}

func TestRingBuffer_ExactSizeWriteIntoEmptyBufferDoesNotSetLost(t *testing.T) {
	// Regression: a single Write of exactly r.size into an empty buffer
	// fits without dropping anything, so Lost on the next ReadChunk MUST
	// be false. Previous behaviour falsely set lost=true and forced an
	// unnecessary RESYNC upstream.
	r := newRingBuffer(8)
	r.Write([]byte("ABCDEFGH")) // exactly cap, count was 0 → no drop
	if r.Len() != 8 {
		t.Fatalf("Len=%d, want 8", r.Len())
	}
	data, lost := r.ReadChunk(64)
	if lost {
		t.Errorf("lost=true on exact-size write into empty buffer, want false")
	}
	if string(data) != "ABCDEFGH" {
		t.Errorf("got %q, want %q", data, "ABCDEFGH")
	}
}

func TestRingBuffer_ExactSizeWriteIntoNonEmptyBufferSetsLost(t *testing.T) {
	// Companion to the above: when the buffer already has bytes, an
	// exact-size write DOES drop them, so Lost must be true.
	r := newRingBuffer(8)
	r.Write([]byte("XX"))       // 2 bytes buffered
	r.Write([]byte("ABCDEFGH")) // overwrites everything
	data, lost := r.ReadChunk(64)
	if !lost {
		t.Errorf("lost=false after exact-size write that overwrote prior bytes, want true")
	}
	if string(data) != "ABCDEFGH" {
		t.Errorf("got %q, want %q", data, "ABCDEFGH")
	}
}

func TestRingBuffer_WriteLargerThanCapacityKeepsTail(t *testing.T) {
	r := newRingBuffer(4)
	// 10 bytes into a 4-byte buffer — only the last 4 survive.
	r.Write([]byte("0123456789"))
	if r.Len() != 4 {
		t.Fatalf("Len=%d, want 4", r.Len())
	}
	data, lost := r.ReadChunk(64)
	if !lost {
		t.Errorf("lost=false, want true")
	}
	if string(data) != "6789" {
		t.Errorf("got %q, want %q", data, "6789")
	}
}

func TestRingBuffer_WrapAround(t *testing.T) {
	r := newRingBuffer(8)
	r.Write([]byte("ABCDEF")) // 6 bytes
	got, _ := r.ReadChunk(4)  // consume 4
	if string(got) != "ABCD" {
		t.Fatalf("phase1 got %q", got)
	}
	r.Write([]byte("GHIJKL")) // wraps; total in buffer = 2 + 6 = 8
	if r.Len() != 8 {
		t.Fatalf("Len=%d, want 8", r.Len())
	}
	got, lost := r.ReadChunk(64)
	if lost {
		t.Errorf("unexpected lost=true (wrap, no overflow)")
	}
	if string(got) != "EFGHIJKL" {
		t.Errorf("got %q, want %q", got, "EFGHIJKL")
	}
}

func TestRingBuffer_LostFlagWithoutData(t *testing.T) {
	r := newRingBuffer(2)
	r.Write([]byte("AB"))
	r.Write([]byte("CD")) // drop AB, store CD; lost=true

	// Drain everything — leaves the buffer empty but with lost still true
	// only on the first read.
	data, lost := r.ReadChunk(64)
	if !lost || string(data) != "CD" {
		t.Fatalf("got (%q, %v), want (CD, true)", data, lost)
	}
	data, lost = r.ReadChunk(64)
	if lost || data != nil {
		t.Fatalf("got (%q, %v), want (nil, false)", data, lost)
	}
}

// ─── TerminalAttach unit tests ───────────────────────────────────────────

func TestNewTerminalAttach_RequiresClient(t *testing.T) {
	_, err := NewTerminalAttach(context.Background(), AttachConfig{
		SessionName: "any",
	})
	if err == nil {
		t.Fatal("expected error when TmuxClient is nil")
	}
}

func TestNewTerminalAttach_RequiresSessionName(t *testing.T) {
	_, err := NewTerminalAttach(context.Background(), AttachConfig{
		TmuxClient: NewClient(),
	})
	if err == nil {
		t.Fatal("expected error when SessionName is empty")
	}
}

// ─── TerminalAttach integration tests (require tmux on PATH) ─────────────

func skipIfNoTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed, skipping integration test")
	}
}

// uniqueSessionName generates a session name that won't collide with
// concurrent test runs on the same machine. tmux session names cannot
// contain `.` (it's the window/pane separator), so use the nanosecond
// portion as a non-dotted suffix instead.
func uniqueSessionName(prefix string) string {
	now := time.Now()
	return prefix + "-" + now.Format("20060102150405") + "-" + strconv.FormatInt(now.UnixNano()%1_000_000, 10)
}

func TestTerminalAttach_RoundTrip(t *testing.T) {
	skipIfNoTmux(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := NewClient()
	name := uniqueSessionName("boss-attach-rt")
	t.Cleanup(func() { _ = c.KillSession(context.Background(), name) })

	if err := c.NewSession(ctx, NewSessionOpts{
		Name:    name,
		WorkDir: t.TempDir(),
		Command: []string{"cat"},
	}); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}
	if !c.HasSession(ctx, name) {
		t.Fatal("session should exist after creation")
	}

	att, err := NewTerminalAttach(ctx, AttachConfig{
		AttachID:    "test-attach-1",
		SessionName: name,
		Cols:        80,
		Rows:        24,
		TmuxClient:  c,
	})
	if err != nil {
		t.Fatalf("NewTerminalAttach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })

	// Give tmux a moment to wire up the attach + redraw.
	time.Sleep(200 * time.Millisecond)

	// Type a string + newline so cat echoes it back.
	if err := att.Input([]byte("hello-cat\n")); err != nil {
		t.Fatalf("Input: %v", err)
	}

	// Drain Output for up to 3 seconds, looking for our string in the bytes.
	deadline := time.After(3 * time.Second)
	var seen bytes.Buffer
	for !strings.Contains(seen.String(), "hello-cat") {
		select {
		case chunk, ok := <-att.Output():
			if !ok {
				t.Fatalf("Output closed before seeing input echo, got=%q", seen.String())
			}
			seen.Write(chunk.Data)
			if chunk.Lost {
				t.Errorf("did not expect Lost=true on round-trip")
			}
		case <-deadline:
			t.Fatalf("timed out waiting for echo; got=%q", seen.String())
		}
	}
}

func TestTerminalAttach_Resize(t *testing.T) {
	skipIfNoTmux(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := NewClient()
	name := uniqueSessionName("boss-attach-rs")
	t.Cleanup(func() { _ = c.KillSession(context.Background(), name) })

	if err := c.NewSession(ctx, NewSessionOpts{
		Name: name, WorkDir: t.TempDir(), Command: []string{"cat"},
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	att, err := NewTerminalAttach(ctx, AttachConfig{
		AttachID: "rs", SessionName: name, Cols: 80, Rows: 24, TmuxClient: c,
	})
	if err != nil {
		t.Fatalf("NewTerminalAttach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })

	if err := att.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
}

func TestTerminalAttach_BackpressureRaisesLost(t *testing.T) {
	skipIfNoTmux(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c := NewClient()
	name := uniqueSessionName("boss-attach-bp")
	t.Cleanup(func() { _ = c.KillSession(context.Background(), name) })

	// Run yes — produces a continuous stream of "y\n" — so tmux pumps a
	// lot of output through. Tiny buffer guarantees overflow.
	if err := c.NewSession(ctx, NewSessionOpts{
		Name: name, WorkDir: t.TempDir(), Command: []string{"yes"},
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	att, err := NewTerminalAttach(ctx, AttachConfig{
		AttachID:    "bp",
		SessionName: name,
		Cols:        200,
		Rows:        50,
		TmuxClient:  c,
		// 64 bytes guarantees every PTY read overflows — even slow runners
		// under -race will see Lost=true raised on essentially every emit.
		RingBufferSize: 64,
	})
	if err != nil {
		t.Fatalf("NewTerminalAttach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })

	// Don't read for a while so the buffer overflows. The Output channel is
	// buffered (small depth), the ring buffer is 64 bytes, and `yes` is
	// producing at full speed — so once the channel buffer fills, the
	// ringbuffer must drop oldest bytes and the next emitted chunk MUST
	// carry Lost=true.
	time.Sleep(500 * time.Millisecond)

	// Drain Output. Under sustained overflow (`yes` pumps far faster than the
	// tiny ring drains), at least one emitted chunk MUST carry Lost=true. We
	// deliberately do NOT assert that subsequent chunks reset to Lost=false:
	// the readLoop can write a fresh overflow into the ring between any two
	// ReadChunks, so consecutive Lost=true chunks are correct behaviour per
	// the ringBuffer doc ("subsequent chunks set lost=false until the next
	// overflow"). The clear-on-emit invariant is exercised deterministically
	// by TestRingBuffer_LostFlagDoesNotPersistPastOneEmit.
	deadline := time.After(5 * time.Second)
	var chunks []*pb.TerminalDataChunk
loop:
	for len(chunks) < 32 {
		select {
		case chunk, ok := <-att.Output():
			if !ok {
				break loop
			}
			chunks = append(chunks, chunk)
		case <-deadline:
			break loop
		}
	}

	sawLost := false
	for _, c := range chunks {
		if c.Lost {
			sawLost = true
			break
		}
	}
	if !sawLost {
		t.Fatalf("no chunk had Lost=true after backpressure (got %d chunks)", len(chunks))
	}
}

func TestTerminalAttach_ExitFiresWhenSessionKilled(t *testing.T) {
	skipIfNoTmux(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := NewClient()
	name := uniqueSessionName("boss-attach-x")
	t.Cleanup(func() { _ = c.KillSession(context.Background(), name) })

	if err := c.NewSession(ctx, NewSessionOpts{
		Name: name, WorkDir: t.TempDir(), Command: []string{"cat"},
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	att, err := NewTerminalAttach(ctx, AttachConfig{
		AttachID: "x", SessionName: name, Cols: 80, Rows: 24, TmuxClient: c,
	})
	if err != nil {
		t.Fatalf("NewTerminalAttach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })

	// Give the attach a moment to be wired up.
	time.Sleep(200 * time.Millisecond)
	if err := c.KillSession(context.Background(), name); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	// Wait for Exited first, then assert that Output is ALREADY closed by
	// the time we receive the exit frame. This locks in the documented
	// ordering invariant: receive Exited → Output already closed.
	exitTimeout := time.After(5 * time.Second)
	var exitFrame *pb.TerminalAttachExited

	// Drain Output in the background so senderLoop is never stuck pushing
	// onto a full channel — but do NOT mark "outputClosed" until after
	// Exited fires; we want to assert that the close already happened.
	drainDone := make(chan struct{})
	go func() {
		for range att.Output() {
		}
		close(drainDone)
	}()

	select {
	case fr, ok := <-att.Exited():
		if !ok {
			t.Fatal("Exited channel closed without delivering a frame")
		}
		exitFrame = fr
	case <-exitTimeout:
		t.Fatalf("timed out waiting for Exited")
	}

	// At this point Output() MUST already be closed. Use a non-blocking
	// receive on `drainDone` (which is closed only after Output() closes
	// and the goroutine returns) — but we need to allow a tiny scheduling
	// window for the drain goroutine to observe the close, so use a short
	// deadline rather than zero.
	select {
	case <-drainDone:
		// Output channel is closed — invariant holds.
	case <-time.After(2 * time.Second):
		t.Fatalf("Output() was not closed by the time Exited fired (ordering invariant violated)")
	}

	if exitFrame == nil {
		t.Fatal("exit frame was nil")
		return
	}
	if exitFrame.AttachId != "x" {
		t.Errorf("exit AttachId=%q want %q", exitFrame.AttachId, "x")
	}
}

func TestTerminalAttach_InputAfterCloseReturnsErr(t *testing.T) {
	skipIfNoTmux(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := NewClient()
	name := uniqueSessionName("boss-attach-ic")
	t.Cleanup(func() { _ = c.KillSession(context.Background(), name) })

	if err := c.NewSession(ctx, NewSessionOpts{
		Name: name, WorkDir: t.TempDir(), Command: []string{"cat"},
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	att, err := NewTerminalAttach(ctx, AttachConfig{
		AttachID: "ic", SessionName: name, Cols: 80, Rows: 24, TmuxClient: c,
	})
	if err != nil {
		t.Fatalf("NewTerminalAttach: %v", err)
	}
	if err := att.Close(); err != nil {
		// cmd.Wait on a killed process may return a non-nil ExitError —
		// tolerate any err type from Close itself; we only care that it
		// doesn't hang.
		_ = err
	}
	if err := att.Input([]byte("x")); !errors.Is(err, ErrAttachClosed) {
		t.Errorf("Input after Close returned %v, want ErrAttachClosed", err)
	}
	if err := att.Resize(80, 24); !errors.Is(err, ErrAttachClosed) {
		t.Errorf("Resize after Close returned %v, want ErrAttachClosed", err)
	}
	// Close is idempotent.
	if err := att.Close(); err != nil && !errors.Is(err, ErrAttachClosed) {
		// Re-calling Close after a successful first call returns the same
		// closeErr — exit error from the killed process is acceptable.
		_ = err
	}
}
