package session

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A subscriber that never reads must not block the writer. This is the
// io.Pipe deadlock the bus exists to remove: pw.Write() blocked forever when
// the RPC client stopped draining, and that write was inside the bootstrap.
func TestSetupBusWriteNeverBlocksOnAStalledSubscriber(t *testing.T) {
	bus := NewSetupBus(0)
	_, cancel := bus.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < setupBusSubscriberBuffer*4; i++ {
			if _, err := bus.Write([]byte("line\n")); err != nil {
				t.Errorf("Write returned %v; the bus must never error", err)
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked on a stalled subscriber")
	}
}

func TestSetupBusDeliversWholeLinesToSubscribers(t *testing.T) {
	bus := NewSetupBus(0)
	lines, cancel := bus.Subscribe()
	defer cancel()

	// A single Write may carry several newline-separated lines, and a line may
	// arrive split across two Writes — both shapes a setup script produces.
	if _, err := bus.Write([]byte("alpha\nbra")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := bus.Write([]byte("vo\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, want := range []string{"alpha", "bravo"} {
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

// A client that subscribes after the bootstrap has already emitted output
// still sees it. Without replay, the RPC's subscribe-after-handoff window
// silently eats the first lines of every setup script.
func TestSetupBusReplaysBufferedLinesToALateSubscriber(t *testing.T) {
	bus := NewSetupBus(8)
	if _, err := bus.Write([]byte("early\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	lines, cancel := bus.Subscribe()
	defer cancel()
	select {
	case got := <-lines:
		if got != "early" {
			t.Fatalf("got %q, want %q", got, "early")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late subscriber did not receive the replayed line")
	}
}

// The replay ring must not grow without bound: a setup script emitting tens of
// thousands of lines would otherwise retain all of them for a subscriber that
// may never arrive.
func TestSetupBusReplayIsBoundedToTheMostRecentLines(t *testing.T) {
	bus := NewSetupBus(2)
	for _, line := range []string{"one\n", "two\n", "three\n"} {
		if _, err := bus.Write([]byte(line)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	lines, cancel := bus.Subscribe()
	defer cancel()

	for _, want := range []string{"two", "three"} {
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	select {
	case extra := <-lines:
		t.Fatalf("replay returned %q; the ring should hold only the last 2 lines", extra)
	default:
	}
}

// A setup script whose final line has no trailing newline — `printf done`, or
// a script killed mid-line — must still deliver that line. Nothing else in the
// bus ever flushes it, so without this Close the tail is silently lost.
func TestSetupBusCloseFlushesAnUnterminatedTail(t *testing.T) {
	bus := NewSetupBus(0)
	lines, cancel := bus.Subscribe()
	defer cancel()

	if _, err := bus.Write([]byte("no trailing newline")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	bus.Close()

	select {
	case got, ok := <-lines:
		if !ok {
			t.Fatal("channel closed without delivering the unterminated tail")
		}
		if got != "no trailing newline" {
			t.Fatalf("got %q, want the unterminated tail", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not flush the unterminated tail")
	}
}

// A CRLF straddling two Writes is the shape a Windows-flavoured setup script
// produces under a pipe that flushes on a size boundary. The '\r' must be
// trimmed at line assembly, not per-Write, or it leaks into the payload.
func TestSetupBusTrimsACarriageReturnSplitAcrossWrites(t *testing.T) {
	bus := NewSetupBus(0)
	lines, cancel := bus.Subscribe()
	defer cancel()

	if _, err := bus.Write([]byte("split\r")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := bus.Write([]byte("\nnext\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, want := range []string{"split", "next"} {
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

// A newline-free blob must not grow the daemon's memory without bound. The
// io.Pipe this replaces was bounded by a sized bufio reader; the bus keeps the
// bound and truncates instead of failing the bootstrap.
func TestSetupBusBoundsALineWithNoNewline(t *testing.T) {
	bus := NewSetupBus(0)
	lines, cancel := bus.Subscribe()
	defer cancel()

	// Written in chunks: the cap has to hold across Writes, not merely within
	// one, which is where an unbounded accumulator would hide.
	chunk := strings.Repeat("x", 64*1024)
	for range 8 {
		if _, err := bus.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if _, err := bus.Write([]byte("\ntail\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case got := <-lines:
		if len(got) != setupBusMaxLineBytes {
			t.Fatalf("line len = %d, want it capped at %d", len(got), setupBusMaxLineBytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the capped line")
	}
	// The overflow is discarded up to the newline, not re-emitted as more lines.
	select {
	case got := <-lines:
		if got != "tail" {
			t.Fatalf("got %q, want the next line %q — the overflow leaked", got, "tail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the line after the capped one")
	}
}

func TestSetupBusCloseClosesSubscriberChannels(t *testing.T) {
	bus := NewSetupBus(0)
	lines, cancel := bus.Subscribe()
	defer cancel()

	bus.Close()
	select {
	case _, ok := <-lines:
		if ok {
			t.Fatal("expected the subscriber channel to be closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not close the subscriber channel")
	}
}

// Close is called from the runner's defer and cancel from the RPC's defer, so
// they race by construction: both must be safe in any order and repeatedly.
func TestSetupBusCloseAndCancelAreIdempotentInAnyOrder(t *testing.T) {
	bus := NewSetupBus(0)
	_, cancel := bus.Subscribe()
	cancel()
	cancel()
	bus.Close()
	bus.Close()

	if _, err := bus.Write([]byte("after close\n")); err != nil {
		t.Fatalf("Write after Close returned %v; it must stay a silent no-op", err)
	}
}

// Subscribing to an already-closed bus is the RPC's subscribe-after-handoff
// race when the bootstrap finished first. It must be benign.
func TestSetupBusSubscribeAfterCloseReturnsAClosedChannel(t *testing.T) {
	bus := NewSetupBus(0)
	bus.Close()

	lines, cancel := bus.Subscribe()
	defer cancel()
	select {
	case _, ok := <-lines:
		if ok {
			t.Fatal("expected an already-closed channel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe after Close returned an open channel")
	}
}

// The test above uses a zero replay limit, so it holds trivially whether or not
// a closed bus replays. This is the case that actually matters: the bootstrap
// SETTLED between StreamCreateSession's Start and its Subscribe, which is most
// likely for a fast failure — precisely the run whose one diagnostic line is
// the only thing the client ever had. Losing it here would be the regression
// against the io.Pipe that the replay ring exists to prevent.
func TestSetupBusSubscribeAfterCloseStillReplays(t *testing.T) {
	bus := NewSetupBus(setupBusReplayLines)
	if _, err := bus.Write([]byte("cloning\nfatal: repository not found\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	bus.Close()

	lines, cancel := bus.Subscribe()
	defer cancel()

	var got []string
	for line := range lines {
		got = append(got, line)
	}
	want := []string{"cloning", "fatal: repository not found"}
	if !slices.Equal(got, want) {
		t.Fatalf("replay after Close = %q, want %q", got, want)
	}
}
