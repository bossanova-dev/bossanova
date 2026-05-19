// Package agentruntime manages coding-agent CLI subprocess lifecycles.
//
// The Runner handles the generic machinery shared by all agent plugins
// (claude, codex, etc.): per-session process tracking, ring-buffered
// output history, fan-out to live subscribers, NDJSON log capture,
// graceful SIGTERM-then-SIGKILL shutdown, and diagnostic preamble/exit
// markers in the log file. The agent-specific argv is supplied by the
// caller through Options.BuildArgv, so each plugin owns its own CLI
// flag conventions without duplicating the rest of the lifecycle code.
package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
)

// DefaultRingBufferSize is the number of output lines kept in memory per session.
const DefaultRingBufferSize = 1000

// OutputLine is a single line of output from an agent process.
type OutputLine struct {
	Text      string
	Timestamp time.Time
}

// CommandFactory creates exec.Cmd instances. Allows injection for testing.
type CommandFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// BuildArgvInput describes the per-Start data the caller-supplied argv
// builder needs to construct a full agent CLI invocation. Callers see
// only this struct and decide which fields apply to their agent.
type BuildArgvInput struct {
	WorkDir           string
	Plan              string
	Resume            *string
	SessionID         string
	ProvidedSessionID bool
	LogPath           string
	Options           map[string]string
}

// Options configures a Runner. BuildArgv is required; everything else is
// optional with sensible defaults.
type Options struct {
	// BuildArgv returns the full argv (including binary) to exec for a
	// session. Required. Must return a non-empty slice.
	BuildArgv func(BuildArgvInput) []string
	// BinaryName is the human-readable agent name used in diagnostic
	// log preambles ("[runner] spawning <name>: ..."). Defaults to "agent".
	BinaryName string
	// BufSize overrides the per-session ring buffer line count.
	// Zero means DefaultRingBufferSize.
	BufSize int

	// PostExit, if non-nil, is called after cmd.Wait returns with a
	// non-nil error. The hook receives the original exit error and a
	// tail of the per-session log (last ~8KB). If the hook returns a
	// non-nil error it replaces the original exit error reported via
	// ExitError(sessionID).
	//
	// Used by the codex plugin to upgrade a generic non-zero exit into
	// a typed ErrAuthRequired when the log tail contains the "401
	// Unauthorized" / "Missing bearer" markers.
	PostExit func(originalErr error, logTail []byte) error

	// SessionIDFromOutput, if non-nil, is given the first ~8KB of stdout
	// shortly after the subprocess starts and may return a discovered
	// session ID. When the hook returns a non-empty string, that value
	// is what Runner.Start returns to the caller — overriding any
	// caller-provided session ID hint.
	//
	// Used by the codex plugin: codex generates its own UUID and emits
	// it via a `thread.started` JSONL event on stdout, so the runner
	// must wait for that line before returning the canonical session ID
	// to bossd. The hook is called once when either ~8KB has accumulated
	// or a brief timeout elapses (~500ms), whichever comes first.
	SessionIDFromOutput func(stdoutTail []byte) string
}

// Runner is the per-plugin agent subprocess manager.
type Runner struct {
	mu                  sync.RWMutex
	procs               map[string]*process
	cmdFunc             CommandFactory
	logger              zerolog.Logger
	bufSize             int
	buildArgv           func(BuildArgvInput) []string
	binName             string
	postExit            func(originalErr error, logTail []byte) error
	sessionIDFromOutput func(stdoutTail []byte) string
}

// process tracks a running agent subprocess.
type process struct {
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	sessionID string
	workDir   string
	ring      *ringBuffer
	subs      *subscribers
	logFile   *os.File
	logPath   string        // for PostExit log-tail reads
	done      chan struct{} // closed when process exits
	exitErr   error

	// earlyOutput is a bounded ring of the first earlyOutputCap bytes of
	// subprocess stdout+stderr, plus a one-shot signal channel that
	// closes when the buffer is full. Used by SessionIDFromOutput to
	// discover an agent-generated session ID without keeping the runner
	// blocked on the entire run. Nil when SessionIDFromOutput is unset
	// so the existing tests pay zero cost.
	earlyOutputMu     sync.Mutex
	earlyOutput       []byte
	earlyOutputReady  chan struct{}
	earlyOutputFull   chan struct{}
	earlyOutputActive bool
}

// earlyOutputCap is the maximum number of stdout/stderr bytes accumulated
// before the SessionIDFromOutput hook is invoked. 8 KB easily covers a
// codex `thread.started` event, which appears on the first line of
// `codex exec --json` output (~150 bytes).
const earlyOutputCap = 8 * 1024

// earlyOutputTimeout bounds how long Runner.Start waits for the early
// output buffer to produce a discoverable session ID. Two seconds still keeps
// StartRun responsive while avoiding false misses on loaded machines where
// process startup and stdout copy scheduling can exceed half a second.
const earlyOutputTimeout = 2 * time.Second

// Option configures a Runner via NewRunner's variadic options.
type Option func(*Runner)

// WithCommandFactory overrides the command factory (for testing).
func WithCommandFactory(f CommandFactory) Option {
	return func(r *Runner) { r.cmdFunc = f }
}

// NewRunner creates a new agent process runner.
func NewRunner(logger zerolog.Logger, opts Options, extra ...Option) *Runner {
	r := &Runner{
		procs:               make(map[string]*process),
		cmdFunc:             exec.CommandContext,
		logger:              logger,
		bufSize:             defaultBufSize(opts.BufSize),
		buildArgv:           opts.BuildArgv,
		binName:             defaultBinName(opts.BinaryName),
		postExit:            opts.PostExit,
		sessionIDFromOutput: opts.SessionIDFromOutput,
	}
	for _, opt := range extra {
		opt(r)
	}
	return r
}

func defaultBufSize(n int) int {
	if n <= 0 {
		return DefaultRingBufferSize
	}
	return n
}

func defaultBinName(s string) string {
	if s == "" {
		return "agent"
	}
	return s
}

// Start spawns an agent CLI process. The argv is constructed by the
// Options.BuildArgv closure; the runner pipes `plan` to the subprocess's
// stdin, captures stdout+stderr line-by-line into the NDJSON log at
// logPath, and tracks the process under sessionID.
//
// The log file is opened with O_NOFOLLOW to defeat symlink/TOCTOU
// attacks on the configured log path.
//
// Diagnostic NDJSON entries with `[runner]` prefixes are written at three
// points: (1) immediately before cmd.Start, recording the args, cwd and
// PATH so "<binary> not found on PATH" is diagnosable in one read; (2) on
// cmd.Start failure, recording the OS error before the log file is
// closed; (3) on cmd.Wait returning a non-nil error, recording the exit
// reason. These exist because the log file used to remain at 0 bytes
// when the subprocess died before producing output, leaving no on-disk
// record of why a run failed.
func (r *Runner) Start(ctx context.Context, workDir, plan string, resume *string, sessionID, logPath string) (string, error) {
	// Determine whether the caller provided a session ID.
	providedSessionID := sessionID != ""

	// If no session ID provided, generate one from timestamp.
	if sessionID == "" {
		sessionID = fmt.Sprintf("%s-%d", r.binName, time.Now().UnixNano())
	}

	// Hold the runner mutex from the existence check through the
	// r.procs[sessionID] = p insertion. Releasing the lock between the two
	// (the previous shape) opened a TOCTOU window: two concurrent Start
	// calls with the same sessionID could both pass the check, both run
	// the setup below, and the second insertion would silently overwrite
	// the first — orphaning that process because its cancel func is no
	// longer reachable via Stop(). The setup is bounded I/O (mkdir, log
	// file open, exec.Cmd construction); Start runs at most once per
	// session so holding the lock through it is acceptable.
	//
	// `locked` plus the deferred unlock catches every error-path return
	// below; the success path explicitly unlocks after the insertion so
	// cmd.Start and the subsequent goroutines run without the lock held.
	r.mu.Lock()
	locked := true
	defer func() {
		if locked {
			r.mu.Unlock()
		}
	}()
	if _, exists := r.procs[sessionID]; exists {
		return "", fmt.Errorf("session %s already exists", sessionID)
	}

	procCtx, cancel := context.WithCancel(ctx)

	if r.buildArgv == nil {
		cancel()
		return "", fmt.Errorf("agentruntime: BuildArgv is required")
	}
	argv := r.buildArgv(BuildArgvInput{
		WorkDir:           workDir,
		Plan:              plan,
		Resume:            resume,
		SessionID:         sessionID,
		ProvidedSessionID: providedSessionID,
		LogPath:           logPath,
	})
	if len(argv) == 0 {
		cancel()
		return "", fmt.Errorf("agentruntime: BuildArgv returned empty argv")
	}

	cmd := r.cmdFunc(procCtx, argv[0], argv[1:]...)
	cmd.Dir = workDir
	// On context cancellation, send SIGTERM for graceful shutdown. If the
	// process doesn't exit within WaitDelay, Go's os/exec sends SIGKILL
	// automatically. This matches Stop()'s documented 10s-then-force-kill
	// behaviour.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	// Pipe plan to stdin.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("create stdin pipe: %w", err)
	}

	// logPath is supplied by the caller (bossd). Ensure the parent dir exists,
	// then open with O_NOFOLLOW to refuse a symlink at the final path
	// component (defends against a hostile or buggy agent process planting a
	// symlink at our log path).
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		cancel()
		return "", fmt.Errorf("create log dir: %w", err)
	}

	logFile, err := openLogNoFollow(logPath)
	if err != nil {
		cancel()
		return "", err
	}

	p := &process{
		cmd:       cmd,
		cancel:    cancel,
		sessionID: sessionID,
		workDir:   workDir,
		ring:      newRingBuffer(r.bufSize),
		subs:      newSubscribers(),
		logFile:   logFile,
		logPath:   logPath,
		done:      make(chan struct{}),
	}
	if r.sessionIDFromOutput != nil {
		p.earlyOutput = make([]byte, 0, earlyOutputCap)
		p.earlyOutputReady = make(chan struct{}, 1)
		p.earlyOutputFull = make(chan struct{})
		p.earlyOutputActive = true
	}

	// Use lineWriter for stdout/stderr so that Go's internal I/O copying
	// completes before cmd.Wait returns — eliminating the race between
	// goroutine scheduling and fast process exits on CI.
	lw := &lineWriter{proc: p, logFile: logFile}
	cmd.Stdout = lw
	cmd.Stderr = lw

	r.procs[sessionID] = p
	r.mu.Unlock()
	locked = false

	// Write the spawn preamble before cmd.Start so even an immediate exec
	// failure leaves a useful trace.
	writeRunnerEntry(logFile, fmt.Sprintf(
		"[runner] spawning %s: argv=%v cwd=%s sessionID=%s PATH=%s",
		r.binName, argv, workDir, sessionID, truncatePath(os.Getenv("PATH")),
	))

	r.logger.Info().
		Str("agent", r.binName).
		Str("session", sessionID).
		Str("workDir", workDir).
		Msg("starting agent process")

	if err := cmd.Start(); err != nil {
		writeRunnerEntry(logFile, fmt.Sprintf("[runner] cmd.Start failed: %v", err))
		cancel()
		_ = logFile.Close()
		r.mu.Lock()
		delete(r.procs, sessionID)
		r.mu.Unlock()
		return "", fmt.Errorf("start %s: %w", r.binName, err)
	}

	// Write plan to stdin, then close.
	safego.Go(r.logger, func() {
		defer func() { _ = stdin.Close() }()
		_, _ = io.WriteString(stdin, plan)
	})

	// Wait for process exit.
	safego.Go(r.logger, func() {
		exitErr := cmd.Wait()
		lw.flush() // emit any trailing partial line

		// PostExit hook: only invoked when the subprocess returned an
		// error and the caller registered a hook. Read up to 8KB from
		// the log tail (the same window we use for early output) and
		// give the hook a chance to upgrade the error to a typed value.
		if exitErr != nil && r.postExit != nil {
			tail := readLogTail(p.logPath, earlyOutputCap)
			if replacement := r.postExit(exitErr, tail); replacement != nil {
				exitErr = replacement
			}
		}
		p.exitErr = exitErr

		if p.exitErr != nil {
			// Surface non-zero exit / signal in the log file itself, not
			// just zerolog. The repair-debug workflow reads this file.
			writeRunnerEntry(p.logFile, fmt.Sprintf("[runner] exited: %v", p.exitErr))
		} else {
			writeRunnerEntry(p.logFile, "[runner] exited cleanly")
		}
		_ = p.logFile.Close()
		p.subs.close()
		close(p.done)

		// Read p.sessionID under the runner mutex so the SessionIDFromOutput
		// path (which may have re-keyed the process) doesn't race with the
		// log emission below.
		r.mu.RLock()
		loggedSession := p.sessionID
		r.mu.RUnlock()
		r.logger.Info().
			Str("agent", r.binName).
			Str("session", loggedSession).
			Err(p.exitErr).
			Msg("agent process exited")
	})

	// SessionIDFromOutput discovery: wait briefly for the subprocess to emit
	// stdout/stderr, retrying the hook as fresh bytes arrive. If the hook
	// returns a non-empty session ID, that becomes our return value —
	// overriding the caller-provided hint. Plugins like codex use this to
	// surface the agent-generated UUID emitted on the `thread.started` event.
	if r.sessionIDFromOutput != nil {
		timer := time.NewTimer(earlyOutputTimeout)
		defer timer.Stop()

		for doneWaiting := false; !doneWaiting; {
			select {
			case <-p.earlyOutputReady:
			case <-p.earlyOutputFull:
				doneWaiting = true
			case <-p.done:
				doneWaiting = true
			case <-timer.C:
				doneWaiting = true
			}

			snapshot := p.snapshotEarlyOutput()
			if discovered := r.sessionIDFromOutput(snapshot); discovered != "" {
				// Re-key the process map so future Stop/IsRunning/ExitError
				// calls find the process under the canonical session ID.
				r.mu.Lock()
				delete(r.procs, sessionID)
				p.sessionID = discovered
				r.procs[discovered] = p
				r.mu.Unlock()
				sessionID = discovered
				break
			}
		}
		p.stopEarlyOutputCapture()
	}

	return sessionID, nil
}

func (p *process) snapshotEarlyOutput() []byte {
	p.earlyOutputMu.Lock()
	defer p.earlyOutputMu.Unlock()
	snapshot := make([]byte, len(p.earlyOutput))
	copy(snapshot, p.earlyOutput)
	return snapshot
}

func (p *process) stopEarlyOutputCapture() {
	p.earlyOutputMu.Lock()
	p.earlyOutputActive = false
	p.earlyOutputMu.Unlock()
}

// readLogTail returns up to n bytes from the end of the file at path.
// Returns nil on any error (best-effort: the PostExit hook either has
// content to inspect or has none).
func readLogTail(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	size := info.Size()
	var offset int64
	if size > n {
		offset = size - n
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	return tail
}

// writeRunnerEntry emits one NDJSON line to f matching the format used by
// lineWriter (`{"ts":"...","text":"..."}`). Errors are deliberately
// swallowed: this is a best-effort diagnostic helper, and an unwritable log
// file is already a worse problem the caller will have to surface.
func writeRunnerEntry(f *os.File, text string) {
	if f == nil {
		return
	}
	entry := struct {
		TS   string `json:"ts"`
		Text string `json:"text"`
	}{
		TS:   time.Now().UTC().Format(time.RFC3339Nano),
		Text: text,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		data = []byte(`{"ts":"","text":"<encode error>"}`)
	}
	_, _ = f.Write(data)
	_, _ = f.Write([]byte{'\n'})
}

// truncatePath shortens a PATH-like string for log output. Full PATH values
// can exceed 1KB on macOS dev machines; logging the head is enough to spot
// the obvious case ("<binary> isn't in any of these dirs").
func truncatePath(path string) string {
	const maxLen = 256
	if len(path) <= maxLen {
		return path
	}
	return path[:maxLen] + "...(truncated)"
}

// Stop terminates the agent process for the given session. It cancels the
// process context (which sends SIGTERM via cmd.Cancel) and waits for the
// process to exit. If graceful shutdown takes longer than 10 seconds it
// force-kills the process and waits for the exit goroutine to observe it.
func (r *Runner) Stop(sessionID string) error {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	p.cancel()

	select {
	case <-p.done:
		return nil
	case <-time.After(10 * time.Second):
	}

	// Graceful shutdown timed out — force kill and wait for the exit
	// goroutine to actually observe the process exit.
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	<-p.done
	return nil
}

// IsRunning reports whether an agent process is active for the session.
func (r *Runner) IsRunning(sessionID string) bool {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return false
	}

	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Wait blocks until the tracked process exits or ctx is cancelled. It returns
// the same completed-process error as ExitError.
func (r *Runner) Wait(ctx context.Context, sessionID string) error {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return nil
	}

	select {
	case <-p.done:
		return p.exitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ExitError returns the exit error for a completed session.
func (r *Runner) ExitError(sessionID string) error {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return nil
	}

	select {
	case <-p.done:
		return p.exitErr
	default:
		return nil // still running
	}
}

// Subscribe returns a channel that receives output lines for the session.
func (r *Runner) Subscribe(ctx context.Context, sessionID string) (<-chan OutputLine, error) {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	ch := p.subs.add(ctx)
	return ch, nil
}

// History returns the buffered output lines for a session.
func (r *Runner) History(sessionID string) []OutputLine {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return nil
	}

	return p.ring.lines()
}

// lineWriter splits subprocess output into lines and writes one NDJSON
// entry per line: {"ts": "<RFC3339Nano>", "text": "<line>"}. Using
// cmd.Stdout/cmd.Stderr = lineWriter ensures Go's internal I/O copy
// completes before cmd.Wait returns, eliminating races with goroutine
// scheduling.
type lineWriter struct {
	mu      sync.Mutex
	proc    *process
	logFile *os.File
	buf     []byte
}

func (lw *lineWriter) Write(data []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	n := len(data)
	lw.buf = append(lw.buf, data...)

	// Tee into the early-output buffer used by SessionIDFromOutput. We
	// hold the per-process mutex independently of the lineWriter mutex
	// so the goroutine reading the buffer in Start cannot deadlock with
	// fresh subprocess writes.
	if lw.proc.earlyOutputReady != nil {
		lw.proc.earlyOutputMu.Lock()
		if lw.proc.earlyOutputActive {
			remaining := earlyOutputCap - len(lw.proc.earlyOutput)
			toWrite := data
			if len(toWrite) > remaining {
				toWrite = toWrite[:remaining]
			}
			if len(toWrite) > 0 {
				lw.proc.earlyOutput = append(lw.proc.earlyOutput, toWrite...)
				select {
				case lw.proc.earlyOutputReady <- struct{}{}:
				default:
				}
			}
			if len(lw.proc.earlyOutput) >= earlyOutputCap {
				lw.proc.earlyOutputActive = false
				close(lw.proc.earlyOutputFull)
			}
		}
		lw.proc.earlyOutputMu.Unlock()
	}

	for {
		idx := bytes.IndexByte(lw.buf, '\n')
		if idx < 0 {
			break
		}
		text := string(lw.buf[:idx])
		lw.buf = lw.buf[idx+1:]
		ts := time.Now()

		line := OutputLine{Text: text, Timestamp: ts}
		lw.proc.ring.add(line)
		lw.writeNDJSON(text, ts)
		lw.proc.subs.broadcast(line)
	}

	return n, nil
}

// flush emits any remaining partial line in the buffer.
func (lw *lineWriter) flush() {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	if len(lw.buf) == 0 {
		return
	}
	text := string(lw.buf)
	lw.buf = nil
	ts := time.Now()

	line := OutputLine{Text: text, Timestamp: ts}
	lw.proc.ring.add(line)
	lw.writeNDJSON(text, ts)
	lw.proc.subs.broadcast(line)
}

func (lw *lineWriter) writeNDJSON(text string, ts time.Time) {
	entry := struct {
		TS   string `json:"ts"`
		Text string `json:"text"`
	}{
		TS:   ts.UTC().Format(time.RFC3339Nano),
		Text: text,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		// Fall back to a synthetic placeholder so the log line ordering
		// is preserved even if marshalling somehow fails.
		data = []byte(`{"ts":"","text":"<encode error>"}`)
	}
	_, _ = lw.logFile.Write(data)
	_, _ = lw.logFile.Write([]byte{'\n'})
}

// ErrLogPathSymlink is returned when the configured log path resolves
// through a symlink. We refuse to follow symlinks defensively.
var ErrLogPathSymlink = errors.New("log path is a symlink; refusing to follow")

// openLogNoFollow opens path for writing, creating it if needed,
// refusing to follow a symlink at the final path component.
func openLogNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		// Linux returns ELOOP, BSD/Darwin returns EMLINK for O_NOFOLLOW.
		var pe *os.PathError
		if errors.As(err, &pe) {
			if pe.Err == syscall.ELOOP || pe.Err == syscall.EMLINK {
				return nil, fmt.Errorf("%w: %s", ErrLogPathSymlink, path)
			}
		}
		return nil, fmt.Errorf("open log: %w", err)
	}
	return f, nil
}

// --- Ring Buffer ---

// ringBuffer is a fixed-size circular buffer of OutputLine entries.
type ringBuffer struct {
	mu    sync.RWMutex
	buf   []OutputLine
	size  int
	head  int // next write position
	count int // total items written (for overflow detection)
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{
		buf:  make([]OutputLine, size),
		size: size,
	}
}

// add appends a line to the ring buffer.
func (rb *ringBuffer) add(line OutputLine) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.buf[rb.head] = line
	rb.head = (rb.head + 1) % rb.size
	rb.count++
}

// lines returns all stored lines in chronological order.
func (rb *ringBuffer) lines() []OutputLine {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if rb.count == 0 {
		return nil
	}

	n := min(rb.count, rb.size)

	result := make([]OutputLine, n)
	if rb.count <= rb.size {
		// Buffer hasn't wrapped yet.
		copy(result, rb.buf[:n])
	} else {
		// Buffer has wrapped: read from head (oldest) to end, then start to head.
		start := rb.head // oldest entry
		copied := copy(result, rb.buf[start:])
		copy(result[copied:], rb.buf[:start])
	}

	return result
}

// --- Subscribers ---

// subscribers manages broadcast channels for output streaming.
type subscribers struct {
	mu     sync.RWMutex
	chans  []chan OutputLine
	closed bool
}

func newSubscribers() *subscribers {
	return &subscribers{}
}

// add creates a new subscription channel. The channel is removed when ctx is cancelled.
func (s *subscribers) add(ctx context.Context) <-chan OutputLine {
	ch := make(chan OutputLine, 64)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		close(ch)
		return ch
	}
	s.chans = append(s.chans, ch)
	s.mu.Unlock()

	// Remove the channel when the context is cancelled.
	go func() {
		<-ctx.Done()
		s.remove(ch)
	}() // no safego: trivial cleanup, cannot panic

	return ch
}

// broadcast sends a line to all subscriber channels.
func (s *subscribers) broadcast(line OutputLine) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, ch := range s.chans {
		select {
		case ch <- line:
		default:
			// Slow consumer — drop the line rather than blocking.
		}
	}
}

// close closes all subscriber channels.
func (s *subscribers) close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	for _, ch := range s.chans {
		close(ch)
	}
	s.chans = nil
}

// remove removes a specific channel from subscribers.
func (s *subscribers) remove(ch chan OutputLine) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, c := range s.chans {
		if c == ch {
			s.chans = append(s.chans[:i], s.chans[i+1:]...)
			close(ch)
			return
		}
	}
}
