// Package claude manages Claude CLI subprocess lifecycle for coding sessions.
package claude

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/safego"
)

// DefaultRingBufferSize is the number of output lines kept in memory per session.
const DefaultRingBufferSize = 1000

// OutputLine is a single line of output from a Claude process.
type OutputLine struct {
	Text      string
	Timestamp time.Time
}

// ClaudeRunner manages Claude CLI subprocesses.
type ClaudeRunner interface {
	// Start spawns a Claude CLI process in workDir with the given plan.
	// If resume is non-nil, it resumes an existing Claude session.
	// If sessionID is non-empty, it is passed via --session-id and used as the tracking key.
	// When sessionID is empty, a generated claude-<timestamp> ID is used instead.
	// Returns the session ID assigned to this process.
	Start(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error)

	// Stop terminates the Claude process for the given session.
	Stop(sessionID string) error

	// IsRunning reports whether a Claude process is active for the session.
	IsRunning(sessionID string) bool

	// ExitError returns the exit error for a completed session.
	// Returns nil if the session is still running, exited successfully,
	// or is unknown.
	ExitError(sessionID string) error

	// Subscribe returns a channel that receives output lines for the session.
	// The channel is closed when the process exits or the caller cancels ctx.
	Subscribe(ctx context.Context, sessionID string) (<-chan OutputLine, error)

	// History returns the buffered output lines for a session.
	History(sessionID string) []OutputLine
}

// CommandFactory creates exec.Cmd instances. Allows injection for testing.
type CommandFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

var _ ClaudeRunner = (*Runner)(nil)

// Runner is the default ClaudeRunner implementation.
type Runner struct {
	mu         sync.RWMutex
	procs      map[string]*process
	cmdFunc    CommandFactory
	logger     zerolog.Logger
	bufSize    int
	logDir     string // if set, write log files here; otherwise use workDir/.boss/
	configPath string // if set, load config from this path; otherwise use default
}

// process tracks a running Claude subprocess.
type process struct {
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	sessionID string
	workDir   string
	ring      *ringBuffer
	subs      *subscribers
	logFile   *os.File
	done      chan struct{} // closed when process exits
	exitErr   error
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithCommandFactory overrides the command factory (for testing).
func WithCommandFactory(f CommandFactory) RunnerOption {
	return func(r *Runner) { r.cmdFunc = f }
}

// WithLogDir overrides the log file directory (for testing).
func WithLogDir(dir string) RunnerOption {
	return func(r *Runner) { r.logDir = dir }
}

// WithConfigPath overrides the config file path (for testing).
func WithConfigPath(path string) RunnerOption {
	return func(r *Runner) { r.configPath = path }
}

// NewRunner creates a new Claude process runner.
func NewRunner(logger zerolog.Logger, opts ...RunnerOption) *Runner {
	r := &Runner{
		procs:   make(map[string]*process),
		cmdFunc: exec.CommandContext,
		logger:  logger,
		bufSize: DefaultRingBufferSize,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Start spawns a Claude CLI process. It runs `claude --print --output-format=stream-json`
// in the given workDir, piping the plan to stdin.
func (r *Runner) Start(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error) {
	// Determine whether the caller provided a session ID.
	providedSessionID := sessionID != ""

	// If no session ID provided, generate one from timestamp.
	if sessionID == "" {
		sessionID = fmt.Sprintf("claude-%d", time.Now().UnixNano())
	}

	r.mu.Lock()
	if _, exists := r.procs[sessionID]; exists {
		r.mu.Unlock()
		return "", fmt.Errorf("session %s already exists", sessionID)
	}
	r.mu.Unlock()

	// Build command args.
	args := []string{"--print", "--verbose", "--output-format", "stream-json"}
	if resume != nil {
		args = append(args, "--resume", *resume)
	}
	// When sessionID was explicitly provided by the caller, pass it to Claude CLI.
	// This makes the Claude Code session use the provided ID instead of generating its own.
	if providedSessionID {
		args = append(args, "--session-id", sessionID)
	}

	// Read global config for optional flags.
	var cfg config.Settings
	if r.configPath != "" {
		cfg, _ = config.LoadFrom(r.configPath)
	} else {
		cfg, _ = config.Load()
	}
	if cfg.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	procCtx, cancel := context.WithCancel(ctx)
	cmd := r.cmdFunc(procCtx, "claude", args...)
	cmd.Dir = workDir

	// Pipe plan to stdin.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("create stdin pipe: %w", err)
	}

	// Open log file.
	logPath := r.logPath(workDir, sessionID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		cancel()
		return "", fmt.Errorf("create log dir: %w", err)
	}

	logFile, err := os.Create(logPath)
	if err != nil {
		cancel()
		return "", fmt.Errorf("create log file: %w", err)
	}

	p := &process{
		cmd:       cmd,
		cancel:    cancel,
		sessionID: sessionID,
		workDir:   workDir,
		ring:      newRingBuffer(r.bufSize),
		subs:      newSubscribers(),
		logFile:   logFile,
		done:      make(chan struct{}),
	}

	// Use lineWriter for stdout/stderr so that Go's internal I/O copying
	// completes before cmd.Wait returns — eliminating the race between
	// goroutine scheduling and fast process exits on CI.
	lw := &lineWriter{proc: p, logFile: logFile}
	cmd.Stdout = lw
	cmd.Stderr = lw

	r.mu.Lock()
	r.procs[sessionID] = p
	r.mu.Unlock()

	r.logger.Info().
		Str("session", sessionID).
		Str("workDir", workDir).
		Msg("starting claude process")

	if err := cmd.Start(); err != nil {
		cancel()
		_ = logFile.Close()
		r.mu.Lock()
		delete(r.procs, sessionID)
		r.mu.Unlock()
		return "", fmt.Errorf("start claude: %w", err)
	}

	// Write plan to stdin, then close.
	safego.Go(r.logger, func() {
		defer func() { _ = stdin.Close() }()
		_, _ = io.WriteString(stdin, plan)
	})

	// Wait for process exit.
	safego.Go(r.logger, func() {
		p.exitErr = cmd.Wait()
		lw.flush() // emit any trailing partial line
		_ = p.logFile.Close()
		p.subs.close()
		close(p.done)
		r.logger.Info().
			Str("session", sessionID).
			Err(p.exitErr).
			Msg("claude process exited")
	})

	return sessionID, nil
}

// Stop terminates the Claude process for the given session.
func (r *Runner) Stop(sessionID string) error {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	p.cancel()

	// Wait for process to exit with a timeout.
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		// Force kill if graceful shutdown didn't work.
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	}

	return nil
}

// IsRunning reports whether a Claude process is active for the session.
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

// logPath returns the log file path for a session.
func (r *Runner) logPath(workDir, sessionID string) string {
	if r.logDir != "" {
		return filepath.Join(r.logDir, sessionID+".log")
	}
	return filepath.Join(workDir, ".boss", "claude.log")
}

// lineWriter is an io.Writer that splits output into lines and feeds them
// into the ring buffer, log file, and subscriber channels. Using cmd.Stdout/
// cmd.Stderr = lineWriter ensures Go's internal I/O copy completes before
// cmd.Wait returns, eliminating races with goroutine scheduling.
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

	for {
		idx := bytes.IndexByte(lw.buf, '\n')
		if idx < 0 {
			break
		}
		text := string(lw.buf[:idx])
		lw.buf = lw.buf[idx+1:]

		line := OutputLine{Text: text, Timestamp: time.Now()}
		lw.proc.ring.add(line)
		_, _ = fmt.Fprintf(lw.logFile, "%s\n", text)
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

	line := OutputLine{Text: text, Timestamp: time.Now()}
	lw.proc.ring.add(line)
	_, _ = fmt.Fprintf(lw.logFile, "%s\n", text)
	lw.proc.subs.broadcast(line)
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

	n := rb.count
	if n > rb.size {
		n = rb.size
	}

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
