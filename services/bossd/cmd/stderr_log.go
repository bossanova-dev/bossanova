package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	bossalog "github.com/recurser/bossalib/kit/log"
	"github.com/recurser/bossd/internal/adapter/stderrcap"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// stderrSupervision is what bossd decides about its own fd 2 before the
// logger exists. A launchd-supervised stderr file (BOS-2171) mirrors only
// warn+ — the rotated bossd.log already holds every line — and is capped by
// copy-truncate. Any other stderr (a terminal, /dev/null, a shell redirect)
// keeps the full mirror and is never truncated.
type stderrSupervision struct {
	capper         *stderrcap.Capper
	logOpts        []bossalog.Option
	startupTrimErr error
}

// superviseStderr checks stderr against the plist's BOSSD_STDERR_LOG_PATH and,
// when it is the supervised file, trims it once before any new line lands.
func superviseStderr(getenv func(string) string, stderr *os.File) stderrSupervision {
	toStderr := bossalog.WithStderr(stderr)
	path, ok := stderrcap.Supervised(getenv, stderr)
	if !ok {
		return stderrSupervision{logOpts: []bossalog.Option{toStderr}}
	}
	capper := &stderrcap.Capper{Path: path, File: stderr, Limit: stderrcap.DefaultLimit}
	_, err := capper.Trim()
	return stderrSupervision{
		capper:         capper,
		logOpts:        []bossalog.Option{toStderr, bossalog.WithStderrLevel(zerolog.WarnLevel)},
		startupTrimErr: err,
	}
}

// setupLogging installs bossd's logger and, when supervised, the cap loop.
// Cleanups run in reverse, so the cap loop is joined before the log closes.
func (w *daemonWiring) setupLogging(getenv func(string) string, stderr *os.File) {
	supervision := superviseStderr(getenv, stderr)
	// Console logging plus the rotated file at $XDG_STATE_HOME/bossanova/logs/bossd.log.
	logCloser := bossalog.Setup("bossd", supervision.logOpts...)
	w.deferCleanup(func() { _ = logCloser.Close() })
	if supervision.capper == nil {
		return
	}
	if supervision.startupTrimErr != nil {
		log.Warn().Err(supervision.startupTrimErr).Str("path", supervision.capper.Path).Msg("stderrcap: could not cap the supervised stderr log at startup")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := supervision.capper.Start(ctx, stderrcap.DefaultInterval, log.Logger)
	w.deferCleanup(func() {
		cancel()
		<-done
	})
}

// writeFatal reports run()'s error straight to fd 2, never through the
// logger: a supervised logger filters by level, and this line is the one an
// operator opens bossd.stderr.log to find. The RFC 3339 stamp dates it like
// the console lines around it.
func writeFatal(out io.Writer, now time.Time, err error) {
	_, _ = fmt.Fprintf(out, "%s bossd: %v\n", now.Format(time.RFC3339), err)
}
