// Package tccprobe checks whether macOS TCC-protected directories are readable.
//
// A filesystem syscall can remain blocked in __open_nocancel after Probe times
// out. That leaves one permanent goroutine per root, once per daemon startup;
// the timeout still prevents startup itself from blocking.
package tccprobe

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
)

// DefaultTimeout bounds one directory probe.
const DefaultTimeout = 3 * time.Second

// Status describes the result of probing a directory.
type Status uint8

const (
	StatusOK Status = iota
	StatusDenied
	StatusBlocked
	StatusAbsent
	StatusError
)

// Result contains the result for one root.
type Result struct {
	Path   string
	Status Status
	Err    error
}

var (
	readDirMu sync.RWMutex
	readDir   = os.ReadDir
)

// WorkerTracker receives a probe worker's completion channel so callers can
// retain blocked filesystem operations for shutdown coordination.
type WorkerTracker func(done <-chan struct{})

// Probe checks each root without allowing an uninterruptible filesystem call to
// block its caller.
func Probe(ctx context.Context, roots []string, timeout time.Duration) []Result {
	return ProbeWithTracker(ctx, roots, timeout, nil)
}

// ProbeWithTracker is Probe with lifecycle tracking for workers that can remain
// blocked in an uninterruptible filesystem call after the diagnostic returns.
func ProbeWithTracker(ctx context.Context, roots []string, timeout time.Duration, track WorkerTracker) []Result {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	results := make([]Result, 0, len(roots))
	for _, root := range roots {
		results = append(results, probe(ctx, root, timeout, track))
	}
	return results
}

func probe(ctx context.Context, root string, timeout time.Duration, track WorkerTracker) Result {
	readDirMu.RLock()
	reader := readDir
	readDirMu.RUnlock()

	type readResult struct{ err error }
	completed := make(chan readResult, 1)
	done := safego.Go(zerolog.Nop(), func() {
		_, err := reader(root)
		completed <- readResult{err: err}
	})
	if track != nil {
		track(done)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case result := <-completed:
		// A buffered result is sent before the worker returns. Observe the
		// returned lifecycle channel too, so a completed probe cannot outlive
		// the diagnostic that reports it.
		<-done
		return Result{Path: root, Status: statusFor(result.err), Err: result.err}
	case <-ctx.Done():
		// A completed read can race with cancellation. Prefer its concrete
		// result when its goroutine has already exited; otherwise the worker is
		// still observable through done and is genuinely blocked at this point.
		select {
		case <-done:
			result := <-completed
			return Result{Path: root, Status: statusFor(result.err), Err: result.err}
		default:
		}
		return Result{Path: root, Status: StatusBlocked, Err: ctx.Err()}
	case <-timer.C:
		select {
		case <-done:
			result := <-completed
			return Result{Path: root, Status: statusFor(result.err), Err: result.err}
		default:
		}
		return Result{Path: root, Status: StatusBlocked, Err: context.DeadlineExceeded}
	}
}

func statusFor(err error) Status {
	switch {
	case err == nil:
		return StatusOK
	case errors.Is(err, fs.ErrNotExist):
		return StatusAbsent
	case errors.Is(err, fs.ErrPermission):
		return StatusDenied
	default:
		return StatusError
	}
}
