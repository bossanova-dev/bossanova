// Package tccprobe checks whether macOS TCC-protected directories are readable.
//
// A filesystem syscall can remain blocked in __open_nocancel after Probe times
// out. That leaves one permanent goroutine — and, because it is parked in a
// syscall, one OS thread — per blocked root, per Probe call. The timeout still
// prevents the caller from blocking.
//
// Probe is called from two places, with different lifetimes (BOS-725):
//
//   - Once at daemon startup, via ProbeWithTracker, whose WorkerTracker joins
//     the stranded workers to shutdown coordination.
//   - On every RepairDoctor invocation, so an operator who grants access sees
//     the check clear without a daemon restart. That path deliberately passes
//     no tracker: a permanently-blocked worker must not be able to wedge
//     daemon shutdown, and its cost is bounded by how often a human (or an
//     agent, via the repair_doctor MCP tool) asks. On a host that is genuinely
//     TCC-blocked this accumulates one stranded thread per blocked root per
//     call, which is the price of an on-demand answer.
package tccprobe

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

// DefaultTimeout bounds one directory probe.
const DefaultTimeout = domain.TCCDefaultTimeout

// Status describes the result of probing a directory.
type Status = domain.TCCStatus

const (
	StatusOK      = domain.TCCStatusOK
	StatusDenied  = domain.TCCStatusDenied
	StatusBlocked = domain.TCCStatusBlocked
	StatusAbsent  = domain.TCCStatusAbsent
	StatusError   = domain.TCCStatusError
)

// Result contains the result for one root.
type Result = domain.TCCResult

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

// ProtectedRootsFor selects the protected roots containing the working paths.
func ProtectedRootsFor(home string, paths []string) []string {
	return domain.TCCProtectedRootsFor(home, paths)
}

// Remedy returns the shared operator-facing advice for a protected root.
func Remedy(root, executable string, status Status) string {
	return domain.TCCRemedy(root, executable, status)
}
