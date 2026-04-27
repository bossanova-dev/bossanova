package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	creackpty "github.com/creack/pty/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// Tunables for the per-attach pipeline. Size choices come from the plan
// (`docs/plans/2026-04-26-web-tmux-attach.md`):
//   - 256KB ring buffer per attach (drop-oldest with `lost=true` flag).
//   - 32KB max emitted chunk so individual frames stay below the 1MB WS cap
//     by a wide margin and the wire stays predictable.
//   - 8 message Output buffer absorbs a small amount of sender→reader jitter
//     without blocking the PTY read goroutine.
const (
	defaultRingBufferSize = 256 * 1024
	maxChunkBytes         = 32 * 1024
	outputChannelDepth    = 8
	ptyReadChunkSize      = 16 * 1024
)

// ErrAttachClosed is returned by Input/Resize after Close has been called.
var ErrAttachClosed = errors.New("tmux attach closed")

// AttachConfig configures a new TerminalAttach.
type AttachConfig struct {
	// AttachID is the bosso-generated UUID. Currently used only for logging.
	AttachID string

	// SessionName is the persisted tmux session name from the chat row.
	// MUST be the authoritative `tmux_session_name` field — do NOT recompute
	// from `ChatSessionName(repoID, claudeID)` (truncates to 8 chars and
	// risks collisions across sessions; see plan Codex catch #5).
	SessionName string

	// Cols and Rows are the initial PTY size negotiated with the browser.
	// Zero values default to 80x24.
	Cols uint32
	Rows uint32

	// TmuxClient is required. It is used to call SetAttachOptions before
	// spawning the PTY so multi-client behavior is in place.
	TmuxClient *Client

	// RingBufferSize is the per-attach ring buffer size in bytes. Defaults
	// to 256KB if zero.
	RingBufferSize int

	// CommandFactory is the optional injection point for tests; defaults to
	// exec.CommandContext.
	CommandFactory CommandFactory

	// Logger is optional; defaults to a no-op logger.
	Logger zerolog.Logger
}

// TerminalAttach owns one `tmux attach` PTY and the goroutines that pump
// data between it and the daemon's TerminalStream. One TerminalAttach per
// (browser tab, chat) pair.
//
// Lifecycle:
//
//   - NewTerminalAttach calls SetAttachOptions, opens the PTY with `tmux
//     attach -t <name>`, and launches three goroutines:
//
//   - readLoop:    PTY → ring buffer (non-blocking; never blocks tmux).
//
//   - senderLoop:  ring buffer → Output() channel (drains in chunks).
//
//   - waitLoop:    cmd.Wait() → Exited() then closes Output().
//
//   - The caller drains Output() and Exited() and eventually calls Close.
//
//   - Close is idempotent and waits for the goroutines to exit.
type TerminalAttach struct {
	cfg AttachConfig
	log zerolog.Logger

	cmd     *exec.Cmd
	ptyFd   *os.File
	ringBuf *ringBuffer

	output chan *pb.TerminalDataChunk
	exited chan *pb.TerminalAttachExited

	// dataReady is signalled by the read loop after a write; the sender
	// loop drains the buffer when it fires. Buffered length 1 — additional
	// signals while the sender is busy collapse into one wakeup.
	dataReady chan struct{}

	// readDone is closed when the read loop exits (PTY EOF / read error).
	// The sender loop watches it so it can flush remaining bytes and exit.
	readDone chan struct{}

	// outputDone is closed by senderLoop's defer immediately after it closes
	// `output`. waitLoop blocks on this before sending on `exited`, so
	// consumers can rely on: by the time Exited() yields a frame, Output()
	// is already closed.
	outputDone chan struct{}

	// closing is closed by Close to start a coordinated shutdown.
	closing   chan struct{}
	closeOnce sync.Once

	// wg waits for the three internal goroutines to finish.
	wg sync.WaitGroup

	// cancel cancels the cmd context so the tmux client process is killed
	// when Close is called.
	cancel context.CancelFunc

	// seq is the monotonic counter on emitted TerminalDataChunk.
	seq uint64

	// closeErr captures the cmd.Wait error so Close can surface it.
	closeErr error

	// inputMu serialises writes to the PTY.
	inputMu sync.Mutex
}

// NewTerminalAttach calls SetAttachOptions, then spawns `tmux attach -t
// <SessionName>` inside a PTY, and launches the three lifecycle goroutines.
// Returns immediately with the started attach. The caller is responsible for
// draining Output() and Exited() and eventually calling Close.
func NewTerminalAttach(ctx context.Context, cfg AttachConfig) (*TerminalAttach, error) {
	if cfg.TmuxClient == nil {
		return nil, fmt.Errorf("tmux client is required")
	}
	if cfg.SessionName == "" {
		return nil, fmt.Errorf("session name is required")
	}

	ringSize := cfg.RingBufferSize
	if ringSize <= 0 {
		ringSize = defaultRingBufferSize
	}
	cmdFactory := cfg.CommandFactory
	if cmdFactory == nil {
		cmdFactory = exec.CommandContext
	}
	cols := cfg.Cols
	if cols == 0 {
		cols = 80
	}
	rows := cfg.Rows
	if rows == 0 {
		rows = 24
	}

	// Configure session-level multi-client options BEFORE spawning the PTY.
	if err := cfg.TmuxClient.SetAttachOptions(ctx, cfg.SessionName); err != nil {
		return nil, fmt.Errorf("set tmux attach options: %w", err)
	}

	// Bind the child to a derived context so caller cancellation terminates
	// the tmux client process. The cancel func is held by the attach and
	// fired from Close.
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := cmdFactory(cmdCtx, "tmux", "attach", "-t", cfg.SessionName)
	// tmux needs a usable TERM to initialise its terminal driver. CI runners
	// (GitHub Actions etc.) often leave TERM unset or set it to "dumb", which
	// causes `tmux attach` to exit immediately with "open terminal failed".
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = ensureTerm(env)

	ptyFd, err := creackpty.StartWithSize(cmd, &creackpty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start tmux attach pty: %w", err)
	}

	a := &TerminalAttach{
		cfg:        cfg,
		log:        cfg.Logger.With().Str("component", "tmux-attach").Str("attach_id", cfg.AttachID).Str("session", cfg.SessionName).Logger(),
		cmd:        cmd,
		ptyFd:      ptyFd,
		ringBuf:    newRingBuffer(ringSize),
		output:     make(chan *pb.TerminalDataChunk, outputChannelDepth),
		exited:     make(chan *pb.TerminalAttachExited, 1),
		dataReady:  make(chan struct{}, 1),
		readDone:   make(chan struct{}),
		outputDone: make(chan struct{}),
		closing:    make(chan struct{}),
		cancel:     cancel,
	}

	a.wg.Add(3)
	go a.readLoop()
	go a.senderLoop()
	go a.waitLoop()

	a.log.Debug().Int("ring_size", ringSize).Uint32("cols", cols).Uint32("rows", rows).Msg("tmux attach started")
	return a, nil
}

// Output returns the channel of outbound TerminalDataChunk frames. Two
// shutdown paths close this channel:
//
//   - Natural EOF (the tmux client exits or its PTY end closes): the read
//     loop drains, the senderLoop emits any remaining buffered bytes, and
//     then closes Output. All bytes that made it through the ring buffer
//     are flushed.
//
//   - Close()-initiated shutdown: senderLoop returns as soon as it
//     observes `closing`, so any bytes still in the ring buffer or in
//     flight from the read loop are discarded. Only natural EOF guarantees
//     a complete flush.
//
// In both cases, by the time Exited() yields a frame, Output() is
// guaranteed to already be closed.
func (a *TerminalAttach) Output() <-chan *pb.TerminalDataChunk { return a.output }

// Exited returns a channel that receives exactly one TerminalAttachExited
// when the tmux client process exits. The implementation guarantees that
// when a value is received on this channel, Output() is already closed —
// callers may rely on this ordering.
func (a *TerminalAttach) Exited() <-chan *pb.TerminalAttachExited { return a.exited }

// Input writes raw bytes to the PTY. Returns ErrAttachClosed if Close has
// been called.
func (a *TerminalAttach) Input(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	select {
	case <-a.closing:
		return ErrAttachClosed
	default:
	}
	a.inputMu.Lock()
	defer a.inputMu.Unlock()
	_, err := a.ptyFd.Write(data)
	if err != nil {
		return fmt.Errorf("pty write: %w", err)
	}
	return nil
}

// Resize updates the PTY winsize. Returns ErrAttachClosed if Close has been
// called.
func (a *TerminalAttach) Resize(cols, rows uint32) error {
	select {
	case <-a.closing:
		return ErrAttachClosed
	default:
	}
	if cols == 0 || rows == 0 {
		return fmt.Errorf("resize requires non-zero cols and rows (got cols=%d rows=%d)", cols, rows)
	}
	err := creackpty.Setsize(a.ptyFd, &creackpty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		return fmt.Errorf("pty setsize: %w", err)
	}
	return nil
}

// Close terminates the tmux client process, releases the PTY, and waits for
// the three internal goroutines to exit. Idempotent — additional calls
// return the original close error (if any).
func (a *TerminalAttach) Close() error {
	a.closeOnce.Do(func() {
		close(a.closing)
		// Cancel the child's context so SIGKILL fires if the process doesn't
		// notice the closed PTY.
		a.cancel()
		// Closing the PTY makes the readLoop's read return an error and
		// unblocks any pending pty.Write.
		_ = a.ptyFd.Close()
		a.wg.Wait()
	})
	return a.closeErr
}

// readLoop reads from the PTY into the ring buffer in a tight loop. It MUST
// NOT block on consumers — the ring buffer drops oldest bytes on overflow.
// On EOF / error it closes readDone so the sender loop can flush + exit.
func (a *TerminalAttach) readLoop() {
	defer a.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			a.log.Error().Interface("panic", r).Msg("tmux attach readLoop panic")
		}
		close(a.readDone)
		// One last wakeup so the sender drains anything still buffered.
		a.notifyDataReady()
	}()

	buf := make([]byte, ptyReadChunkSize)
	for {
		n, err := a.ptyFd.Read(buf)
		if n > 0 {
			a.ringBuf.Write(buf[:n])
			a.notifyDataReady()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				a.log.Debug().Err(err).Msg("pty read ended")
			}
			return
		}
	}
}

// senderLoop drains the ring buffer onto Output(). It exits once the read
// loop has finished AND the buffer is empty, then closes Output so consumers
// know no more data is coming. Closing `outputDone` immediately after
// `output` lets waitLoop block on it before signalling Exited, preserving
// the documented ordering invariant.
func (a *TerminalAttach) senderLoop() {
	defer a.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			a.log.Error().Interface("panic", r).Msg("tmux attach senderLoop panic")
		}
		close(a.output)
		close(a.outputDone)
	}()

	for {
		// Drain whatever is currently in the buffer before waiting for more.
		for {
			data, lost := a.ringBuf.ReadChunk(maxChunkBytes)
			if len(data) == 0 && !lost {
				break
			}
			a.seq++
			chunk := &pb.TerminalDataChunk{
				AttachId: a.cfg.AttachID,
				Seq:      a.seq,
				Data:     data,
				Lost:     lost,
			}
			// Block here is OK — Output is buffered; the consumer (Task A3)
			// pulls at gRPC speed. Backpressure manifests upstream: the
			// readLoop keeps writing into the ring buffer, dropping oldest
			// bytes on overflow. This is the design.
			select {
			case a.output <- chunk:
			case <-a.closing:
				return
			}
		}

		select {
		case <-a.dataReady:
			// New data arrived (or readLoop is exiting); loop and drain.
		case <-a.closing:
			return
		case <-a.readDone:
			// Read loop is done. Drain any remaining bytes one last time
			// then exit. We re-run the inner drain by continuing the outer
			// loop; the next dataReady select will fall through to readDone
			// again with an empty buffer.
			data, lost := a.ringBuf.ReadChunk(maxChunkBytes)
			if len(data) > 0 || lost {
				a.seq++
				chunk := &pb.TerminalDataChunk{
					AttachId: a.cfg.AttachID,
					Seq:      a.seq,
					Data:     data,
					Lost:     lost,
				}
				select {
				case a.output <- chunk:
				case <-a.closing:
					return
				}
				continue
			}
			return
		}
	}
}

// waitLoop blocks on cmd.Wait, then synthesises a TerminalAttachExited frame
// and pushes it onto Exited(). It serialises with the data path before
// signalling: Output() is closed before the Exited frame is delivered, so
// downstream consumers can rely on the documented ordering.
func (a *TerminalAttach) waitLoop() {
	defer a.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			a.log.Error().Interface("panic", r).Msg("tmux attach waitLoop panic")
		}
	}()

	err := a.cmd.Wait()
	a.closeErr = err
	// Release the context resources held by the cmd's parent context,
	// regardless of whether Close was called.
	a.cancel()

	exitCode := int32(0)
	reason := ""
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = int32(exitErr.ExitCode())
			reason = exitErr.String()
		} else {
			exitCode = -1
			reason = err.Error()
		}
	}

	// Closing the PTY ends the readLoop in case tmux exited cleanly without
	// closing its end of the pty itself. This intentionally overlaps with
	// Close()'s ptyFd.Close above the closeOnce: this branch handles the
	// natural-EOF teardown where Close was never called by the caller, so
	// we still need to release the FD here. When Close DID fire first, this
	// returns os.ErrClosed which we discard — both paths converge to "FD
	// is closed exactly once."
	_ = a.ptyFd.Close()

	// Wait for the data path to fully drain and close before signalling exit
	// so consumers can rely on: receive Exited → Output is already closed.
	// readDone fires when readLoop returns; outputDone fires when senderLoop
	// closes Output(). Both must complete before we publish the exit frame.
	<-a.readDone
	<-a.outputDone

	frame := &pb.TerminalAttachExited{
		AttachId: a.cfg.AttachID,
		ExitCode: exitCode,
		Reason:   reason,
	}
	select {
	case a.exited <- frame:
	case <-a.closing:
	}
}

// notifyDataReady wakes the senderLoop. Coalesces multiple notifications
// while the sender is mid-drain into a single wakeup.
func (a *TerminalAttach) notifyDataReady() {
	select {
	case a.dataReady <- struct{}{}:
	default:
	}
}

// ringBuffer is a fixed-size circular byte buffer with drop-oldest overflow
// semantics. ReadChunk returns at most `max` bytes plus a `lost` flag that
// is true ONLY for the first chunk emitted after an overflow event — it is
// then cleared, so subsequent chunks set lost=false until the next overflow.
type ringBuffer struct {
	mu   sync.Mutex
	buf  []byte
	size int
	// head is the read index; tail is the write index. count is the number
	// of valid bytes currently in the buffer (0 ≤ count ≤ size).
	head  int
	tail  int
	count int
	// lost is set when Write drops oldest bytes; ReadChunk reads it and
	// clears it on the next emission. The flag does NOT persist past one
	// emit per the plan ("On overflow, drop oldest, set lost=true. Next sent
	// frame includes kind=5 resync_needed").
	lost bool
}

func newRingBuffer(size int) *ringBuffer {
	if size <= 0 {
		size = defaultRingBufferSize
	}
	return &ringBuffer{
		buf:  make([]byte, size),
		size: size,
	}
}

// Write appends p to the buffer, dropping the oldest bytes if it would
// overflow. Always succeeds and never blocks. Sets the `lost` flag if any
// bytes were dropped.
//
// If len(p) is larger than the entire ring, only the most recent `size`
// bytes are retained — this matches the "drop oldest" intent.
func (r *ringBuffer) Write(p []byte) {
	if len(p) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// If p alone is larger than the ring, only the tail of p fits. Drop
	// everything currently in the buffer and keep only the last r.size
	// bytes of p.
	if len(p) >= r.size {
		copy(r.buf, p[len(p)-r.size:])
		r.head = 0
		r.tail = 0 // tail wraps to 0 when count == size
		// Only set lost=true if bytes were actually dropped: either we
		// already had buffered content (now overwritten) or p itself was
		// strictly larger than the ring (tail truncated). When the buffer
		// was empty AND len(p) == r.size, the write fits exactly and no
		// bytes are lost.
		if r.count > 0 || len(p) > r.size {
			r.lost = true
		}
		r.count = r.size
		return
	}

	// Will writing len(p) overflow? If so, advance head to drop oldest.
	free := r.size - r.count
	if len(p) > free {
		drop := len(p) - free
		r.head = (r.head + drop) % r.size
		r.count -= drop
		r.lost = true
	}

	// Copy p into the ring at tail, in up to two segments.
	n := copy(r.buf[r.tail:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
	}
	r.tail = (r.tail + len(p)) % r.size
	r.count += len(p)
}

// ReadChunk returns up to `max` bytes from the buffer and reports whether
// the buffer overflowed since the last ReadChunk call. The lost flag is
// cleared on read. Returns (nil, false) when the buffer is empty and no
// overflow has occurred.
func (r *ringBuffer) ReadChunk(max int) (data []byte, lost bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 && !r.lost {
		return nil, false
	}

	n := r.count
	if n > max {
		n = max
	}
	out := make([]byte, n)
	if n > 0 {
		// Copy in up to two segments depending on whether the read wraps.
		first := r.size - r.head
		if first > n {
			first = n
		}
		copy(out, r.buf[r.head:r.head+first])
		if n > first {
			copy(out[first:], r.buf[:n-first])
		}
		r.head = (r.head + n) % r.size
		r.count -= n
	}

	lost = r.lost
	r.lost = false
	return out, lost
}

// Len returns the number of buffered bytes (test helper).
func (r *ringBuffer) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// ensureTerm returns env with TERM set to xterm-256color when it is absent or
// set to an unusable value ("" or "dumb"). tmux exits immediately if it cannot
// initialise its terminal driver, which happens in CI environments where TERM
// is unset or dumb. A copy of env is returned to avoid mutating the caller's
// slice.
func ensureTerm(env []string) []string {
	for i, e := range env {
		if !strings.HasPrefix(e, "TERM=") {
			continue
		}
		val := strings.TrimPrefix(e, "TERM=")
		if val == "" || val == "dumb" {
			out := make([]string, len(env))
			copy(out, env)
			out[i] = "TERM=xterm-256color"
			return out
		}
		return env
	}
	return append(append([]string(nil), env...), "TERM=xterm-256color")
}
