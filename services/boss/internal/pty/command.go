package pty

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	creackpty "github.com/creack/pty/v2"
	"golang.org/x/term"
)

const detachByte = 0x1d      // Ctrl+]
const detachByteCtrlX = 0x18 // Ctrl+X

// ctrlRBracketCSIu is the kitty keyboard protocol encoding of Ctrl+].
var ctrlRBracketCSIu = []byte("\x1b[93;5u")

// ctrlXCSIu is the kitty keyboard protocol encoding of Ctrl+X (codepoint 120, modifier 5=Ctrl).
var ctrlXCSIu = []byte("\x1b[120;5u")

// PTYCommand implements bubbletea's ExecCommand interface.
// It proxies I/O between the real terminal and a PTY-hosted process,
// allowing the user to detach (Ctrl+X or Ctrl+]) while the process keeps running.
type PTYCommand struct {
	manager  *Manager
	claudeID string
	cmd      *exec.Cmd // nil when reattaching to an existing process

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// Set after Run() returns.
	Detached      bool
	ProcessExited bool
}

// NewPTYCommand creates a PTYCommand for launching or reattaching to a Claude process.
func NewPTYCommand(manager *Manager, claudeID string, cmd *exec.Cmd) *PTYCommand {
	return &PTYCommand{
		manager:  manager,
		claudeID: claudeID,
		cmd:      cmd,
	}
}

// SetStdin is called by bubbletea before Run().
func (c *PTYCommand) SetStdin(r io.Reader) { c.stdin = r }

// SetStdout is called by bubbletea before Run().
func (c *PTYCommand) SetStdout(w io.Writer) { c.stdout = w }

// SetStderr is called by bubbletea before Run().
func (c *PTYCommand) SetStderr(w io.Writer) { c.stderr = w }

// Run blocks until the user detaches or the process exits.
func (c *PTYCommand) Run() error {
	// Put the real terminal in raw mode so keystrokes are forwarded
	// immediately without echo. The PTY slave handles its own modes.
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, oldState) //nolint:errcheck // best-effort restore on exit

	proc, err := c.manager.GetOrStart(c.claudeID, c.cmd)
	if err != nil {
		return err
	}

	// Connect output.
	proc.Attach(c.stdout)
	defer proc.Detach()

	// Set initial PTY size from the real terminal.
	if f, ok := c.stdout.(*os.File); ok {
		if rows, cols, sizeErr := creackpty.Getsize(f); sizeErr == nil {
			_ = proc.Resize(uint16(rows), uint16(cols))
		}
	}

	// Relay SIGWINCH to resize the PTY.
	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, syscall.SIGWINCH)
	defer signal.Stop(sigch)
	go func() {
		for range sigch {
			if f, ok := c.stdout.(*os.File); ok {
				if rows, cols, sizeErr := creackpty.Getsize(f); sizeErr == nil {
					_ = proc.Resize(uint16(rows), uint16(cols))
				}
			}
		}
	}()

	// Replay any buffered output from a previous attach.
	proc.ReplayBuffer(c.stdout)

	// Create a cancel pipe for interrupting the stdin read goroutine.
	cancelR, cancelW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer cancelR.Close() //nolint:errcheck // best-effort cleanup
	defer cancelW.Close() //nolint:errcheck // best-effort cleanup

	inputDone := make(chan error, 1)
	detachCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		stdinFd := int(os.Stdin.Fd())
		cancelFd := int(cancelR.Fd())
		for {
			// Wait for stdin or cancel pipe using select(2).
			maxFd := stdinFd
			if cancelFd > maxFd {
				maxFd = cancelFd
			}
			var readSet syscall.FdSet
			fdSet(&readSet, stdinFd)
			fdSet(&readSet, cancelFd)

			_, err := sysSelect(maxFd+1, &readSet, nil, nil, nil)
			if err != nil {
				if err == syscall.EINTR {
					continue
				}
				inputDone <- err
				return
			}

			// Cancel pipe readable — time to stop.
			if fdIsSet(&readSet, cancelFd) {
				return
			}

			if fdIsSet(&readSet, stdinFd) {
				n, readErr := syscall.Read(stdinFd, buf)
				if n > 0 {
					data := buf[:n]

					// Check for raw detach bytes (Ctrl+X or Ctrl+]).
					for _, b := range data {
						if b == detachByteCtrlX || b == detachByte {
							close(detachCh)
							return
						}
					}

					// Check for CSI u encodings (kitty protocol).
					if bytes.Contains(data, ctrlXCSIu) || bytes.Contains(data, ctrlRBracketCSIu) {
						close(detachCh)
						return
					}

					_ = proc.WriteInput(data)
				}
				if readErr != nil {
					inputDone <- readErr
					return
				}
			}
		}
	}()

	defer func() {
		_, _ = cancelW.Write([]byte{0}) // Signal goroutine to stop.
		wg.Wait()                       // Wait for it to exit.
	}()

	select {
	case <-detachCh:
		c.Detached = true
		return nil

	case <-proc.Done():
		c.ProcessExited = true
		return proc.ExitErr()

	case <-inputDone:
		// stdin closed or error — treat as detach.
		c.Detached = true
		return nil
	}
}

// fdSet sets a file descriptor in a syscall.FdSet.
func fdSet(set *syscall.FdSet, fd int) {
	set.Bits[fd/64] |= 1 << (uint(fd) % 64)
}

// fdIsSet checks if a file descriptor is set in a syscall.FdSet.
func fdIsSet(set *syscall.FdSet, fd int) bool {
	return set.Bits[fd/64]&(1<<(uint(fd)%64)) != 0
}
