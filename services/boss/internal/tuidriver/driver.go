// Package tuidriver provides a programmatic driver for the Boss TUI.
// It spawns the boss binary in a PTY, feeds output through a VT terminal
// emulator, and provides methods for sending keystrokes and reading the
// rendered screen content.
//
// This enables agents to drive and verify TUI behavior end-to-end without
// human interaction.
package tuidriver

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strings"
	"sync"
	"time"

	creackpty "github.com/creack/pty/v2"

	"github.com/charmbracelet/x/vt"
)

// Driver controls a TUI process running in a PTY.
type Driver struct {
	cmd      *exec.Cmd
	pty      io.ReadWriteCloser
	vt       *vt.Emulator
	mu       sync.Mutex // protects vt.Write, vt.String
	width    int
	height   int
	rawOut   io.Writer     // if non-nil, raw PTY bytes are teed here (for .cast capture)
	done     chan struct{} // closed when readLoop exits
	respDone chan struct{} // closed when responseLoop exits
}

// Options configures the TUI driver.
type Options struct {
	// Command is the executable path (e.g. compiled boss binary).
	Command string
	// Args are additional CLI arguments.
	Args []string
	// Env is the process environment. If nil, inherits os.Environ().
	Env []string
	// Dir is the working directory for the process.
	Dir string
	// Width is the terminal width in columns (default 120).
	Width int
	// Height is the terminal height in rows (default 30).
	Height int
	// RawOutput, if non-nil, receives a copy of every raw PTY output byte
	// (the same bytes fed to the VT emulator) for .cast capture.
	RawOutput io.Writer
}

// New spawns a command in a PTY and begins reading output into the VT
// emulator. The caller must call Close when done.
func New(opts Options) (*Driver, error) {
	if opts.Width == 0 {
		opts.Width = 120
	}
	if opts.Height == 0 {
		opts.Height = 30
	}

	// #nosec G204 -- generic PTY driver runs caller-supplied command; local/test-trust
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.Command(opts.Command, opts.Args...)
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}

	ptmx, err := creackpty.StartWithSize(cmd, &creackpty.Winsize{
		Rows: clampUint16(opts.Height),
		Cols: clampUint16(opts.Width),
	})
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}

	em := vt.NewEmulator(opts.Width, opts.Height)

	d := &Driver{
		cmd:      cmd,
		pty:      ptmx,
		vt:       em,
		width:    opts.Width,
		height:   opts.Height,
		rawOut:   opts.RawOutput,
		done:     make(chan struct{}),
		respDone: make(chan struct{}),
	}

	// Drain VT emulator responses (DA, mode queries) and feed them back
	// to the PTY so bubbletea receives its expected terminal responses.
	go d.responseLoop()

	go d.readLoop()

	return d, nil
}

// readLoop reads PTY output and feeds it to the VT emulator.
func (d *Driver) readLoop() {
	defer close(d.done)
	buf := make([]byte, 4096)
	for {
		n, err := d.pty.Read(buf)
		if n > 0 {
			d.mu.Lock()
			_, _ = d.vt.Write(buf[:n])
			d.mu.Unlock()
			if d.rawOut != nil {
				_, _ = d.rawOut.Write(buf[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

// responseLoop reads terminal response sequences from the VT emulator
// and writes them back to the PTY. This is necessary because bubbletea
// queries terminal capabilities (DECRQM, DA, etc.) and expects responses.
// Without draining these, the VT emulator's internal pipe blocks.
func (d *Driver) responseLoop() {
	defer close(d.respDone)
	buf := make([]byte, 256)
	for {
		n, err := d.vt.Read(buf)
		if n > 0 {
			_, _ = d.pty.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// Width returns the effective terminal width in columns, after any default
// has been applied in New.
func (d *Driver) Width() int { return d.width }

// Height returns the effective terminal height in rows, after any default
// has been applied in New.
func (d *Driver) Height() int { return d.height }

// Screen returns the current terminal screen as plain text.
func (d *Driver) Screen() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.vt.String()
}

// ScreenContains returns true if the screen contains the given substring.
func (d *Driver) ScreenContains(text string) bool {
	return strings.Contains(d.Screen(), text)
}

// SendKey writes a single byte to the PTY (e.g. 'j', 'q', 'a').
func (d *Driver) SendKey(b byte) error {
	_, err := d.pty.Write([]byte{b})
	return err
}

// SendString writes a string to the PTY.
func (d *Driver) SendString(s string) error {
	_, err := d.pty.Write([]byte(s))
	return err
}

// PasteString writes bracketed paste bytes to the PTY.
func (d *Driver) PasteString(s string) error {
	_, err := d.pty.Write([]byte("\x1b[200~" + s + "\x1b[201~"))
	return err
}

// SendEnter sends a carriage return.
func (d *Driver) SendEnter() error {
	return d.SendKey('\r')
}

// SendEscape sends the escape character.
func (d *Driver) SendEscape() error {
	return d.SendKey(0x1b)
}

// SendCtrlC sends ETX (Ctrl+C).
func (d *Driver) SendCtrlC() error {
	return d.SendKey(0x03)
}

// WaitFor polls the screen until the predicate returns true or timeout.
// It polls every 50ms.
func (d *Driver) WaitFor(timeout time.Duration, pred func(screen string) bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred(d.Screen()) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s; last screen:\n%s", timeout, d.Screen())
}

// WaitForText waits until the screen contains the given text.
func (d *Driver) WaitForText(timeout time.Duration, text string) error {
	return d.WaitFor(timeout, func(screen string) bool {
		return strings.Contains(screen, text)
	})
}

// WaitForNoText waits until the screen no longer contains the given text.
func (d *Driver) WaitForNoText(timeout time.Duration, text string) error {
	return d.WaitFor(timeout, func(screen string) bool {
		return !strings.Contains(screen, text)
	})
}

// Done returns a channel that is closed when the process exits.
func (d *Driver) Done() <-chan struct{} {
	return d.done
}

// closeWaitTimeout bounds each teardown wait in Close. A pty read wedged in the
// kernel never returns, so readLoop never closes d.done; before BOS-698 that
// deadlocked Close's caller until the 300s Bazel test timeout killed the whole
// target with a message that named nothing.
const closeWaitTimeout = 5 * time.Second

// waitClosed blocks until ch is closed or d elapses, reporting whether ch
// closed in time.
func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

// Close sends Ctrl+C, waits for the process to exit, and cleans up.
//
// Every wait is bounded (BOS-698). On expiry Close deliberately does NOT return
// early: closing the pty is itself what usually unblocks a wedged readLoop, so
// bailing out would strand the very goroutine the bound exists to rescue.
// Instead Close finishes the teardown and reports each stalled loop in its
// returned error, joined with any pty close error. All current callers discard
// that error; a caller that checks it now learns the driver did not shut down
// cleanly instead of hanging forever.
func (d *Driver) Close() error {
	_ = d.SendCtrlC()

	var errs []error

	// Graceful window: give a Ctrl-C-aware process time to run its shutdown
	// handler before force-killing it.
	if !waitClosed(d.done, 3*time.Second) {
		_ = d.cmd.Process.Kill()
		if !waitClosed(d.done, closeWaitTimeout) {
			errs = append(errs, fmt.Errorf("tuidriver: readLoop did not exit within %s after Kill", closeWaitTimeout))
		}
	}

	// Reap the child process to prevent zombies.
	_ = d.cmd.Wait()

	// Close the PTY. On the normal path readLoop has already exited (d.done
	// closed); on the stalled path this close is also what unblocks its read.
	if err := d.pty.Close(); err != nil {
		errs = append(errs, err)
	}

	// Unblock responseLoop's vt.Read by closing the vt emulator's internal
	// pipe writer directly. vt.Close() would also set an unsynchronized
	// `closed` bool the race detector flags against vt.Read; io.PipeWriter
	// Close/Read are documented as safe to call concurrently, so going
	// through InputPipe() avoids the race entirely.
	if closer, ok := d.vt.InputPipe().(io.Closer); ok {
		_ = closer.Close()
	}
	if !waitClosed(d.respDone, closeWaitTimeout) {
		errs = append(errs, fmt.Errorf("tuidriver: responseLoop did not exit within %s", closeWaitTimeout))
	}

	return errors.Join(errs...)
}

// clampUint16 narrows a terminal dimension (rows/cols — always small and
// non-negative in practice) to uint16, clamping any out-of-range value into
// [0, math.MaxUint16] so the conversion can neither overflow nor wrap.
func clampUint16(v int) uint16 {
	if v < 0 {
		return 0
	}
	if v > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(v)
}
