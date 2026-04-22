// Package log provides shared logging initialization for Bossanova services.
package log

import (
	"io"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Setup configures zerolog with human-friendly console output and a service tag.
// Logs are mirrored to stderr and a rotated file under $XDG_STATE_HOME/bossanova/logs/<service>.log
// (or ~/.local/state/bossanova/logs/<service>.log as a fallback). File-open failures never
// block startup; the logger falls back to stderr-only and emits a single warning.
//
// The returned io.Closer must be closed at shutdown to stop lumberjack's rotation
// goroutine. It is always non-nil (a no-op when file logging is disabled) so
// callers can `defer closer.Close()` without a nil check.
func Setup(service string) io.Closer {
	return setup(service, true)
}

// SetupFileOnly configures zerolog to write only to the rotated log file,
// never to stderr. Use this for interactive processes (like the boss TUI)
// where stderr output would corrupt the UI. Semantics otherwise match Setup.
func SetupFileOnly(service string) io.Closer {
	return setup(service, false)
}

func setup(service string, includeStderr bool) io.Closer {
	var (
		writer io.Writer
		closer io.Closer = noopCloser{}
	)
	if includeStderr {
		writer = zerolog.ConsoleWriter{Out: os.Stderr}
	} else {
		writer = io.Discard
	}

	path := LogPath(service)
	fileFallback := false
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			if includeStderr {
				log.Warn().Err(err).Str("path", path).Msg("log: could not create log dir, falling back to stderr-only")
			}
			fileFallback = !includeStderr
		} else {
			fileWriter := &lumberjack.Logger{
				Filename:   path,
				MaxSize:    5,
				MaxBackups: 1,
				Compress:   false,
			}
			if includeStderr {
				writer = io.MultiWriter(writer, fileWriter)
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
		writer = zerolog.ConsoleWriter{Out: os.Stderr}
	}

	log.Logger = zerolog.New(writer).With().Timestamp().Str("service", service).Logger()
	if fileFallback {
		log.Warn().Str("service", service).Msg("log: could not open rotated log file, falling back to stderr (TUI output may be garbled)")
	}
	return closer
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// LogPath returns the path to the rotated log file for a service. Returns "" if
// no suitable state directory can be determined.
func LogPath(service string) string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "bossanova", "logs", service+".log")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "bossanova", "logs", service+".log")
}
