// Package log provides shared logging initialization for Bossanova services.
package log

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/term"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Option adjusts how Setup builds the logger.
type Option func(*options)

type options struct {
	stderr      io.Writer
	stderrLevel zerolog.Level
}

// WithStderrLevel mirrors only events at level or above to stderr. The rotated
// file still receives every level. A supervised daemon uses it so the file its
// service manager appends stderr to does not duplicate the rotated log.
func WithStderrLevel(level zerolog.Level) Option {
	return func(o *options) { o.stderrLevel = level }
}

// WithStderr replaces os.Stderr as the console destination. A caller that has
// already inspected its stderr handle passes that handle; tests pass a buffer
// so they never reassign the process-wide os.Stderr.
func WithStderr(w io.Writer) Option {
	return func(o *options) { o.stderr = w }
}

// consoleWriter renders console lines with an RFC 3339 timestamp (full date,
// seconds, offset), the same shape as the JSON time field in the rotated file,
// so a console line and its file twin pair by exact string. Colour escapes are
// emitted only to a terminal: a service manager's stderr file is plain text.
func consoleWriter(out io.Writer) zerolog.ConsoleWriter {
	return zerolog.ConsoleWriter{Out: out, TimeFormat: time.RFC3339, NoColor: !isTerminal(out)}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Setup configures zerolog with human-friendly console output and a service tag.
// Logs are mirrored to stderr and a rotated file under $XDG_STATE_HOME/bossanova/logs/<service>.log
// (or ~/.local/state/bossanova/logs/<service>.log as a fallback). File-open failures never
// block startup; the logger falls back to stderr-only and emits a single warning.
// Retention is up to 20 rotated 5 MB backups plus the live file (~105 MB per service),
// or roughly two days at the measured bossd rate of about 2 MB/hour.
//
// The returned io.Closer must be closed at shutdown to stop lumberjack's rotation
// goroutine. It is always non-nil (a no-op when file logging is disabled) so
// callers can `defer closer.Close()` without a nil check.
//
// Console lines carry an RFC 3339 timestamp and no colour when stderr is not a
// terminal. WithStderrLevel narrows what reaches stderr without narrowing the file.
func Setup(service string, opts ...Option) io.Closer {
	return setup(service, true, opts...)
}

// SetupFileOnly configures zerolog to write only to the rotated log file,
// never to stderr. Use this for interactive processes (like the boss TUI)
// where stderr output would corrupt the UI. Semantics otherwise match Setup.
func SetupFileOnly(service string) io.Closer {
	return setup(service, false)
}

func setup(service string, includeStderr bool, opts ...Option) io.Closer {
	o := options{stderr: os.Stderr, stderrLevel: zerolog.TraceLevel}
	for _, opt := range opts {
		opt(&o)
	}
	var (
		writer io.Writer
		closer io.Closer = noopCloser{}
	)
	if includeStderr {
		// Unfiltered until a file sink exists: with no rotated file, stderr is
		// the only record, so no level is withheld from it.
		writer = consoleWriter(o.stderr)
	} else {
		writer = io.Discard
	}

	path := Path(service)
	fileFallback := false
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			if includeStderr {
				log.Warn().Err(err).Str("path", path).Msg("log: could not create log dir, falling back to stderr-only")
			}
			fileFallback = !includeStderr
		} else {
			fileWriter := &closedSink{writer: newRotatingFile(path)}
			if includeStderr {
				writer = fileFirstWriter{file: fileWriter, stderr: writer, stderrLevel: o.stderrLevel}
			} else {
				writer = fileWriter
			}
			closer = fileWriter
		}
	} else {
		// File-only mode with no resolvable state dir (no $XDG_STATE_HOME
		// and os.UserHomeDir failed) would otherwise silently discard all
		// logs, which defeats the reason file-only mode exists — we chose
		// it because the TUI can't tolerate stderr, but losing every log
		// on a misconfigured host is strictly worse than a corrupted frame.
		fileFallback = !includeStderr
	}
	if fileFallback {
		writer = consoleWriter(o.stderr)
	}

	log.Logger = zerolog.New(writer).With().Timestamp().Str("service", service).Logger()
	if fileFallback {
		log.Warn().Str("service", service).Msg("log: could not open rotated log file, falling back to stderr (TUI output may be garbled)")
	}
	return closer
}

const (
	maxLogSizeMB  = 5
	maxLogBackups = 20
)

func newRotatingFile(path string) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   path,
		MaxSize:    maxLogSizeMB,
		MaxBackups: maxLogBackups,
		// Readers discover -*.log backups and require io.ReaderAt; keep them uncompressed.
		Compress: false,
	}
}

// fileFirstWriter sends every event to the file sink and mirrors only events
// at stderrLevel or above to stderr. The rotated file opens lazily, so an event
// the file sink rejects (EACCES, read-only FS, ENOSPC) reaches stderr whatever
// its level: no event lands in neither sink.
type fileFirstWriter struct {
	file        io.Writer
	stderr      io.Writer
	stderrLevel zerolog.Level
}

func (w fileFirstWriter) Write(p []byte) (int, error) {
	return w.WriteLevel(zerolog.NoLevel, p)
}

func (w fileFirstWriter) WriteLevel(level zerolog.Level, p []byte) (int, error) {
	_, fileErr := w.file.Write(p)
	if fileErr == nil && level < w.stderrLevel {
		return len(p), nil
	}
	if _, err := w.stderr.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// closedSink prevents lumberjack from reopening its file after shutdown.
type closedSink struct {
	mu     sync.Mutex
	writer io.WriteCloser
	closed bool
	err    error
}

func (s *closedSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return len(p), nil
	}
	return s.writer.Write(p)
}

func (s *closedSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.err = s.writer.Close()
	}
	return s.err
}

// Dir returns the directory holding rotated service logs. Returns "" if no
// suitable state directory can be determined.
func Dir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "bossanova", "logs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "bossanova", "logs")
}

// Path returns the path to the rotated log file for a service. Returns "" if
// no suitable state directory can be determined.
func Path(service string) string {
	dir := Dir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, service+".log")
}

// AgentLogsDir returns the directory holding per-agent-session logs, given the
// configured worktree base directory. Agent logs deliberately sit beside the
// worktree root rather than under it, so a worktree checkout cannot reach them.
// Unlike the service log dir they are not rotated, and they hold agent output
// in either of two formats (see the logtail package). Returns "" when no
// worktree base directory is configured.
func AgentLogsDir(worktreeBaseDir string) string {
	if worktreeBaseDir == "" {
		return ""
	}
	// Clean first: filepath.Dir("…/worktrees/") is "…/worktrees", which would
	// nest agent logs inside the worktree root and defeat the isolation above.
	return filepath.Join(filepath.Dir(filepath.Clean(worktreeBaseDir)), "agent-logs")
}

// AgentLogFile names one agent session's log inside an already-resolved
// agent-logs directory. This is the single definition of the <id>.log
// convention every producer and reader of these logs shares — bossd's agent
// runs and tmux chats, and `boss tail <agent-session-id>`. Returns "" for an
// empty dir, so a caller that has no agent-logs directory configured keeps
// seeing the empty path its own guards already handle.
func AgentLogFile(agentLogsDir, agentSessionID string) string {
	if agentLogsDir == "" {
		return ""
	}
	return filepath.Join(agentLogsDir, agentSessionID+".log")
}

// AgentLogPath returns the log path for a single agent session. Returns "" when
// no worktree base directory is configured.
func AgentLogPath(worktreeBaseDir, agentSessionID string) string {
	return AgentLogFile(AgentLogsDir(worktreeBaseDir), agentSessionID)
}
