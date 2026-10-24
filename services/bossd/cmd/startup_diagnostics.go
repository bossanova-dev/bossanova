package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/adapter/tccprobe"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/session"
)

// logRotationLaneAvailability emits the single operator-facing startup
// diagnostic describing whether the auto-rotation lane can actually fire
// (BOS-315). The message literally contains the HasLiveRotationSeams token so
// the line is greppable. Fail-soft: on a repo-list error it still logs the
// seams + kill-switch fields with zero counts and never blocks startup.
func logRotationLaneAvailability(logger zerolog.Logger, hasSeams, rotationEnabled bool, repos []*models.Repo, listErr error) {
	ev := logger.Info().
		Bool("has_live_rotation_seams", hasSeams).
		Bool("rotation_enabled", rotationEnabled)
	autoRotate, optedOut := 0, 0
	if listErr != nil {
		ev = ev.AnErr("repo_count_error", listErr)
	} else {
		for _, repo := range repos {
			if repo.CanAutoRotate {
				autoRotate++
			} else {
				optedOut++
			}
		}
	}
	ev.Int("auto_rotate_repos", autoRotate).
		Int("opted_out_repos", optedOut).
		Msg("rotation lane availability (HasLiveRotationSeams)")
}

// logProtectedRootProbeResults emits the operator-facing ERR line for every
// root the startup probe found Blocked or Denied. OK and Absent are not
// permission failures and are skipped — not everyone has a ~/Desktop.
//
// The message body is tccprobe.Remedy verbatim, which is what keeps the
// startup log and the RepairDoctor check from drifting apart (BOS-725). Remedy
// already leads with the root, the executable and "withholding" (the anchor
// operators grep for), so it is the whole message: prefixing "bossd cannot
// read <root>" would repeat both and contradict Remedy's own "degraded, not
// the daemon" framing.
func logProtectedRootProbeResults(logger zerolog.Logger, results []tccprobe.Result, executablePath string) {
	for _, result := range results {
		if result.Status != tccprobe.StatusBlocked && result.Status != tccprobe.StatusDenied {
			continue
		}
		logger.Error().Err(result.Err).
			Str("executable", executablePath).
			Str("root", result.Path).
			Msg(tccprobe.Remedy(result.Path, executablePath, result.Status))
	}
}

const protectedRootResolutionTimeout = 3 * time.Second

var errSymlinkResolutionTimedOut = errors.New("symlink resolution timed out")

type symlinkResolver func(string) (string, error)

// symlinkResolutionWorkerTracker retains resolver lifecycle handles so callers
// can include workers that outlive the shared resolution deadline in daemon
// shutdown coordination.
type symlinkResolutionWorkerTracker func(<-chan struct{})

type symlinkResolutionResult struct {
	index int
	path  string
	err   error
}

// protectedRootsForResolved keeps lexical matches, then resolves every
// candidate under one shared deadline. EvalSymlinks can itself wait on a macOS
// TCC prompt, so at most three goroutines may be left blocked after timeout.
func protectedRootsForResolved(home string, paths []string, timeout time.Duration, resolve symlinkResolver) ([]string, []tccprobe.Result) {
	return protectedRootsForResolvedWithTracker(home, paths, timeout, resolve, nil)
}

func protectedRootsForResolvedWithTracker(home string, paths []string, timeout time.Duration, resolve symlinkResolver, track symlinkResolutionWorkerTracker) ([]string, []tccprobe.Result) {
	if timeout <= 0 {
		timeout = protectedRootResolutionTimeout
	}
	return protectedRootsForResolvedAtWithTracker(home, paths, timeout, time.Now().Add(timeout), time.Now, resolve, track)
}

// protectedRootsForResolvedAt makes the deadline boundary testable. A result
// already buffered when timeout handling starts is retained; every other
// candidate is reported blocked without launching queued work.
func protectedRootsForResolvedAt(home string, paths []string, timeout time.Duration, deadline time.Time, now func() time.Time, resolve symlinkResolver) ([]string, []tccprobe.Result) {
	return protectedRootsForResolvedAtWithTracker(home, paths, timeout, deadline, now, resolve, nil)
}

func protectedRootsForResolvedAtWithTracker(home string, paths []string, timeout time.Duration, deadline time.Time, now func() time.Time, resolve symlinkResolver, track symlinkResolutionWorkerTracker) ([]string, []tccprobe.Result) {
	roots := tccprobe.ProtectedRootsFor(home, paths)
	workerLimit := min(3, len(paths))
	if workerLimit == 0 {
		return roots, nil
	}

	completed := make(chan symlinkResolutionResult, workerLimit)
	results := make([]symlinkResolutionResult, len(paths))
	finished := make([]bool, len(paths))

	next, active := 0, 0
	start := func(index int) {
		active++
		done := safego.Go(zerolog.Nop(), func() {
			path, err := resolveSymlinkCandidate(paths[index], resolve)
			completed <- symlinkResolutionResult{index: index, path: path, err: err}
		})
		if track != nil {
			track(done)
		}
	}
	timedOut := false
	for next < len(paths) && active < workerLimit {
		if !now().Before(deadline) {
			timedOut = true
			break
		}
		start(next)
		next++
	}

	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()

resolutionLoop:
	for !timedOut && active > 0 {
		select {
		case result := <-completed:
			active--
			results[result.index] = result
			finished[result.index] = true
			if next >= len(paths) {
				continue
			}
			if !now().Before(deadline) {
				timedOut = true
				break resolutionLoop
			}
			start(next)
			next++
		case <-timer.C:
			timedOut = true
			break resolutionLoop
		}
	}

	if timedOut {
		drainCompletedResolutions(completed, results, finished)
	}
	roots = mergeResolvedRoots(home, roots, results, finished)

	var diagnostics []tccprobe.Result
	for index, result := range results {
		if !finished[index] || result.err == nil {
			continue
		}
		status := tccprobe.StatusError
		if errors.Is(result.err, fs.ErrPermission) {
			status = tccprobe.StatusDenied
		}
		diagnostics = append(diagnostics, tccprobe.Result{
			Path:   paths[index],
			Status: status,
			Err:    result.err,
		})
	}
	if timedOut {
		for index, candidate := range paths {
			if finished[index] {
				continue
			}
			diagnostics = append(diagnostics, tccprobe.Result{
				Path:   candidate,
				Status: tccprobe.StatusBlocked,
				Err:    fmt.Errorf("%w after %s for %s", errSymlinkResolutionTimedOut, timeout, candidate),
			})
		}
	}
	return roots, diagnostics
}

// resolveSymlinkCandidate resolves a path that may end in a not-yet-created
// worktree leaf. EvalSymlinks requires every component to exist, so resolve
// the longest existing ancestor and retain the unresolved suffix instead.
func resolveSymlinkCandidate(candidate string, resolve symlinkResolver) (string, error) {
	resolved, err := resolve(candidate)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return resolved, err
	}

	ancestor, suffix, err := longestExistingAncestor(candidate)
	if err != nil {
		return "", err
	}
	resolvedAncestor, err := resolve(ancestor)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{resolvedAncestor}, suffix...)...), nil
}

func longestExistingAncestor(candidate string) (string, []string, error) {
	ancestor := filepath.Clean(candidate)
	var suffix []string
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			return ancestor, suffix, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, err
		}

		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", nil, err
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
	}
}

func drainCompletedResolutions(completed <-chan symlinkResolutionResult, results []symlinkResolutionResult, finished []bool) {
	for {
		select {
		case result := <-completed:
			results[result.index] = result
			finished[result.index] = true
		default:
			return
		}
	}
}

func mergeResolvedRoots(home string, roots []string, results []symlinkResolutionResult, finished []bool) []string {
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		seen[root] = struct{}{}
	}
	for index, result := range results {
		if !finished[index] || result.err != nil {
			continue
		}
		for _, root := range tccprobe.ProtectedRootsFor(home, []string{result.path}) {
			if _, exists := seen[root]; exists {
				continue
			}
			seen[root] = struct{}{}
			roots = append(roots, root)
		}
	}
	return roots
}

type daemonStateWriter func(string, daemonstate.Metadata) error

// persistTCCProbeResults rewrites the daemon identity record with the actual
// bounded probes performed by bossd. Persistence is observational: a state
// write failure is logged but never allowed to abort startup.
func persistTCCProbeResults(
	logger zerolog.Logger,
	appDataDir string,
	metadata daemonstate.Metadata,
	results []tccprobe.Result,
	write daemonStateWriter,
) daemonstate.Metadata {
	metadata.TCCProbeCompleted = true
	metadata.TCCProbeResults = make([]daemonstate.TCCProbeResult, 0, len(results))
	for _, result := range results {
		persisted := daemonstate.TCCProbeResult{
			Path:   result.Path,
			Status: persistedTCCProbeStatus(result.Status),
		}
		if result.Err != nil {
			persisted.Diagnostic = result.Err.Error()
		}
		metadata.TCCProbeResults = append(metadata.TCCProbeResults, persisted)
	}
	if err := write(appDataDir, metadata); err != nil {
		logger.Warn().Err(err).Msg("startup diagnostics could not persist protected-folder probe results")
	}
	return metadata
}

func persistedTCCProbeStatus(status tccprobe.Status) daemonstate.TCCProbeStatus {
	switch status {
	case tccprobe.StatusOK:
		return daemonstate.TCCProbeStatusOK
	case tccprobe.StatusDenied:
		return daemonstate.TCCProbeStatusDenied
	case tccprobe.StatusBlocked:
		return daemonstate.TCCProbeStatusBlocked
	case tccprobe.StatusAbsent:
		return daemonstate.TCCProbeStatusAbsent
	case tccprobe.StatusError:
		return daemonstate.TCCProbeStatusError
	default:
		// Fail closed if the probe package gains a status before persistence is
		// updated; doctor must not present an unknown observation as healthy.
		return daemonstate.TCCProbeStatusBlocked
	}
}

func stagedBinaryStale(runningPath, stagedPath, sourcePath string) (bool, error) {
	runningResolved, err := filepath.EvalSymlinks(runningPath)
	if err != nil {
		return false, fmt.Errorf("resolve running executable: %w", err)
	}
	stagedResolved, err := filepath.EvalSymlinks(stagedPath)
	if err != nil {
		if os.IsNotExist(err) && filepath.Clean(runningPath) != filepath.Clean(stagedPath) {
			return false, nil
		}
		return false, fmt.Errorf("resolve staged executable: %w", err)
	}
	if runningResolved != stagedResolved {
		return false, nil
	}

	sourceResolved, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return false, fmt.Errorf("resolve source executable: %w", err)
	}
	if sourceResolved == runningResolved {
		return false, nil
	}

	runningDigest, err := executableDigest(runningResolved)
	if err != nil {
		return false, fmt.Errorf("hash running executable: %w", err)
	}
	sourceDigest, err := executableDigest(sourceResolved)
	if err != nil {
		return false, fmt.Errorf("hash source executable: %w", err)
	}
	return runningDigest != sourceDigest, nil
}

func executableDigest(path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	// #nosec G304 -- paths are the running daemon and the bossd executable found on PATH.
	// owner=@recurser review-by=2027-02-04 issue=BOS-696
	file, err := os.Open(path)
	if err != nil {
		return digest, err
	}
	defer func() { _ = file.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return digest, err
	}
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

// mapRotationSwitchError translates a session.SwitchAgentAccount error into the sentinels the
// chat rotator classifies on. The rotation package cannot import session (the rotator is
// a seam the session layer is wired INTO), so this adapter is the single translation
// point.
//
// Order matters: ErrChatMidTurn is checked first because a mid-turn refusal is also a
// pre-pane-touch refusal, but the rotator's abort branch is deliberately different from
// the not-attempted branch (abort keeps the charge and re-probes; not-attempted refunds
// and parks). Anything else that returned before the pane was touched maps to
// ErrSwitchNotAttempted, additionally carrying ErrSwitchAgentAccountIneligible when the cause
// was the target account being disabled / health-failed / auth-invalid / cooling — on the
// respawn-in-place path the "target" IS the bound account, and that is the case the
// rotator resolves by rotating to an eligible account instead. (BOS-981)
func mapRotationSwitchError(err error) error {
	if err == nil {
		return nil
	}
	// Map the mid-turn refusal onto the rotator's fail-safe sentinel so the
	// respawn-in-place path leaves the chat as-is (no FAILED audit) and re-probes
	// later, rather than treating a deliberately-skipped live turn as a failure.
	if errors.Is(err, session.ErrChatMidTurn) {
		return rotation.ErrSwitchAborted
	}
	if !errors.Is(err, session.ErrSwitchNotAttempted) {
		return err
	}
	if errors.Is(err, session.ErrAgentAccountDisabled) ||
		errors.Is(err, session.ErrAgentAccountFailed) ||
		errors.Is(err, session.ErrAgentAccountAuthInvalid) ||
		errors.Is(err, session.ErrAgentAccountCooling) {
		return fmt.Errorf("%w: %w: %w", rotation.ErrSwitchNotAttempted, rotation.ErrSwitchAgentAccountIneligible, err)
	}
	return fmt.Errorf("%w: %w", rotation.ErrSwitchNotAttempted, err)
}
