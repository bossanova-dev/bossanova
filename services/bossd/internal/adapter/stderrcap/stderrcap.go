// Package stderrcap bounds the file a service manager appends bossd's stderr to.
//
// launchd opens the LaunchAgent's StandardErrorPath with O_APPEND and never
// rotates it. When bossd recognises that file as its own fd 2 (Supervised),
// a Capper copies it to <path>.1 once it passes Limit and truncates it in
// place. Copy-truncate, not rename: truncation acts on the inode, so every
// holder of the shared open file description — bossd and any child that
// inherited fd 2 — keeps appending at the new end, while a rename would leave
// them writing to the backup. O_APPEND is what makes that safe; a non-append
// writer would keep its old offset and leave a sparse hole, so Trim refuses
// one.
//
// Lines written between the copy and the truncate are lost. The window is a
// few milliseconds and this is a backstop, so that is accepted. The live file
// stays under Limit plus one interval's growth, plus one backup of that size.
package stderrcap

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
)

const (
	// EnvVar names the stderr file the service manager redirects fd 2 into.
	// The bossd LaunchAgent plist sets it to its own StandardErrorPath.
	EnvVar = "BOSSD_STDERR_LOG_PATH"
	// DefaultLimit is the size past which the supervised stderr file is trimmed.
	DefaultLimit int64 = 10 << 20
	// DefaultInterval is how often a running daemon re-checks the size.
	DefaultInterval = 5 * time.Minute
)

// Supervised reports whether stderr is the file getenv(EnvVar) names. A stale
// variable, a detached spawn (stderr on /dev/null) or a shell redirect to a
// different file all report false, so none of them is filtered or truncated.
func Supervised(getenv func(string) string, stderr *os.File) (string, bool) {
	path := getenv(EnvVar)
	if path == "" || stderr == nil {
		return "", false
	}
	named, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	held, err := stderr.Stat()
	if err != nil {
		return "", false
	}
	return path, os.SameFile(named, held)
}

// Capper trims File, which must be the open handle for Path, once it exceeds Limit.
type Capper struct {
	Path  string
	File  *os.File
	Limit int64
}

// Trim copies Path to Path.1 and truncates it in place when it is over Limit.
// It reports whether it truncated. It leaves the file alone when the handle is
// not a regular O_APPEND file or Path no longer names the handle's file.
func (c Capper) Trim() (bool, error) {
	held, err := c.File.Stat()
	if err != nil {
		return false, fmt.Errorf("stat stderr: %w", err)
	}
	if !held.Mode().IsRegular() || held.Size() <= c.Limit || !isAppendOnly(c.File) {
		return false, nil
	}
	if same, err := c.namesHeld(held); err != nil || !same {
		return false, err
	}
	if err := c.backup(); err != nil {
		return false, err
	}
	// Re-check: the path may have been replaced while the copy ran, and the
	// truncate must hit the file the copy just preserved.
	if held, err = c.File.Stat(); err != nil {
		return false, fmt.Errorf("stat stderr: %w", err)
	}
	if same, err := c.namesHeld(held); err != nil || !same {
		return false, err
	}
	if err := c.File.Truncate(0); err != nil {
		return false, fmt.Errorf("truncate %s: %w", c.Path, err)
	}
	return true, nil
}

func (c Capper) namesHeld(held os.FileInfo) (bool, error) {
	named, err := os.Stat(c.Path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", c.Path, err)
	}
	return os.SameFile(named, held), nil
}

func (c Capper) backup() error {
	tmp := c.Path + ".1.tmp"
	if err := copyFile(c.Path, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, c.Path+".1"); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename backup: %w", err)
	}
	return nil
}

func copyFile(src, dst string) error {
	// #nosec G304 -- src is the service manager's stderr file, verified SameFile with fd 2; not attacker-named
	// owner=@recurser review-by=2027-05-02 issue=BOS-2171
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	// #nosec G304 -- dst is src with a fixed .1.tmp suffix beside the verified stderr file
	// owner=@recurser review-by=2027-05-02 issue=BOS-2171
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create backup: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy to backup: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close backup: %w", err)
	}
	return nil
}

// TrimAndReport runs Trim and turns its failure into a single warning, so a
// cap that cannot be applied never blocks or fails the daemon.
func (c Capper) TrimAndReport(logger zerolog.Logger) {
	trimmed, err := c.Trim()
	if err != nil {
		logger.Warn().Err(err).Str("path", c.Path).Msg("stderrcap: could not cap the supervised stderr log")
		return
	}
	if trimmed {
		logger.Info().Str("path", c.Path).Int64("limit_bytes", c.Limit).Msg("stderrcap: copied the supervised stderr log to .1 and truncated it")
	}
}

// Start re-checks the cap every interval until ctx is done. The returned
// channel closes when the loop has exited; the caller must wait on it.
func (c Capper) Start(ctx context.Context, interval time.Duration, logger zerolog.Logger) <-chan struct{} {
	return safego.Go(logger, func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.TrimAndReport(logger)
			}
		}
	})
}
