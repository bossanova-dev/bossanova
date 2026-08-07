package pty

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	creackpty "github.com/creack/pty/v2"
	"golang.org/x/term"

	"github.com/recurser/boss/internal/termreset"
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
	manager        *Manager
	agentSessionID string
	cmd            *exec.Cmd // nil when reattaching to an existing process

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// Set after Run() returns.
	Detached      bool
	ProcessExited bool

	// pasteUpload, when non-nil, copies a pasted local image to the machine the
	// agent runs on (see SetPasteUploader). Nil is local mode.
	pasteUpload PasteUpload

	// uploadMu guards uploadDone, which holds the safego.Go done channels of
	// every upload goroutine launched during this attach so teardown can join
	// them instead of leaking them.
	uploadMu   sync.Mutex
	uploadDone []<-chan struct{}

	// enterHold withholds the user's Enter while a paste upload is in flight, so
	// a turn cannot be submitted with the image removed and no path in its
	// place. Zero-valued and inert until pasteClaim calls begin, so local mode
	// never withholds a keystroke.
	enterHold pasteEnterHold

	inputReady chan struct{}
}

// NewPTYCommand creates a PTYCommand for launching or reattaching to a Claude process.
func NewPTYCommand(manager *Manager, agentSessionID string, cmd *exec.Cmd) *PTYCommand {
	return &PTYCommand{
		manager:        manager,
		agentSessionID: agentSessionID,
		cmd:            cmd,
	}
}

// SetStdin is called by bubbletea before Run().
func (c *PTYCommand) SetStdin(r io.Reader) { c.stdin = r }

// SetStdout is called by bubbletea before Run().
func (c *PTYCommand) SetStdout(w io.Writer) { c.stdout = w }

// SetStderr is called by bubbletea before Run().
func (c *PTYCommand) SetStderr(w io.Writer) { c.stderr = w }

// stdinFile returns the *os.File backing stdin — c.stdin if SetStdin was
// called with one (the production path: bubbletea passes os.Stdin), and
// os.Stdin as a fallback. Reading the global os.Stdin from inside Run()
// races with tests that swap os.Stdin to inject a PTY slave, so route
// every fd lookup through this helper.
func (c *PTYCommand) stdinFile() *os.File {
	if f, ok := c.stdin.(*os.File); ok {
		return f
	}
	return os.Stdin
}

// Run blocks until the user detaches or the process exits.
func (c *PTYCommand) Run() error {
	// Put the real terminal in raw mode so keystrokes are forwarded
	// immediately without echo. The PTY slave handles its own modes.
	stdinFile := c.stdinFile()
	fd := int(stdinFile.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, oldState) //nolint:errcheck // best-effort restore on exit
	// Reset the outer terminal's mouse-tracking modes before restoring cooked
	// mode. LIFO defer ordering runs this BEFORE term.Restore, while the
	// terminal is still ours. A foreign full-screen child (tmux with
	// `set -g mouse on`) enables xterm mouse reporting on the real terminal but
	// never emits the matching reset when boss abandons the proxy on Ctrl+X
	// detach, stranding the terminal in mouse-reporting mode and breaking native
	// drag-select. Fires on every return past MakeRaw (detach, process-exit,
	// inputDone, early errors). See BOS-499.
	defer func() {
		if c.stdout != nil {
			writeTerminalModeReset(c.stdout)
		}
	}()

	proc, err := c.manager.GetOrStart(c.agentSessionID, c.cmd)
	if err != nil {
		return err
	}

	// Connect output.
	proc.Attach(c.stdout)
	defer proc.Detach()

	// Set initial PTY size from the real terminal.
	if f, ok := c.stdout.(*os.File); ok {
		if rows, cols, sizeErr := creackpty.Getsize(f); sizeErr == nil {
			_ = proc.Resize(clampUint16(rows), clampUint16(cols))
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
					_ = proc.Resize(clampUint16(rows), clampUint16(cols))
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

	// Uploads triggered by a pasted image run off the keystroke path, so they
	// need a lifetime bound to this attach.
	uploadCtx, cancelUploads := context.WithCancel(context.Background())
	// Registered BEFORE the stdin-reader stop defer below, so LIFO ordering
	// runs this AFTER that reader has been joined — no new upload can be
	// launched while we drain. Cancel first, then wait: PasteUpload runs its
	// transport under uploadCtx, so cancelling makes an in-flight upload die on
	// detach, which is why the drain needs no timeout.
	defer func() {
		cancelUploads()
		c.awaitPasteUploads()
	}()

	// In local mode pasteClaim returns nil, newPasteScanner keeps no state, and
	// feed below is the identity function.
	pasteScan := newPasteScanner(c.pasteClaim(uploadCtx, proc))

	inputDone := make(chan error, 1)
	detachCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		stdinFd := fd
		cancelFd := int(cancelR.Fd())
		if c.inputReady != nil {
			close(c.inputReady)
		}
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
					var flushedStalePending bool
					data, pending, flushedStalePending = stripTerminalQueryReplies(buf[:n], pending)
					if flushedStalePending {
						slog.Debug(
							"flushed stale terminal query reply pending bytes",
							"agentSessionID", c.agentSessionID,
							// Total bytes forwarded (stale fragment + the real
							// input that triggered the flush), not the leaked
							// fragment size alone.
							"forwardedBytes", len(data),
						)
					}

					// Paste interception sits AFTER the query-reply strip so it
					// sees exactly the bytes the PTY would have seen, and
					// BEFORE the detach scan so detach keeps firing on
					// everything still forwarded — a claimed paste is gone by
					// then and can never be mistaken for a keypress. In local
					// mode this is the identity function (nil claim above).
					//
					// Detach latency is unchanged in host mode too: a detach
					// key arriving INSIDE an in-flight paste makes the scanner
					// flush the buffered body verbatim and return to
					// passthrough on that same chunk, so the key is in `data`
					// by the time the scan below runs. Do NOT move that scan
					// above feed to "fix" this: returning early would leave the
					// scanner holding carry-over state for bytes it never saw,
					// and the scan would run on a chunk the scanner may still
					// claim — the ordering is what keeps "what the agent sees"
					// and "what detach sees" the same bytes.
					data = pasteScan.feed(data)

					if len(data) > 0 {
						if containsDetachSequence(data) {
							// Detaching abandons the composer, so a submit key
							// withheld behind an in-flight upload has nothing
							// left to submit and must not be replayed into a
							// process being torn down.
							c.enterHold.discard()
							close(detachCh)
							return
						}

						// Withhold Enter while an upload is in flight — see
						// pasteEnterHold. Placed AFTER the detach scan so detach
						// latency is untouched (the scan still sees every byte
						// the user typed), and after feed for the same reason:
						// what the agent sees and what detach sees stay the same
						// bytes, minus only the key being deliberately held.
						// Inert with no upload pending, so local mode is
						// unchanged.
						data = c.enterHold.filter(data)
					}

					if len(data) > 0 {
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

// writeTerminalModeReset writes the xterm mouse-tracking DECRST disable
// sequences to w (best-effort; write errors are ignored, matching the other
// teardown writes). It neutralizes any mouse-reporting modes a foreign
// full-screen child (tmux with `set -g mouse on`) enabled on the real terminal
// but never reset, which would otherwise strand the outer terminal in
// mouse-reporting mode and break native drag-select. DECRST on a mode that was
// never set is a harmless no-op. See BOS-499.
//
// The core set (?1000/?1002/?1003/?1006) covers what tmux `mouse on` enables;
// the defensive extras (?1005 legacy UTF-8, ?1015 urxvt, ?9 X10) cover exotic
// terminal/tmux encodings. The bytes themselves live in internal/termreset so
// the CLI layer (startup self-heal, SIGHUP rescue, `boss fix-terminal`) can
// reuse them without importing the PTY process manager. See BOS-650.
func writeTerminalModeReset(w io.Writer) {
	termreset.WriteMouseReset(w)
}

func containsDetachSequence(data []byte) bool {
	for _, b := range data {
		if b == detachByteCtrlX || b == detachByte {
			return true
		}
	}

	for _, seq := range detachSequences {
		if bytes.Contains(data, seq) {
			return true
		}
	}

	return false
}

// fdSet sets a file descriptor in a syscall.FdSet.
func fdSet(set *syscall.FdSet, fd int) {
	if fd < 0 {
		return
	}
	set.Bits[fd/64] |= 1 << (uint(fd) % 64)
}

// fdIsSet checks if a file descriptor is set in a syscall.FdSet.
func fdIsSet(set *syscall.FdSet, fd int) bool {
	if fd < 0 {
		return false
	}
	return set.Bits[fd/64]&(1<<(uint(fd)%64)) != 0
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
