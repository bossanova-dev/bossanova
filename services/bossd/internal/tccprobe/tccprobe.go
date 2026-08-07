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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// ProtectedRootsFor returns the macOS TCC-guarded roots (~/Documents, ~/Desktop,
// ~/Downloads) that contain at least one of paths. The match is lexical; callers
// that need symlinks resolved must resolve the candidates first.
func ProtectedRootsFor(home string, paths []string) []string {
	roots := []string{
		filepath.Join(home, "Documents"),
		filepath.Join(home, "Desktop"),
		filepath.Join(home, "Downloads"),
	}
	var selected []string
	for _, root := range roots {
		for _, candidate := range paths {
			relative, err := filepath.Rel(root, candidate)
			if err != nil {
				continue
			}
			if relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				selected = append(selected, root)
				break
			}
		}
	}
	return selected
}

// Remedy returns the operator-facing advice for a root that probed Blocked or
// Denied. It is the single source of this advice inside bossd: the startup log
// line and the RepairDoctor check share it so the two cannot drift apart.
//
// It is not the only such text in the product — `boss daemon doctor`
// (services/boss/cmd/daemon_doctor.go) prints its own remediation from the
// probe results persisted at boot, and cannot import this package (it is a
// different module, and this package is internal to bossd). Reconciling the
// two surfaces is a separate change.
//
// A blocked probe only degrades discovery under root; it does not mean the
// daemon is broken or that sessions on that path will fail, and reinstalling
// the binary is not a remedy (TCC grants are tied to the installed path, not
// the bytes).
//
// Exactly one route clears with no daemon restart: answering the pending
// dialog, which unblocks the read already in flight. A Full Disk Access grant
// added in System Settings is not applied to an already-running process, and a
// root the startup scan found stays in the doctor's probed set until the daemon
// restarts — so relocating fixes the degradation but not the reported root. The
// on-demand re-probe removes the restart from the answer-the-dialog loop; it
// does not make macOS re-evaluate a running process's grants.
//
// status selects the routes that actually apply. Blocked means a dialog is
// pending, so answering it is the fastest fix; Denied means TCC already
// returned a decision and there is no dialog to answer, so offering one would
// send the operator down a dead end. Everything else gets the Denied wording.
func Remedy(root, executable string, status Status) string {
	remedy := fmt.Sprintf(
		"Discovery under %s is degraded, not the daemon: macOS is withholding filesystem access from %s for this root. "+
			"Best fix: relocate the repository/worktree base out of ~/Documents, ~/Desktop and ~/Downloads (for example, to ~/src/...) — that needs no grant and no display at all. ",
		root, executable,
	)
	if status == StatusBlocked {
		remedy += "A permission dialog is pending: answering it clears this at once, but nobody can answer it over SSH. "
	}
	return remedy + fmt.Sprintf(
		"Otherwise grant Files-and-Folders/Full Disk Access to %s in System Settings > Privacy & Security (reachable remotely via Screen Sharing or MDM), then run 'boss daemon restart' — macOS does not apply a new Full Disk Access grant to an already-running process.",
		executable,
	)
}
