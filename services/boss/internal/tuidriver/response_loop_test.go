package tuidriver

import (
	"os"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// TestResponseLoopForwardsTerminalResponses verifies that responseLoop sends
// emulator-generated terminal capability responses back to the PTY.
func TestResponseLoopForwardsTerminalResponses(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()

	emulator := vt.NewEmulator(80, 24)
	d := &Driver{
		pty:      writer,
		vt:       emulator,
		respDone: make(chan struct{}),
	}
	go d.responseLoop()
	defer func() {
		if closer, ok := emulator.InputPipe().(interface{ Close() error }); ok {
			_ = closer.Close()
		}
		<-d.respDone
	}()

	// CSI c asks for primary device attributes. The emulator generates a
	// response on its input pipe, which responseLoop must forward to the PTY.
	if _, err := emulator.Write([]byte("\x1b[c")); err != nil {
		t.Fatalf("emulator.Write: %v", err)
	}
	if err := reader.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("read forwarded terminal response: %v", err)
	}

	const want = "\x1b[?62;1;6;22c"
	if got := string(buf[:n]); got != want {
		t.Errorf("forwarded response = %q, want %q", got, want)
	}
}

// TestWaitForPollsAtDocumentedInterval keeps an unmet predicate from spinning
// while it waits for the screen to change.
func TestWaitForPollsAtDocumentedInterval(t *testing.T) {
	d := &Driver{vt: vt.NewEmulator(80, 24)}
	calls := 0
	if err := d.WaitFor(110*time.Millisecond, func(string) bool {
		calls++
		return false
	}); err == nil {
		t.Fatal("WaitFor returned nil for an unmet predicate")
	}

	// The initial call plus polls at roughly 50 ms and 100 ms are expected.
	// A small allowance tolerates scheduler jitter; a zero-duration mutation
	// would spin thousands of times before the deadline.
	if calls > 4 {
		t.Fatalf("WaitFor evaluated predicate %d times in 110ms; want at most 4", calls)
	}
}
