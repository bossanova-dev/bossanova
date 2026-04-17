package claude

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/tmux"
)

var _ ClaudeRunner = (*TmuxRunner)(nil)

// TmuxRunner runs Claude sessions inside tmux for autopilot observability.
// If tmux is unavailable, it falls back to the headless Runner.
type TmuxRunner struct {
	mu         sync.RWMutex
	procs      map[string]*tmuxProcess
	tmux       *tmux.Client
	fallback   *Runner
	logger     zerolog.Logger
	bufSize    int
	logDir     string
	configPath string
}

// tmuxProcess tracks a Claude process running inside a tmux session.
type tmuxProcess struct {
	sessionID  string
	tmuxName   string
	workDir    string
	ring       *ringBuffer
	subs       *subscribers
	done       chan struct{}
	tailDone   chan struct{} // closed when tailLogFile goroutine has finished
	exitErr    error
	isFallback bool // true when using headless Runner
}

// TmuxRunnerOption configures a TmuxRunner.
type TmuxRunnerOption func(*TmuxRunner)

// WithTmuxLogDir overrides the log file directory.
func WithTmuxLogDir(dir string) TmuxRunnerOption {
	return func(r *TmuxRunner) { r.logDir = dir }
}

// WithTmuxConfigPath overrides the config file path.
func WithTmuxConfigPath(path string) TmuxRunnerOption {
	return func(r *TmuxRunner) { r.configPath = path }
}

// WithTmuxFallback overrides the fallback headless runner (for testing).
func WithTmuxFallback(runner *Runner) TmuxRunnerOption {
	return func(r *TmuxRunner) { r.fallback = runner }
}

// NewTmuxRunner creates a new TmuxRunner.
func NewTmuxRunner(tmuxClient *tmux.Client, logger zerolog.Logger, opts ...TmuxRunnerOption) *TmuxRunner {
	r := &TmuxRunner{
		procs:   make(map[string]*tmuxProcess),
		tmux:    tmuxClient,
		logger:  logger,
		bufSize: DefaultRingBufferSize,
	}
	for _, opt := range opts {
		opt(r)
	}
	// Build fallback runner with matching options if not provided.
	if r.fallback == nil {
		var runnerOpts []RunnerOption
		if r.logDir != "" {
			runnerOpts = append(runnerOpts, WithLogDir(r.logDir))
		}
		if r.configPath != "" {
			runnerOpts = append(runnerOpts, WithConfigPath(r.configPath))
		}
		r.fallback = NewRunner(logger, runnerOpts...)
	}
	return r
}

// tmuxSessionName returns the tmux session name for an autopilot session.
func tmuxSessionName(sessionID string) string {
	short := sessionID
	if len(short) > 12 {
		short = short[:12]
	}
	return "autopilot-" + short
}

// Start spawns a Claude CLI process inside a tmux session.
func (r *TmuxRunner) Start(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error) {
	// Check tmux availability — fall back to headless if unavailable.
	if !r.tmux.Available(ctx) {
		r.logger.Warn().Msg("tmux unavailable, falling back to headless runner")
		sid, err := r.fallback.Start(ctx, workDir, plan, resume, sessionID)
		if err != nil {
			return "", err
		}
		r.mu.Lock()
		r.procs[sid] = &tmuxProcess{
			sessionID:  sid,
			isFallback: true,
			done:       make(chan struct{}),
		}
		r.mu.Unlock()
		return sid, nil
	}

	providedSessionID := sessionID != ""
	if sessionID == "" {
		sessionID = fmt.Sprintf("claude-%d", time.Now().UnixNano())
	}

	r.mu.Lock()
	if _, exists := r.procs[sessionID]; exists {
		r.mu.Unlock()
		return "", fmt.Errorf("session %s already exists", sessionID)
	}
	r.mu.Unlock()

	// Build Claude args.
	args := []string{"--print", "--verbose", "--output-format", "stream-json"}
	if resume != nil {
		args = append(args, "--resume", *resume)
	}
	if providedSessionID {
		args = append(args, "--session-id", sessionID)
	}

	var cfg config.Settings
	if r.configPath != "" {
		cfg, _ = config.LoadFrom(r.configPath)
	} else {
		cfg, _ = config.Load()
	}
	if cfg.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}

	// Determine log directory.
	logDir := r.logDir
	if logDir == "" {
		logDir = filepath.Join(workDir, ".boss")
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", fmt.Errorf("create log dir: %w", err)
	}

	planPath := filepath.Join(logDir, sessionID+".plan")
	logPath := filepath.Join(logDir, sessionID+".log")
	donePath := filepath.Join(logDir, sessionID+".done")

	// Write plan file.
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		return "", fmt.Errorf("write plan file: %w", err)
	}

	// Build the shell wrapper command.
	// Read the plan from the plan file via shell stdin redirect (not a pipe,
	// which can be unreliable inside tmux sessions) and tee output to log file.
	// Write exit code to done file on completion.
	// Use bash with pipefail so $? reflects claude's exit code, not tee's.
	quotedArgs := make([]string, len(args))
	for i, a := range args {
		quotedArgs[i] = shellQuote(a)
	}
	claudeCmd := "claude " + strings.Join(quotedArgs, " ") + " < " + shellQuote(planPath)
	wrapper := fmt.Sprintf(
		"set -o pipefail; %s 2>&1 | tee -a %s; echo $? > %s",
		claudeCmd, shellQuote(logPath), shellQuote(donePath),
	)

	tmuxName := tmuxSessionName(sessionID)

	p := &tmuxProcess{
		sessionID: sessionID,
		tmuxName:  tmuxName,
		workDir:   workDir,
		ring:      newRingBuffer(r.bufSize),
		subs:      newSubscribers(),
		done:      make(chan struct{}),
		tailDone:  make(chan struct{}),
	}

	r.mu.Lock()
	r.procs[sessionID] = p
	r.mu.Unlock()

	r.logger.Info().
		Str("session", sessionID).
		Str("tmux", tmuxName).
		Str("workDir", workDir).
		Msg("starting claude in tmux session")

	// Create tmux session.
	if err := r.tmux.NewSession(ctx, tmux.NewSessionOpts{
		Name:    tmuxName,
		WorkDir: workDir,
		Command: []string{"bash", "-c", wrapper},
	}); err != nil {
		r.mu.Lock()
		delete(r.procs, sessionID)
		r.mu.Unlock()
		return "", fmt.Errorf("create tmux session: %w", err)
	}

	// Start log tail goroutine.
	safego.Go(r.logger, func() {
		r.tailLogFile(p, logPath)
	})

	// Start completion polling goroutine.
	safego.Go(r.logger, func() {
		r.pollCompletion(ctx, p, donePath)
	})

	return sessionID, nil
}

// tailLogFile reads the log file in a polling loop and feeds lines into
// the ring buffer and subscribers.
func (r *TmuxRunner) tailLogFile(p *tmuxProcess, logPath string) {
	defer close(p.tailDone)

	var offset int64
	buf := make([]byte, 4096)
	var partial []byte

	for {
		select {
		case <-p.done:
			// Process is done — do one final read to catch remaining output.
			r.readLogChunk(p, logPath, &offset, buf, &partial)
			// Flush any remaining partial line.
			if len(partial) > 0 {
				line := OutputLine{Text: string(partial), Timestamp: time.Now()}
				p.ring.add(line)
				p.subs.broadcast(line)
			}
			return
		case <-time.After(100 * time.Millisecond):
			r.readLogChunk(p, logPath, &offset, buf, &partial)
		}
	}
}

// readLogChunk reads new data from the log file starting at offset.
func (r *TmuxRunner) readLogChunk(p *tmuxProcess, logPath string, offset *int64, buf []byte, partial *[]byte) {
	f, err := os.Open(logPath)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(*offset, 0); err != nil {
		return
	}

	for {
		n, err := f.Read(buf)
		if n == 0 {
			break
		}
		*offset += int64(n)

		// Split into lines.
		data := append(*partial, buf[:n]...)
		*partial = nil

		for {
			idx := bytes.IndexByte(data, '\n')
			if idx < 0 {
				*partial = append([]byte(nil), data...)
				break
			}
			text := string(data[:idx])
			data = data[idx+1:]

			line := OutputLine{Text: text, Timestamp: time.Now()}
			p.ring.add(line)
			p.subs.broadcast(line)
		}

		if err != nil {
			break
		}
	}
}

// completeProcess signals process completion, waits for the tail goroutine
// to flush remaining output, then closes subscribers.
func (r *TmuxRunner) completeProcess(p *tmuxProcess) {
	close(p.done)
	<-p.tailDone
	p.subs.close()
}

// pollCompletion polls for the done sentinel file or tmux session exit.
func (r *TmuxRunner) pollCompletion(ctx context.Context, p *tmuxProcess, donePath string) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Kill the tmux session to avoid orphaned processes.
			if err := r.tmux.KillSession(context.Background(), p.tmuxName); err != nil {
				r.logger.Warn().Err(err).
					Str("session", p.sessionID).
					Str("tmux", p.tmuxName).
					Msg("failed to kill tmux session on context cancellation")
			}
			p.exitErr = ctx.Err()
			r.completeProcess(p)
			r.logger.Info().
				Str("session", p.sessionID).
				Msg("context cancelled, tmux autopilot session killed")
			return
		case <-ticker.C:
			// Check for done file.
			if data, err := os.ReadFile(donePath); err == nil {
				code, _ := strconv.Atoi(strings.TrimSpace(string(data)))
				if code != 0 {
					p.exitErr = fmt.Errorf("claude exited with code %d", code)
				}
				r.completeProcess(p)
				r.logger.Info().
					Str("session", p.sessionID).
					Int("exitCode", code).
					Msg("tmux autopilot session completed")
				return
			}

			// Fallback: check if tmux session still exists.
			if !r.tmux.HasSession(ctx, p.tmuxName) {
				p.exitErr = fmt.Errorf("tmux session %s disappeared without writing done file", p.tmuxName)
				r.completeProcess(p)
				r.logger.Warn().
					Str("session", p.sessionID).
					Str("tmux", p.tmuxName).
					Msg("tmux session gone without done file")
				return
			}
		}
	}
}

// Stop terminates the Claude process for the given session.
func (r *TmuxRunner) Stop(sessionID string) error {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	if p.isFallback {
		err := r.fallback.Stop(sessionID)
		r.mu.Lock()
		delete(r.procs, sessionID)
		r.mu.Unlock()
		return err
	}

	if err := r.tmux.KillSession(context.Background(), p.tmuxName); err != nil {
		return err
	}

	// Wait for completion goroutine to detect the session is gone.
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timed out waiting for session %s to stop", sessionID)
	}

	return nil
}

// IsRunning reports whether a Claude process is active for the session.
func (r *TmuxRunner) IsRunning(sessionID string) bool {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return false
	}

	if p.isFallback {
		return r.fallback.IsRunning(sessionID)
	}

	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// ExitError returns the exit error for a completed session.
func (r *TmuxRunner) ExitError(sessionID string) error {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return nil
	}

	if p.isFallback {
		return r.fallback.ExitError(sessionID)
	}

	select {
	case <-p.done:
		return p.exitErr
	default:
		return nil
	}
}

// Subscribe returns a channel that receives output lines for the session.
func (r *TmuxRunner) Subscribe(ctx context.Context, sessionID string) (<-chan OutputLine, error) {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	if p.isFallback {
		return r.fallback.Subscribe(ctx, sessionID)
	}

	ch := p.subs.add(ctx)
	return ch, nil
}

// History returns the buffered output lines for a session.
func (r *TmuxRunner) History(sessionID string) []OutputLine {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return nil
	}

	if p.isFallback {
		return r.fallback.History(sessionID)
	}

	return p.ring.lines()
}

// shellQuote wraps a string in single quotes for safe shell use.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
