package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing/fstest"
	"time"

	libskillinstall "github.com/recurser/bossalib/skillinstall"
	"github.com/recurser/bossalib/vcs"
)

// checkoutGitTimeout bounds every checkout git query.
//
// These run on the per-invocation path: trustedSkillSourceRoot is the
// revision-drift probe's TrustCheckout, so an unbounded query here is an
// unbounded subprocess before any `boss` command starts. A context-free
// exec.Command has no deadline at all, which is not "5 seconds worst case" but
// "until git returns".
const checkoutGitTimeout = 5 * time.Second

// checkoutGitWaitDelay bounds the output-pipe wait after the deadline has
// already killed git, for the reason revisiondrift.ExecGit sets it: .Output()
// copies through a pipe and blocks until every writer closes it, so a git that
// spawns a pager or a credential helper outlives the bound that killed git.
const checkoutGitWaitDelay = 2 * time.Second

func checkoutGitOutput(repoRoot string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), checkoutGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	cmd.WaitDelay = checkoutGitWaitDelay
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// trustedSkillSourceRoot rejects look-alike directory layouts in arbitrary
// repositories. The worktree and its origin must both identify the canonical
// project before its files become an automatic, agent-global install payload.
func trustedSkillSourceRoot(srcRoot string) (bool, error) {
	repoRoot := repoRootFromSourceRoot(srcRoot)
	worktree, err := checkoutGitOutput(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, fmt.Errorf("read checkout worktree: %w", err)
	}
	if !sameFilesystemPath(worktree, repoRoot) {
		return false, nil
	}
	origin, err := checkoutGitOutput(repoRoot, "remote", "get-url", "origin")
	if err != nil {
		return false, fmt.Errorf("read checkout origin: %w", err)
	}
	return strings.EqualFold(vcs.NormalizeRepoURL(origin), canonicalBossanovaURL), nil
}

func sameFilesystemPath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

const skillSnapshotAttempts = 3

// skillSourceRewriteLock is held by repository-owned writers which update the
// canonical skill payload outside Git's index lock (for example,
// scripts/vendor-toolbox.mjs). It prevents an install from accepting the
// stable-looking intermediate state between sequential file writes.
const skillSourceRewriteLock = "boss-skill-snapshot.lock"

// skillSourceRewriteGeneration records the most recent repository-owned skill
// rewrite. Read-only snapshots compare it across their captures because they
// cannot hold the rewrite lock themselves.
const skillSourceRewriteGeneration = "boss-skill-snapshot.generation"

// skillSourceRewriteGenerationDirtyPrefix marks a source rewrite interrupted
// before completion. A snapshot must never install that potentially partial
// payload; the next completed rewrite atomically replaces this marker.
const skillSourceRewriteGenerationDirtyPrefix = "dirty:"

// skillSourceRewriteLockMaxAge bounds how long a hard-stopped writer can leave
// snapshots blocked. Writers are short-lived; an older sentinel is abandoned.
const skillSourceRewriteLockMaxAge = 5 * time.Minute

const skillSourceRewriteLockContentionWait = 5 * time.Second

const skillSourceRewriteLockContentionRetry = 50 * time.Millisecond

const skillSourceRewriteLockReclaimSuffix = ".reclaim"

const skillSourceRewriteLockReclaimOwnerFile = "owner.json"

const skillSourceRewriteLockReclaimRecoveryFile = ".recovering"

// snapshotSkillFS freezes arbitrary skill bytes before callers compute a
// manifest or mutate an installed tree. Two matching captures protect against
// sources that change while a capture is in progress.
func snapshotSkillFS(fsys fs.FS) (fs.FS, error) {
	return snapshotSkillFSWithGuard(fsys, nil)
}

// snapshotCheckoutSkillFS additionally synchronizes the capture with Git's
// worktree update lock and repository-owned skill rewrites. Holding the rewrite
// lock for the entire capture prevents a writer from appearing and disappearing
// between the pre- and post-capture stability checks.
func snapshotCheckoutSkillFS(srcRoot string) (fs.FS, error) {
	repoRoot := repoRootFromSourceRoot(srcRoot)
	lock, err := waitForCheckoutSkillSourceRewriteLock(repoRoot)
	if err != nil {
		return nil, err
	}

	frozen, snapshotErr := snapshotSkillFSWithGuardAndGeneration(
		os.DirFS(srcRoot),
		func() (bool, error) { return waitForCheckoutGitUpdateStable(repoRoot) },
		func() (string, error) { return checkoutSkillSourceRewriteGeneration(repoRoot) },
	)
	releaseErr := lock.release()
	if snapshotErr != nil {
		return nil, snapshotErr
	}
	if releaseErr != nil {
		return nil, releaseErr
	}
	return frozen, nil
}

// snapshotCheckoutSkillFSReadOnly observes writer coordination without taking
// the source-side rewrite lock. It is for `boss skills check`, whose read-only
// contract must hold even when the checkout's Git metadata is not writable.
func snapshotCheckoutSkillFSReadOnly(srcRoot string) (fs.FS, error) {
	repoRoot := repoRootFromSourceRoot(srcRoot)
	return snapshotSkillFSWithGuardAndGeneration(
		os.DirFS(srcRoot),
		func() (bool, error) { return waitForCheckoutSkillSourceStable(repoRoot) },
		func() (string, error) { return checkoutSkillSourceRewriteGeneration(repoRoot) },
	)
}

func waitForCheckoutSkillSourceRewriteLock(repoRoot string) (checkoutSkillSourceRewriteLock, error) {
	deadline := time.Now().Add(skillSourceRewriteLockContentionWait)
	for {
		lock, acquired, err := acquireCheckoutSkillSourceRewriteLock(repoRoot)
		if err != nil {
			return checkoutSkillSourceRewriteLock{}, err
		}
		if acquired {
			return lock, nil
		}
		if !time.Now().Before(deadline) {
			return checkoutSkillSourceRewriteLock{}, fmt.Errorf("checkout skill rewrite lock remained held for %s", skillSourceRewriteLockContentionWait)
		}
		time.Sleep(skillSourceRewriteLockContentionRetry)
	}
}

func waitForCheckoutGitUpdateStable(repoRoot string) (bool, error) {
	deadline := time.Now().Add(skillSourceRewriteLockContentionWait)
	for {
		stable, err := checkoutGitUpdateStable(repoRoot)
		if err != nil || stable {
			return stable, err
		}
		if !time.Now().Before(deadline) {
			return false, fmt.Errorf("checkout Git update lock remained held for %s", skillSourceRewriteLockContentionWait)
		}
		time.Sleep(skillSourceRewriteLockContentionRetry)
	}
}

func waitForCheckoutSkillSourceStable(repoRoot string) (bool, error) {
	deadline := time.Now().Add(skillSourceRewriteLockContentionWait)
	for {
		stable, err := checkoutSkillSourceStable(repoRoot)
		if err != nil || stable {
			return stable, err
		}
		if !time.Now().Before(deadline) {
			return false, fmt.Errorf("checkout skill source remained busy for %s", skillSourceRewriteLockContentionWait)
		}
		time.Sleep(skillSourceRewriteLockContentionRetry)
	}
}

// snapshotSkillFSWithGuard accepts a snapshot only if guard reports the source
// stable both before and after the two captures. The nil guard keeps the
// generic fs.FS snapshot helper usable for embedded and in-memory sources.
func snapshotSkillFSWithGuard(fsys fs.FS, guard func() (bool, error)) (fs.FS, error) {
	return snapshotSkillFSWithGuardAndGeneration(fsys, guard, nil)
}

// snapshotSkillFSWithGuardAndGeneration accepts a snapshot only if guard
// reports the source stable and generation remains unchanged across capture.
func snapshotSkillFSWithGuardAndGeneration(fsys fs.FS, guard func() (bool, error), generation func() (string, error)) (fs.FS, error) {
	for attempt := 0; attempt < skillSnapshotAttempts; attempt++ {
		if guard != nil {
			stable, err := guard()
			if err != nil {
				return nil, fmt.Errorf("check checkout skill source before capture: %w", err)
			}
			if !stable {
				continue
			}
		}
		beforeGeneration := ""
		if generation != nil {
			var err error
			beforeGeneration, err = generation()
			if err != nil {
				return nil, fmt.Errorf("read checkout skill rewrite generation before capture: %w", err)
			}
		}
		frozen, err := captureSkillFS(fsys)
		if err != nil {
			return nil, fmt.Errorf("snapshot checkout skill payload: %w", err)
		}
		confirmed, err := captureSkillFS(fsys)
		if err != nil {
			return nil, fmt.Errorf("confirm checkout skill payload: %w", err)
		}
		frozenManifest, err := libskillinstall.Manifest(frozen)
		if err != nil {
			return nil, fmt.Errorf("manifest checkout skill snapshot: %w", err)
		}
		confirmedManifest, err := libskillinstall.Manifest(confirmed)
		if err != nil {
			return nil, fmt.Errorf("manifest confirmed checkout skill snapshot: %w", err)
		}
		stable := true
		if guard != nil {
			stable, err = guard()
			if err != nil {
				return nil, fmt.Errorf("check checkout skill source after capture: %w", err)
			}
		}
		afterGeneration := beforeGeneration
		if generation != nil {
			var err error
			afterGeneration, err = generation()
			if err != nil {
				return nil, fmt.Errorf("read checkout skill rewrite generation after capture: %w", err)
			}
		}
		if stable && beforeGeneration == afterGeneration && frozenManifest == confirmedManifest {
			if err := validateSnapshotSkillPayload(frozen); err != nil {
				return nil, fmt.Errorf("validate checkout skill snapshot: %w", err)
			}
			return frozen, nil
		}
	}
	return nil, fmt.Errorf("checkout skill sources changed during capture after %d attempts", skillSnapshotAttempts)
}

func checkoutGitUpdateStable(repoRoot string) (bool, error) {
	lockPath, err := checkoutLockPath(repoRoot, "index.lock")
	if err != nil {
		return false, err
	}
	_, err = os.Stat(lockPath)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat checkout update lock %q: %w", "index.lock", err)
	}
	return true, nil
}

func checkoutSkillSourceStable(repoRoot string) (bool, error) {
	stable, err := checkoutGitUpdateStable(repoRoot)
	if err != nil || !stable {
		return stable, err
	}
	lockPath, err := checkoutLockPath(repoRoot, skillSourceRewriteLock)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(lockPath)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat checkout update lock %q: %w", skillSourceRewriteLock, err)
	}
	return true, nil
}

func checkoutSkillSourceRewriteGeneration(repoRoot string) (string, error) {
	generationPath, err := checkoutLockPath(repoRoot, skillSourceRewriteGeneration)
	if err != nil {
		return "", err
	}
	generation, err := os.ReadFile(generationPath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read checkout skill rewrite generation: %w", err)
	}
	generation = []byte(strings.TrimSpace(string(generation)))
	if strings.HasPrefix(string(generation), skillSourceRewriteGenerationDirtyPrefix) {
		return "", errors.New("checkout skill rewrite is incomplete after abandoned lock recovery")
	}
	return string(generation), nil
}

func markCheckoutSkillSourceRewriteGenerationDirty(repoRoot string) error {
	generationPath, err := checkoutLockPath(repoRoot, skillSourceRewriteGeneration)
	if err != nil {
		return err
	}
	pending, err := os.CreateTemp(filepath.Dir(generationPath), filepath.Base(generationPath)+".dirty-*")
	if err != nil {
		return fmt.Errorf("create incomplete checkout skill rewrite generation: %w", err)
	}
	pendingPath := pending.Name()
	defer func() { _ = os.Remove(pendingPath) }()
	if err := pending.Chmod(0o600); err != nil {
		_ = pending.Close()
		return fmt.Errorf("set incomplete checkout skill rewrite generation permissions: %w", err)
	}
	if _, err := pending.WriteString(skillSourceRewriteGenerationDirtyPrefix + strconv.FormatInt(time.Now().UnixMilli(), 10) + "\n"); err != nil {
		_ = pending.Close()
		return fmt.Errorf("write incomplete checkout skill rewrite generation: %w", err)
	}
	if err := pending.Close(); err != nil {
		return fmt.Errorf("close incomplete checkout skill rewrite generation: %w", err)
	}
	if err := os.Rename(pendingPath, generationPath); err != nil {
		return fmt.Errorf("publish incomplete checkout skill rewrite generation: %w", err)
	}
	return nil
}

type checkoutSkillSourceRewriteLock struct {
	path string
	info fs.FileInfo
}

type checkoutSkillSourceRewriteLockMetadata struct {
	CreatedAt      *int64  `json:"createdAt"`
	PID            int     `json:"pid"`
	OwnerStartTime *string `json:"ownerStartTime,omitempty"`
	Token          string  `json:"token,omitempty"`
}

type checkoutSkillSourceRewriteClaimIdentity struct {
	info          fs.FileInfo
	ownerContents string
	ownerPresent  bool
}

func checkoutLockPath(repoRoot, lockName string) (string, error) {
	lockPath, err := checkoutGitOutput(repoRoot, "rev-parse", "--git-path", lockName)
	if err != nil {
		return "", fmt.Errorf("find checkout update lock %q: %w", lockName, err)
	}
	if !filepath.IsAbs(lockPath) {
		lockPath = filepath.Join(repoRoot, lockPath)
	}
	return lockPath, nil
}

func acquireCheckoutSkillSourceRewriteLock(repoRoot string) (checkoutSkillSourceRewriteLock, bool, error) {
	lockPath, err := checkoutLockPath(repoRoot, skillSourceRewriteLock)
	if err != nil {
		return checkoutSkillSourceRewriteLock{}, false, err
	}
	if info, err := os.Stat(lockPath); err == nil {
		if !checkoutSkillSourceRewriteLockAbandoned(lockPath, info) {
			return checkoutSkillSourceRewriteLock{}, false, nil
		}
		if _, reclaimErr := reclaimAbandonedCheckoutSkillSourceRewriteLock(repoRoot, lockPath, info); reclaimErr != nil {
			return checkoutSkillSourceRewriteLock{}, false, reclaimErr
		}
		return checkoutSkillSourceRewriteLock{}, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return checkoutSkillSourceRewriteLock{}, false, fmt.Errorf("stat checkout update lock %q: %w", skillSourceRewriteLock, err)
	}

	pending, err := os.CreateTemp(filepath.Dir(lockPath), filepath.Base(lockPath)+".snapshot-*")
	if err != nil {
		return checkoutSkillSourceRewriteLock{}, false, fmt.Errorf("create checkout skill snapshot lock: %w", err)
	}
	pendingPath := pending.Name()
	defer func() { _ = os.Remove(pendingPath) }()
	createdAt := time.Now().UnixMilli()
	metadata := checkoutSkillSourceRewriteLockOwnerMetadata(createdAt)
	metadata.Token = "snapshot"
	contents, err := json.Marshal(metadata)
	if err != nil {
		_ = pending.Close()
		return checkoutSkillSourceRewriteLock{}, false, fmt.Errorf("encode checkout skill snapshot lock: %w", err)
	}
	if _, err := pending.Write(contents); err != nil {
		_ = pending.Close()
		return checkoutSkillSourceRewriteLock{}, false, fmt.Errorf("write checkout skill snapshot lock: %w", err)
	}
	if err := pending.Close(); err != nil {
		return checkoutSkillSourceRewriteLock{}, false, fmt.Errorf("close checkout skill snapshot lock: %w", err)
	}
	if err := os.Link(pendingPath, lockPath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return checkoutSkillSourceRewriteLock{}, false, nil
		}
		return checkoutSkillSourceRewriteLock{}, false, fmt.Errorf("publish checkout skill snapshot lock: %w", err)
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		return checkoutSkillSourceRewriteLock{}, false, fmt.Errorf("stat published checkout skill snapshot lock: %w", err)
	}
	return checkoutSkillSourceRewriteLock{path: lockPath, info: info}, true, nil
}

func checkoutSkillSourceRewriteLockAbandoned(lockPath string, info fs.FileInfo) bool {
	if time.Since(info.ModTime()) <= skillSourceRewriteLockMaxAge {
		return false
	}
	contents, err := os.ReadFile(lockPath)
	if err != nil {
		return false
	}
	var metadata checkoutSkillSourceRewriteLockMetadata
	if err := json.Unmarshal(contents, &metadata); err != nil || metadata.PID <= 0 {
		// A pre-metadata sentinel cannot identify a live owner and remains
		// reclaimable for compatibility with older writers.
		return true
	}
	return !checkoutSkillSourceRewriteLockOwnerAlive(metadata)
}

func checkoutSkillSourceRewriteLockOwnerMetadata(createdAt int64) checkoutSkillSourceRewriteLockMetadata {
	metadata := checkoutSkillSourceRewriteLockMetadata{CreatedAt: &createdAt, PID: os.Getpid()}
	if startTime, ok := checkoutSkillSourceRewriteLockOwnerStartTime(metadata.PID); ok {
		metadata.OwnerStartTime = &startTime
	}
	return metadata
}

func checkoutSkillSourceRewriteLockOwnerStartTime(pid int) (string, bool) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	startTime := strings.TrimSpace(string(out))
	return startTime, err == nil && startTime != ""
}

func checkoutSkillSourceRewriteLockOwnerAlive(metadata checkoutSkillSourceRewriteLockMetadata) bool {
	if metadata.PID <= 0 {
		return false
	}
	if startTime, ok := checkoutSkillSourceRewriteLockOwnerStartTime(metadata.PID); ok {
		return metadata.OwnerStartTime == nil || *metadata.OwnerStartTime == startTime
	}
	process, err := os.FindProcess(metadata.PID)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// Treat an uncertain probe as live: reclaiming a live writer is worse than
	// retrying a snapshot around a stale sentinel.
	return !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrProcessDone)
}

func reclaimAbandonedCheckoutSkillSourceRewriteLock(repoRoot, lockPath string, observed fs.FileInfo) (bool, error) {
	claimPath := lockPath + skillSourceRewriteLockReclaimSuffix
	pendingPath, err := os.MkdirTemp(filepath.Dir(claimPath), filepath.Base(claimPath)+".pending-*")
	if err != nil {
		return false, fmt.Errorf("prepare abandoned checkout update lock claim %q: %w", skillSourceRewriteLock, err)
	}
	defer func() { _ = os.RemoveAll(pendingPath) }()
	createdAt := time.Now().UnixMilli()
	owner := checkoutSkillSourceRewriteLockOwnerMetadata(createdAt)
	metadata, err := json.Marshal(owner)
	if err != nil {
		return false, fmt.Errorf("encode abandoned checkout update lock claim owner: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pendingPath, skillSourceRewriteLockReclaimOwnerFile), metadata, 0o600); err != nil {
		return false, fmt.Errorf("write abandoned checkout update lock claim owner: %w", err)
	}
	if err := os.Rename(pendingPath, claimPath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if _, reclaimErr := reclaimAbandonedCheckoutSkillSourceRewriteLockClaim(claimPath); reclaimErr != nil {
				return false, reclaimErr
			}
			return false, nil
		}
		return false, fmt.Errorf("publish abandoned checkout update lock claim %q: %w", skillSourceRewriteLock, err)
	}
	defer func() { _ = os.RemoveAll(claimPath) }()
	checkoutSkillSourceRewriteLockClaimPublished(claimPath)

	current, err := os.Stat(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat abandoned checkout update lock %q: %w", skillSourceRewriteLock, err)
	}
	if !os.SameFile(observed, current) || !checkoutSkillSourceRewriteLockAbandoned(lockPath, current) {
		return false, nil
	}
	checkoutSkillSourceRewriteLockBeforeReclaim()
	// Record the interrupted generation before removing its sentinel. A new
	// writer cannot begin until the sentinel is gone, so it cannot race this
	// dirty marker; its successful next generation atomically replaces it.
	if err := markCheckoutSkillSourceRewriteGenerationDirty(repoRoot); err != nil {
		return false, err
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("remove abandoned checkout update lock %q: %w", skillSourceRewriteLock, err)
	}
	return true, nil
}

func reclaimAbandonedCheckoutSkillSourceRewriteLockClaim(claimPath string) (bool, error) {
	// An interrupted reclaimer can leave a stale recovery marker inside an
	// abandoned claim. That marker fences every contender from publishing a
	// successor while this pass removes the observed claim.
	observed, abandoned, err := checkoutSkillSourceRewriteLockClaimAbandoned(claimPath)
	if err != nil || !abandoned {
		return false, err
	}

	recoveryPath := filepath.Join(claimPath, skillSourceRewriteLockReclaimRecoveryFile)
	recovery, err := os.OpenFile(recoveryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			recovered, reclaimErr := reclaimAbandonedCheckoutSkillSourceRewriteLockRecovery(recoveryPath)
			if reclaimErr != nil {
				return false, reclaimErr
			}
			if !recovered {
				return false, nil
			}
			// The stale recovery marker is itself the fencing sidecar: leave it
			// in place and remove only the observed abandoned claim below. A
			// concurrent reclaimer therefore cannot replace that marker and have
			// another process delete its live successor by pathname.
		} else if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		} else {
			return false, fmt.Errorf("claim abandoned checkout update lock recovery: %w", err)
		}
	} else {
		createdAt := time.Now().UnixMilli()
		owner := checkoutSkillSourceRewriteLockOwnerMetadata(createdAt)
		metadata, err := json.Marshal(owner)
		if err != nil {
			_ = recovery.Close()
			return false, fmt.Errorf("encode abandoned checkout update lock recovery owner: %w", err)
		}
		if _, err := recovery.Write(metadata); err != nil {
			_ = recovery.Close()
			return false, fmt.Errorf("write abandoned checkout update lock recovery owner: %w", err)
		}
		if err := recovery.Close(); err != nil {
			return false, fmt.Errorf("close abandoned checkout update lock recovery claim: %w", err)
		}
	}

	checkoutSkillSourceRewriteLockClaimRecoveryAcquired()
	current, err := checkoutSkillSourceRewriteClaimIdentityAt(claimPath)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("revalidate abandoned checkout update lock claim: %w", err)
	}
	if !sameCheckoutSkillSourceRewriteClaim(observed, current) {
		return false, nil
	}
	if err := os.RemoveAll(claimPath); err != nil {
		return false, fmt.Errorf("remove abandoned checkout update lock claim: %w", err)
	}
	return true, nil
}

func reclaimAbandonedCheckoutSkillSourceRewriteLockRecovery(recoveryPath string) (bool, error) {
	observed, err := os.Stat(recoveryPath)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat checkout update lock recovery: %w", err)
	}
	contents, err := os.ReadFile(recoveryPath)
	abandoned := false
	if err != nil {
		abandoned = time.Since(observed.ModTime()) > skillSourceRewriteLockMaxAge
	} else {
		var metadata checkoutSkillSourceRewriteLockMetadata
		if json.Unmarshal(contents, &metadata) != nil {
			abandoned = time.Since(observed.ModTime()) > skillSourceRewriteLockMaxAge
		} else if metadata.CreatedAt == nil || time.Since(time.UnixMilli(*metadata.CreatedAt)) > skillSourceRewriteLockMaxAge {
			abandoned = !checkoutSkillSourceRewriteLockOwnerAlive(metadata)
		}
	}
	if !abandoned {
		return false, nil
	}
	current, err := os.Stat(recoveryPath)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("revalidate checkout update lock recovery: %w", err)
	}
	if !os.SameFile(observed, current) {
		return true, nil
	}
	checkoutSkillSourceRewriteLockBeforeRecoveryReclaim()
	current, err = os.Stat(recoveryPath)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("revalidate checkout update lock recovery after ownership check: %w", err)
	}
	currentContents, err := os.ReadFile(recoveryPath)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read checkout update lock recovery after ownership check: %w", err)
	}
	if !sameCheckoutSkillSourceRewriteLockRecovery(observed, string(contents), current, string(currentContents)) {
		return false, nil
	}
	// Do not unlink a stale recovery marker. A pathname unlink can remove a
	// successor published after the identity check. The caller treats this exact
	// stale marker as a fence and removes the enclosing observed claim instead.
	return true, nil
}

// sameCheckoutSkillSourceRewriteLockRecovery verifies the recovery marker did
// not change while a stale reclaimer owned its observation. Filesystems may
// reuse a removed file's identity immediately, so file identity alone cannot
// distinguish a newly published successor marker.
func sameCheckoutSkillSourceRewriteLockRecovery(observed fs.FileInfo, observedContents string, current fs.FileInfo, currentContents string) bool {
	return os.SameFile(observed, current) && observedContents == currentContents
}

func checkoutSkillSourceRewriteClaimIdentityAt(claimPath string) (checkoutSkillSourceRewriteClaimIdentity, error) {
	info, err := os.Stat(claimPath)
	if err != nil {
		return checkoutSkillSourceRewriteClaimIdentity{}, err
	}
	contents, err := os.ReadFile(filepath.Join(claimPath, skillSourceRewriteLockReclaimOwnerFile))
	if err != nil {
		return checkoutSkillSourceRewriteClaimIdentity{info: info}, nil
	}
	return checkoutSkillSourceRewriteClaimIdentity{info: info, ownerContents: string(contents), ownerPresent: true}, nil
}

func sameCheckoutSkillSourceRewriteClaim(left, right checkoutSkillSourceRewriteClaimIdentity) bool {
	return os.SameFile(left.info, right.info) &&
		left.ownerPresent == right.ownerPresent &&
		left.ownerContents == right.ownerContents
}

func checkoutSkillSourceRewriteLockClaimAbandoned(claimPath string) (checkoutSkillSourceRewriteClaimIdentity, bool, error) {
	identity, err := checkoutSkillSourceRewriteClaimIdentityAt(claimPath)
	if errors.Is(err, fs.ErrNotExist) {
		return checkoutSkillSourceRewriteClaimIdentity{}, false, nil
	}
	if err != nil {
		return checkoutSkillSourceRewriteClaimIdentity{}, false, fmt.Errorf("stat checkout update lock reclaim claim: %w", err)
	}

	if !identity.ownerPresent {
		// A hard stop can leave the claim directory behind before its owner metadata
		// is written. Its age is the only safe liveness signal in that legacy gap.
		return identity, time.Since(identity.info.ModTime()) > skillSourceRewriteLockMaxAge, nil
	}
	var metadata checkoutSkillSourceRewriteLockMetadata
	if err := json.Unmarshal([]byte(identity.ownerContents), &metadata); err != nil {
		return identity, time.Since(identity.info.ModTime()) > skillSourceRewriteLockMaxAge, nil
	}
	if metadata.CreatedAt != nil && time.Since(time.UnixMilli(*metadata.CreatedAt)) <= skillSourceRewriteLockMaxAge {
		return identity, false, nil
	}
	return identity, !checkoutSkillSourceRewriteLockOwnerAlive(metadata), nil
}

func (lock checkoutSkillSourceRewriteLock) release() error {
	current, err := os.Stat(lock.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat checkout skill snapshot lock before release: %w", err)
	}
	if os.SameFile(lock.info, current) {
		if err := os.Remove(lock.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove checkout skill snapshot lock: %w", err)
		}
	}
	return nil
}

// validateSnapshotSkillPayload prevents an incomplete checkout update from
// removing the entire installed boss skill namespace. A valid payload always
// includes the base boss skill and every published boss-* skill directory.
func validateSnapshotSkillPayload(fsys fs.FS) error {
	if _, err := fs.ReadFile(fsys, "skills/boss/SKILL.md"); err != nil {
		return fmt.Errorf("checkout skill snapshot is missing base boss skill: %w", err)
	}
	entries, err := fs.ReadDir(fsys, "skills")
	if err != nil {
		return fmt.Errorf("read checkout skills directory: %w", err)
	}
	foundPublishedSkill := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "boss-") {
			continue
		}
		if !entry.IsDir() {
			return fmt.Errorf("checkout skill snapshot has non-directory published skill %q", entry.Name())
		}
		foundPublishedSkill = true
		if _, err := fs.ReadFile(fsys, path.Join("skills", entry.Name(), "SKILL.md")); err != nil {
			return fmt.Errorf("checkout skill snapshot is missing published skill %q: %w", entry.Name(), err)
		}
	}
	if !foundPublishedSkill {
		return errors.New("checkout skill snapshot is missing a boss-* skill")
	}
	return nil
}

func captureSkillFS(fsys fs.FS) (fs.FS, error) {
	frozen := fstest.MapFS{}
	if err := fs.WalkDir(fsys, "skills", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("checkout skill entry %q is not a regular file", path)
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		frozen[path] = &fstest.MapFile{Data: data}
		return nil
	}); err != nil {
		return nil, err
	}
	return frozen, nil
}
