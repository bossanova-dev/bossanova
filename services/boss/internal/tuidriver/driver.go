// Package tuidriver provides a programmatic driver for the Boss TUI.
// It spawns the boss binary in a PTY, feeds output through a VT terminal
// emulator, and provides methods for sending keystrokes and reading the
// rendered screen content.
//
// This enables agents to drive and verify TUI behavior end-to-end without
// human interaction.
package tuidriver

import (
	"fmt"
	"io"
	"os"
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
	pty      *os.File
	vt       *vt.Emulator
	mu       sync.Mutex // protects vt.Write, vt.String
	width    int
	height   int
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

	cmd := exec.Command(opts.Command, opts.Args...)
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}

	ptmx, err := creackpty.StartWithSize(cmd, &creackpty.Winsize{
		Rows: uint16(opts.Height),
		Cols: uint16(opts.Width),
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

// Close sends Ctrl+C, waits for the process to exit, and cleans up.
func (d *Driver) Close() error {
	_ = d.SendCtrlC()
	timer := time.NewTimer(3 * time.Second)
	select {
	case <-d.done:
		timer.Stop()
	case <-timer.C:
		_ = d.cmd.Process.Kill()
		<-d.done
	}
	// Reap the child process to prevent zombies.
	_ = d.cmd.Wait()
	// Close the PTY — readLoop has already exited (d.done closed).
	err := d.pty.Close()
	// Unblock responseLoop's vt.Read by closing the vt emulator's internal
	// pipe writer directly. vt.Close() would also set an unsynchronized
	// `closed` bool the race detector flags against vt.Read; io.PipeWriter
	// Close/Read are documented as safe to call concurrently, so going
	// through InputPipe() avoids the race entirely.
	if closer, ok := d.vt.InputPipe().(io.Closer); ok {
		_ = closer.Close()
	}
	<-d.respDone
	return err
}
