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

// detachSequences lists every byte sequence we treat as "user pressed
// Ctrl+X / Ctrl+]". The same key can arrive in several forms depending on
// what the inner TUI (Claude Code) negotiated with the user's real
// terminal:
//
//   - Raw control byte (0x18 / 0x1d) when no enhanced keyboard mode is
//     active.
//   - kitty keyboard protocol: CSI codepoint;modifier u — "\x1b[120;5u"
//     for Ctrl+x, "\x1b[93;5u" for Ctrl+].
//   - xterm modifyOtherKeys=2: CSI 27;modifier;codepoint ~ — Claude Code
//     enables this when it boots, so on a fresh attach Ctrl+X arrives as
//     "\x1b[27;5;120~".
//
// Any of these in the inbound chunk triggers detach.
var detachSequences = [][]byte{
	[]byte("\x1b[120;5u"),    // kitty Ctrl+x
	[]byte("\x1b[93;5u"),     // kitty Ctrl+]
	[]byte("\x1b[27;5;120~"), // modifyOtherKeys=2 Ctrl+x
	[]byte("\x1b[27;5;93~"),  // modifyOtherKeys=2 Ctrl+]
}

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
		// pending carries an incomplete terminal-query reply across reads.
		// See stripTerminalQueryReplies for the leak it defends against.
		var pending []byte
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
					// Strip terminal capability-query replies (DA1/DA2/DCS)
					// that tmux's client startup probes leak onto stdin
					// during attach. Run before the detach scan so we
					// inspect the same bytes the PTY will see — pending
					// carries any incomplete sequence into the next read.
					var data []byte
					data, pending = stripTerminalQueryReplies(buf[:n], pending)

					if len(data) > 0 {
						// Check for raw detach bytes (Ctrl+X or Ctrl+]).
						detached := false
						for _, b := range data {
							if b == detachByteCtrlX || b == detachByte {
								detached = true
								break
							}
						}

						// Check for any of the encoded forms (kitty CSI-u or
						// xterm modifyOtherKeys=2). Claude Code enables one of
						// these on attach, so the raw byte path above won't
						// fire on a real terminal.
						if !detached {
							for _, seq := range detachSequences {
								if bytes.Contains(data, seq) {
									detached = true
									break
								}
							}
						}
						if detached {
							close(detachCh)
							return
						}

						_ = proc.WriteInput(data)
					}
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
