package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/skillinstall"
	"github.com/recurser/bossalib/config"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
	"github.com/recurser/bossalib/vcs"
)

// skillSyncMode selects how runSkillSync treats each agent's skill dir.
type skillSyncMode int

const (
	// skillSyncUpdateOnly (`boss skills sync`): EnsureUpdated per agent — a silent
	// no-op on a current tree and NEVER a fresh install into an empty dir.
	skillSyncUpdateOnly skillSyncMode = iota
	// skillSyncInstall (`boss skills install`): refresh a stale tree AND fresh-install
	// an empty one, so it doubles as first-time setup; a no-op when already current.
	skillSyncInstall
	// skillSyncForce (`boss skills install --force`): Extract unconditionally
	// (repair/overwrite), even when current.
	skillSyncForce
)

// skillPayload selects the payload a skill install writes. A trusted Bossanova
// checkout may supply agent-global skill bytes: another repository can reproduce
// the source directory layout, but must never gain write access to ~/.claude/skills
// or ~/.codex/skills by doing so. Both startup and direct skill commands require
// explicit opt-in because an origin URL alone does not authenticate checked-out
// bytes.
type selectedSkillPayload struct {
	fsys       fs.FS
	srcRoot    string
	fromSource bool
}

// trustCheckoutSkillSourcesEnv requires a deliberate opt-in before checkout
// files can replace an agent's globally installed instructions. A canonical
// remote alone is insufficient: a clone can check out an untrusted fork or PR
// revision without changing its origin URL.
const trustCheckoutSkillSourcesEnv = "BOSS_TRUST_CHECKOUT_SKILLS"

func skillPayload() (selectedSkillPayload, error) {
	return skillPayloadWithCheckoutTrust(os.Getenv(trustCheckoutSkillSourcesEnv) == "1")
}

// startupSkillPayload selects a checkout only after the operator explicitly
// trusts its bytes. This prevents a canonical-looking origin from authorizing a
// local PR or dirty worktree to overwrite globally installed agent instructions.
func startupSkillPayload() (selectedSkillPayload, error) {
	return skillPayloadWithCheckoutTrust(os.Getenv(trustCheckoutSkillSourcesEnv) == "1")
}

func skillPayloadWithCheckoutTrust(trustCheckout bool) (selectedSkillPayload, error) {
	return skillPayloadWithCheckoutTrustMode(trustCheckout, false)
}

// skillCheckPayload selects a checkout payload without creating any source-side
// coordination files, so `boss skills check` remains safe on read-only checkouts.
func skillCheckPayload() (selectedSkillPayload, error) {
	return skillPayloadWithCheckoutTrustMode(true, true)
}

func skillPayloadWithCheckoutTrustMode(trustCheckout, readOnly bool) (selectedSkillPayload, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return selectedSkillPayload{}, fmt.Errorf("get working directory: %w", err)
	}
	if root, ok := libskillinstall.FindSourceRoot(cwd); ok && trustCheckout {
		trusted, err := trustedSkillSourceRoot(root)
		if err != nil {
			return selectedSkillPayload{}, fmt.Errorf("authenticate checkout skill source: %w", err)
		}
		if trusted {
			snapshot := snapshotCheckoutSkillFS
			if readOnly {
				snapshot = snapshotCheckoutSkillFSReadOnly
			}
			frozen, err := snapshot(root)
			if err != nil {
				return selectedSkillPayload{}, err
			}
			return selectedSkillPayload{fsys: frozen, srcRoot: root, fromSource: true}, nil
		}
	}
	return selectedSkillPayload{fsys: skillinstall.SkillsFS}, nil
}

const canonicalBossanovaRepo = "recurser/bossanova"

const canonicalBossanovaURL = "https://github.com/" + canonicalBossanovaRepo

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

// checkoutSkillSourceRewriteLockBeforeReclaim keeps the stale-reclaim race
// reproducible without exposing the coordination protocol to callers.
var checkoutSkillSourceRewriteLockBeforeReclaim = func() {}

// checkoutSkillSourceRewriteLockClaimPublished keeps claim publication
// atomicity reproducible without exposing the coordination protocol to callers.
var checkoutSkillSourceRewriteLockClaimPublished = func(string) {}

// checkoutSkillSourceRewriteLockClaimRecoveryAcquired keeps sidecar recovery
// ownership races reproducible without exposing the coordination protocol.
var checkoutSkillSourceRewriteLockClaimRecoveryAcquired = func() {}

// checkoutSkillSourceRewriteLockBeforeRecoveryReclaim makes recovery sidecar
// successor races reproducible without exposing the coordination protocol.
var checkoutSkillSourceRewriteLockBeforeRecoveryReclaim = func() {}

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

// skillsCmd exposes the non-interactive skill-refresh surface. Unlike the
// interactive startup prompt (maybeInstallSkills), these subcommands never check
// for a TTY and never emit a [Y/n] prompt, so they are safe to call from scripts,
// cron jobs, or a setup hook to close the dogfood loop after a skill edit + build.
func skillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Manage installed boss skills",
	}

	var syncAgent string
	sync := &cobra.Command{
		Use:   "sync",
		Short: "Refresh installed boss skills from the selected checkout or embedded payload (update-only, no prompt)",
		// Reject positional operands: the agent is selected via --agent, and a bare
		// `boss skills sync codex` must error rather than silently ignore "codex" and
		// mutate every agent's global skill dir on PATH.
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runSkillSync(c.OutOrStdout(), skillSyncUpdateOnly, syncAgent)
		},
	}
	sync.Flags().StringVar(&syncAgent, "agent", "", "Restrict to one agent: claude or codex (default: all on PATH)")

	var force bool
	var installAgent string
	install := &cobra.Command{
		Use:   "install",
		Short: "Install or refresh boss skills (fresh-installs missing trees); --force reinstalls even when current",
		// Reject positional operands: the agent is selected via --agent, and a bare
		// `boss skills install codex` must error rather than silently ignore "codex"
		// and mutate every agent's global skill dir on PATH.
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			mode := skillSyncInstall
			if force {
				mode = skillSyncForce
			}
			return runSkillSync(c.OutOrStdout(), mode, installAgent)
		},
	}
	install.Flags().BoolVar(&force, "force", false, "Reinstall (Extract) unconditionally, even when current")
	install.Flags().StringVar(&installAgent, "agent", "", "Restrict to one agent: claude or codex (default: all on PATH)")

	var checkAgent string
	var gate bool
	check := &cobra.Command{
		Use:   "check",
		Short: "Check installed boss skills against this binary and checkout sources",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if gate {
				return runSkillGate(c.OutOrStdout(), checkAgent)
			}
			return runSkillCheck(c.OutOrStdout(), checkAgent)
		},
	}
	check.Flags().BoolVar(&gate, "gate", false, "Fail only when installed skills drift from checkout source for paths not edited by this branch")
	check.Flags().StringVar(&checkAgent, "agent", "", "Restrict to one agent: claude or codex (default: all on PATH)")

	cmd.AddCommand(sync, install, check)
	return cmd
}

// binarySkillsDriftWarning names the COMPLETE remedy, and neither half is
// optional.
//
// `plugins`: bin/bossd-plugin-claude carries its own mirror of the skill payload
// and calls EnsureUpdated on the installed tree at daemon startup. Rebuilding and
// reinstalling only the CLI therefore looks fixed until the next daemon start,
// which restores the old skills from the stale plugin binary.
//
// `./bin/boss`: this warning fires whenever the RUNNING binary's payload is older
// than the source tree, and that binary is often a globally installed one (say
// /opt/homebrew/bin/boss) invoked from inside the checkout. `make build plugins`
// writes the rebuilt CLI to ./bin/boss and leaves the global copy untouched.
// Naming the rebuilt binary explicitly exercises the CLI that was just rebuilt,
// while the checkout source remains the selected install payload from any boss
// binary invoked inside the checkout.
//
// Both commands are anchored at the repository root rather than the cwd, because
// FindSourceRoot searches PARENT directories: the warning fires just as readily
// from services/boss/cmd as from the root, and there a bare `make build plugins`
// would run against the nested directory's Makefile (or none) while `./bin/boss`
// would name a binary that does not exist. Deriving the root from srcRoot makes
// the printed commands correct from anywhere in the checkout.
//
// Must stay a single line — it is printed verbatim to stderr and, indented, to
// `boss skills check`.
func binarySkillsDriftWarning(srcRoot string) string {
	return formatBinarySkillsDriftWarning(
		srcRoot,
		" installed skills were refreshed from source and are current;",
	)
}

// binarySkillsDriftCurrentWarning reports a verified current state without
// claiming that the read-only check command performed the refresh.
func binarySkillsDriftCurrentWarning(srcRoot string) string {
	return formatBinarySkillsDriftWarning(
		srcRoot,
		" installed skills match checkout source and are current;",
	)
}

// binarySkillsDriftCheckWarning keeps the rebuild remedy but makes no claim
// about installed state when `boss skills check` found a stale/absent payload or
// could not inspect one.
func binarySkillsDriftCheckWarning(srcRoot string) string {
	return formatBinarySkillsDriftWarning(srcRoot, "")
}

func formatBinarySkillsDriftWarning(srcRoot, installedStatus string) string {
	root := repoRootFromSourceRoot(srcRoot)
	return fmt.Sprintf(
		"⚠ drift — this running boss binary's embedded skills are behind checkout source %s/skills;"+
			"%s to make the binary shippable,"+
			" rebuild with `make -C %s build plugins`, then run `%s skills install`",
		srcRoot, installedStatus, shellQuote(root), shellQuote(filepath.Join(root, "bin", "boss")),
	)
}

// shellQuote makes an interpolated path safe to paste into a shell. The remedy
// above advertises commands built from a real checkout path, and a checkout under
// e.g. `/home/me/Boss Nova` would otherwise split into multiple arguments, so
// neither command would reach the intended directory or binary.
//
// Quoting is conditional so the overwhelmingly common ordinary path stays
// readable; the safe set is the conservative one used by POSIX shell-quoting
// helpers (shlex.quote), and anything outside it — whitespace, glob characters,
// `$`, backticks, quotes — takes the single-quoted form, where the only character
// needing escaping is a literal single quote.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	const safeExtra = "_@%+=:,./-"
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case strings.ContainsRune(safeExtra, r):
		default:
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// repoRootFromSourceRoot inverts FindSourceRoot's filepath.Join(root,
// SourceRelPath): it climbs one directory per SourceRelPath segment, so it stays
// correct if that constant ever gains or loses a segment, and it uses
// filepath.Dir rather than string trimming so the platform separator is handled.
func repoRootFromSourceRoot(srcRoot string) string {
	root := srcRoot
	for range strings.Split(libskillinstall.SourceRelPath, "/") {
		root = filepath.Dir(root)
	}
	return root
}

// skillSourceStatus compares the running binary's embedded skills with the
// selected checkout snapshot. Reusing payload.fsys keeps the source comparison
// and installed-tree comparison coherent when the checkout changes mid-command.
func skillSourceStatus(payload selectedSkillPayload) (srcRoot string, found, stale bool, err error) {
	if !payload.fromSource {
		return "", false, false, nil
	}
	embedManifest, err := libskillinstall.Manifest(skillinstall.SkillsFS)
	if err != nil {
		return "", false, false, err
	}
	sourceManifest, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		return "", false, false, err
	}
	return payload.srcRoot, true, embedManifest != sourceManifest, nil
}

// warnBinarySkillsDrift emits the non-fatal stale-binary warning. The stronger
// post-refresh wording is reserved for operations that established a current
// installed tree. A missing source tree is normal for consuming repositories.
func warnBinarySkillsDrift(payload selectedSkillPayload, postRefreshCurrent bool, installedManifest string) {
	srcRoot, found, stale, err := skillSourceStatus(payload)
	if err != nil || !found || !stale {
		return
	}
	if postRefreshCurrent {
		currentSourceManifest, manifestErr := libskillinstall.Manifest(payload.fsys)
		if manifestErr != nil || currentSourceManifest != installedManifest {
			postRefreshCurrent = false
		}
	}
	warning := binarySkillsDriftCheckWarning(srcRoot)
	if postRefreshCurrent {
		warning = binarySkillsDriftWarning(srcRoot)
	}
	_, _ = fmt.Fprintln(os.Stderr, warning)
}

// runSkillCheck reports the installed-vs-selected-payload and embed-vs-source state for
// every selected agent. It is read-only and returns an error only after every
// requested report has been written.
func runSkillCheck(out io.Writer, only string) error {
	if err := validateSkillAgentFilter(only); err != nil {
		return err
	}

	// Checks are read-only, so a canonical authenticated checkout is safe to use
	// without the write opt-in required by install and sync commands.
	payload, err := skillCheckPayload()
	if err != nil {
		return fmt.Errorf("select skill payload: %w", err)
	}
	srcRoot, found, binaryStale, err := skillSourceStatus(payload)
	if err != nil {
		return fmt.Errorf("check skill sources: %w", err)
	}
	payloadSource := "binary embed"
	if payload.fromSource {
		payloadSource = fmt.Sprintf("checkout (%s)", payload.srcRoot)
	}
	drift := binaryStale
	reported := false
	var checkErr error
	for _, target := range skillInstallAgents {
		if only != "" && target.command != only {
			continue
		}
		_, pathErr := skillInstallLookPath(target.command)
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			if pathErr != nil {
				continue
			}
			return fmt.Errorf("%s skills dir: %w", target.command, err)
		}
		installed, payloadStale, inspectErr := libskillinstall.InstalledNeedsUpdate(dir, payload.fsys)
		if pathErr != nil && !installed {
			continue
		}
		reported = true
		payloadCurrent := false
		payloadStatus := currentStale(!installed, "up to date", "missing")
		if inspectErr != nil {
			checkErr = errors.Join(checkErr, fmt.Errorf("check %s skills: %w", target.command, inspectErr))
			payloadStatus = fmt.Sprintf("unable to check (%v)", inspectErr)
		} else if installed {
			payloadStatus = currentStale(payloadStale, "up to date", "stale")
			payloadCurrent = !payloadStale
		} else {
			payloadStale = true
		}
		_, _ = fmt.Fprintf(out, "boss skills: %s\n  skills dir: %s\n  installed: %s\n  payload source: %s\n  payload: %s\n", target.command, dir, yesNo(installed), payloadSource, payloadStatus)
		if payloadStale {
			drift = true
			advice, adviceErr := skillCheckRemedyAdvice(payload, dir, installed)
			if adviceErr != nil {
				checkErr = errors.Join(checkErr, fmt.Errorf("check %s skills: %w", target.command, adviceErr))
			}
			_, _ = fmt.Fprintf(out, "  ⚠ drift — installed skills differ from the selected payload; %s\n", advice.inline())
		}
		if !found {
			_, _ = fmt.Fprintln(out, "  sources: not found in this checkout (no skill sources in this checkout)")
			continue
		}
		_, _ = fmt.Fprintf(out, "  sources: %s\n  binary: %s\n", srcRoot, currentStale(binaryStale, "current", "STALE"))
		if binaryStale {
			warning := binarySkillsDriftCheckWarning(srcRoot)
			if payloadCurrent {
				warning = binarySkillsDriftCurrentWarning(srcRoot)
			}
			_, _ = fmt.Fprintln(out, "  "+warning)
		}
	}
	if checkErr != nil {
		return checkErr
	}
	if !reported {
		_, _ = fmt.Fprintln(out, "boss skills: no supported agents found on PATH")
		if !found {
			_, _ = fmt.Fprintln(out, "  sources: not found in this checkout (no skill sources in this checkout)")
		} else {
			_, _ = fmt.Fprintf(out, "  sources: %s\n  binary: %s\n", srcRoot, currentStale(binaryStale, "current", "STALE"))
			if binaryStale {
				_, _ = fmt.Fprintln(out, "  "+binarySkillsDriftCheckWarning(srcRoot))
			}
		}
	}
	if drift {
		return fmt.Errorf("skill drift detected")
	}
	return nil
}

func validateSkillAgentFilter(only string) error {
	if only == "" {
		return nil
	}
	for _, target := range skillInstallAgents {
		if target.command == only {
			return nil
		}
	}
	return fmt.Errorf("unknown agent %q (want claude or codex)", only)
}

func runSkillGate(out io.Writer, only string) error {
	if err := validateSkillAgentFilter(only); err != nil {
		return err
	}
	only = skillGateAgentFilter(only)
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	srcRoot, found := libskillinstall.FindSourceRoot(cwd)
	if !found {
		return nil
	}

	payload := selectedSkillPayload{srcRoot: srcRoot, fromSource: true}
	repoRoot := repoRootFromSourceRoot(srcRoot)
	var gateErr error
	for _, target := range skillInstallAgents {
		if only != "" && target.command != only {
			continue
		}
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			return fmt.Errorf("%s skills dir: %w", target.command, err)
		}
		report, err := libskillinstall.SourceDriftReport(dir, srcRoot)
		if err != nil {
			gateErr = errors.Join(gateErr, fmt.Errorf("check %s skills: %w", target.command, err))
			continue
		}
		if len(report.Entries) == 0 {
			// A zero exit states its own coverage rather than being a silent
			// green a caller can only read as "everything matched".
			_, _ = fmt.Fprintf(out, "boss skills gate: %s %s — compared %d file(s) · skills dir: %s · payload source: %s\n",
				target.command, currentStale(!report.Installed, "clean", "no skills installed"), report.Compared, dir, srcRoot)
			continue
		}

		selfEdited, unexplained, fallback := classifySkillGateDrift(repoRoot, report.Entries)
		if len(selfEdited) > 0 {
			_, _ = fmt.Fprintf(out, "boss skills gate: %s self-edited drift: %s", target.command, strings.Join(selfEdited, ", "))
			if fallback {
				_, _ = fmt.Fprint(out, " (origin/HEAD unavailable; used git status fallback)")
			}
			_, _ = fmt.Fprintln(out)
		}
		if len(unexplained) > 0 {
			gateErr = errors.Join(gateErr, fmt.Errorf("skill drift detected"))
			_, _ = fmt.Fprintf(out, "boss skills gate: %s skill drift detected", target.command)
			if fallback {
				_, _ = fmt.Fprint(out, " (origin/HEAD unavailable; used git status fallback)")
			}
			_, _ = fmt.Fprintln(out)
			entries := skillDriftEntriesWithDirection(repoRoot, dir, unexplained)
			for _, entry := range entries {
				_, _ = fmt.Fprintf(out, "  - %s\n", entry.label())
			}
			advice := composeSkillInstallRemedy(payload, entries, skillCheckoutHasAncestry(repoRoot), true)
			for _, line := range advice.lines("  ") {
				_, _ = fmt.Fprintln(out, line)
			}
		}
	}
	return gateErr
}

func skillGateAgentFilter(only string) string {
	if only != "" {
		return only
	}
	switch filepath.Base(filepath.Dir(filepath.Clean(os.Getenv("BOSS_SKILLS_HOME")))) {
	case ".claude":
		return "claude"
	case ".codex":
		return "codex"
	default:
		return ""
	}
}

// classifySkillGateDrift splits drift entries into those explained by an edit
// to this checkout's own skill sources and those that are not. Entries stay
// whole so their kind reaches the direction lookup instead of being shredded
// into a lookup table that answers "" for a miss.
func classifySkillGateDrift(repoRoot string, entries []libskillinstall.DriftEntry) (selfEdited []string, unexplained []libskillinstall.DriftEntry, usedFallback bool) {
	branchBase, baseErr := checkoutGitOutput(repoRoot, "merge-base", "HEAD", "origin/HEAD")
	if baseErr != nil || strings.TrimSpace(branchBase) == "" {
		usedFallback = true
		branchBase = ""
	}
	for _, entry := range entries {
		rel := entry.Path
		sourcePath := filepath.Join(libskillinstall.SourceRelPath, "skills", filepath.FromSlash(strings.TrimSuffix(rel, "/")))
		diff := ""
		var err error
		if branchBase != "" {
			diff, err = checkoutGitOutput(repoRoot, "diff", "--name-only", branchBase, "--", sourcePath)
			if err != nil {
				usedFallback = true
				diff = ""
			}
		}
		status, statusErr := checkoutGitOutput(repoRoot, "status", "--porcelain", "--", sourcePath)
		if statusErr == nil && strings.TrimSpace(status) != "" {
			diff = status
			err = nil
		}
		if err == nil && strings.TrimSpace(diff) != "" {
			selfEdited = append(selfEdited, rel)
			continue
		}
		unexplained = append(unexplained, entry)
	}
	return selfEdited, unexplained, usedFallback
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func currentStale(stale bool, current, outdated string) string {
	if stale {
		return outdated
	}
	return current
}

func skillInstallRemedy(payload selectedSkillPayload) string {
	if payload.fromSource {
		root := repoRootFromSourceRoot(payload.srcRoot)
		return trustCheckoutSkillSourcesEnv + "=1 " + shellQuote(filepath.Join(root, "bin", "boss")) + " skills install"
	}
	return "boss skills install"
}

// skillDriftDirection is how the installed bytes at a drifted path relate to
// this checkout's recorded history of the matching source file. It decides
// whether a reinstall of that path is a lossless forward fix.
type skillDriftDirection string

const (
	// skillDriftLossless replaces no installed content, so no history is needed.
	skillDriftLossless skillDriftDirection = "lossless"
	// skillDriftBehind is a reachable earlier revision of the source path.
	skillDriftBehind skillDriftDirection = "behind"
	// skillDriftUnrecoverable is content no reachable revision of the source
	// path contains, so a reinstall would replace bytes this checkout cannot
	// restore.
	skillDriftUnrecoverable skillDriftDirection = "unrecoverable"
	// skillDriftUnknown is an undecided lookup: git failed, the object is not
	// present locally, or the bounded lookback ended without a match. Treated as
	// unrecoverable for the remedy decision, but labelled honestly.
	skillDriftUnknown skillDriftDirection = "unknown"
)

// skillDriftHistoryLookback bounds the per-path history walk so a pathological
// history cannot turn a preflight into a stall, at the price of an "unknown"
// verdict that fails safe toward not destroying.
const skillDriftHistoryLookback = 500

// skillDriftHistoryGit reads a drifted path's recorded history. It is a seam so
// a test can make the query fail without removing git from the machine.
var skillDriftHistoryGit = checkoutGitOutput

// skillDriftEntry pairs a library drift kind with the direction resolved here.
// Direction is a property of the checkout, so the library stays repository
// unaware and this command owns the git seam.
type skillDriftEntry struct {
	path      string
	kind      libskillinstall.DriftKind
	direction skillDriftDirection
}

func (e skillDriftEntry) label() string {
	return fmt.Sprintf("%s (%s, %s)", e.path, e.kind, e.direction)
}

// skillCheckoutHasAncestry reports whether this checkout has any history to
// consult. A repository without a resolvable HEAD cannot answer the direction
// question at all, which is distinct from answering it and not deciding.
func skillCheckoutHasAncestry(repoRoot string) bool {
	head, err := skillDriftHistoryGit(repoRoot, "rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil && strings.TrimSpace(head) != ""
}

// resolveSkillDriftDirection reports how one drifted path's installed copy
// relates to this checkout's history of the matching source file.
//
// Kinds that replace no installed content are lossless without a git call: a
// reinstall that adds an absent file, restores an executable bit onto bytes
// that already match the payload, or relinks a top-level symlink destroys
// nothing.
func resolveSkillDriftDirection(repoRoot, dir, rel string, kind libskillinstall.DriftKind) skillDriftDirection {
	switch kind {
	case libskillinstall.DriftAbsent, libskillinstall.DriftMode:
		return skillDriftLossless
	case libskillinstall.DriftBrokenSymlink:
		return skillTopLevelEntryDirection(dir, rel)
	case libskillinstall.DriftContent, libskillinstall.DriftUnexpected:
		// Decided from this checkout's recorded history, below.
	}
	if strings.HasSuffix(rel, "/") {
		// A stale installed skill directory. The reinstall removes a tree this
		// checkout has no source path for, so no forward revision of it exists.
		return skillDriftUnrecoverable
	}
	if isTopLevelSkillKey(rel) {
		// The library keys a leftover top-level entry on its bare skill name,
		// so it lives at <dir>/<name> rather than under the namespace. Hashing
		// the namespaced path instead would always fail and label the entry
		// unknown, where its namespaced sibling "<name>/" is correctly called
		// unrecoverable — one removed skill, two directions.
		return skillTopLevelEntryDirection(dir, rel)
	}
	installedPath := filepath.Join(dir, libskillinstall.Namespace, filepath.FromSlash(rel))
	sourcePath := filepath.Join(libskillinstall.SourceRelPath, "skills", filepath.FromSlash(rel))
	return resolveSkillDriftAncestry(repoRoot, installedPath, sourcePath)
}

// isTopLevelSkillKey reports whether a drift path is the library's bare
// top-level skill name key space — an entry directly in the agent skills dir —
// rather than a payload-relative path under the namespace. Namespaced skill
// directories carry a trailing slash and namespaced files carry their skill
// directory, so only a bare boss skill name lands here.
func isTopLevelSkillKey(rel string) bool {
	if strings.Contains(rel, "/") {
		return false
	}
	return rel == "boss" || strings.HasPrefix(rel, "boss-")
}

// skillTopLevelEntryDirection decides a drifted entry that lives directly in
// the agent skills dir rather than under the namespace.
//
// extract() os.RemoveAll's every top-level boss-* entry — its own comment says
// "(symlinks or real directories)" — so a reinstall is lossless only when there
// is nothing there or the entry is genuinely a symlink it merely relinks. A
// real directory at <dir>/<skill> is the legacy pre-namespacing layout: a tree
// the reinstall deletes and this checkout has no source path to reproduce.
func skillTopLevelEntryDirection(dir, rel string) skillDriftDirection {
	info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(strings.TrimSuffix(rel, "/"))))
	switch {
	case err != nil && os.IsNotExist(err):
		return skillDriftLossless
	case err != nil:
		return skillDriftUnknown
	case info.Mode()&os.ModeSymlink != 0:
		return skillDriftLossless
	default:
		return skillDriftUnrecoverable
	}
}

// resolveSkillDriftAncestry hashes the installed file as a git blob and asks
// whether that blob is the content of sourcePath at any commit reachable from
// HEAD, in one bounded git log per path rather than one per commit.
func resolveSkillDriftAncestry(repoRoot, installedPath, sourcePath string) skillDriftDirection {
	blob, err := skillDriftHistoryGit(repoRoot, "hash-object", "--", installedPath)
	if err != nil || blob == "" {
		return skillDriftUnknown
	}
	out, err := skillDriftHistoryGit(repoRoot, "log", "--format=%H", "--raw", "--no-renames",
		"--abbrev=40", fmt.Sprintf("-n%d", skillDriftHistoryLookback), "HEAD", "--", sourcePath)
	if err != nil {
		return skillDriftUnknown
	}
	commits := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, ":") {
			commits++
			continue
		}
		// ":<oldmode> <newmode> <oldblob> <newblob> <status>\t<path>"
		fields := strings.Fields(strings.ReplaceAll(line, "\t", " "))
		if len(fields) < 4 {
			continue
		}
		for _, candidate := range fields[2:4] {
			if candidate == blob {
				return skillDriftBehind
			}
		}
	}
	if commits >= skillDriftHistoryLookback {
		return skillDriftUnknown
	}
	return skillDriftUnrecoverable
}

func skillDriftEntriesWithDirection(repoRoot, dir string, entries []libskillinstall.DriftEntry) []skillDriftEntry {
	resolved := make([]skillDriftEntry, 0, len(entries))
	for _, entry := range entries {
		resolved = append(resolved, skillDriftEntry{
			path:      entry.Path,
			kind:      entry.Kind,
			direction: resolveSkillDriftDirection(repoRoot, dir, entry.Path, entry.Kind),
		})
	}
	return resolved
}

const skillRemedyUnverifiedNote = "reinstall direction unverified: no checkout history to compare the installed copy against"

const skillRemedyWithheldLead = "reinstall withheld — it would overwrite installed content this checkout cannot restore:"

// skillRemedyUndecidedLead is the withheld lead for a safety decision that
// could not be computed at all. Failing to read the installed tree while
// deciding whether a reinstall destroys it is not evidence that it does not,
// so it withholds in the same direction as a known loss.
const skillRemedyUndecidedLead = "reinstall withheld — the installed-vs-checkout comparison failed, so whether a reinstall would overwrite unrecoverable content is undecided"

// skillRemedyNextActionDestroys and skillRemedyNextActionUndecided are the
// operator's way out of a refusal. Every other outcome in this command family
// ends in a runnable command; a withheld remedy fires at a preflight that has
// just failed an unattended build, so it must not end in a dead end either.
// Neither names a command, because nothing a shell would run may be emitted on
// this path.
const skillRemedyNextActionDestroys = "next: move the listed paths out of the skills directory (or delete them if they are disposable); with nothing unrecoverable left, the reinstall command is offered again"

const skillRemedyNextActionUndecided = "next: re-run this check once the skills directory is readable and no longer changing underneath it"

// skillRemedyAdvice is a composed reinstall prescription: either a runnable
// command or an explicit refusal naming what the command would have destroyed.
type skillRemedyAdvice struct {
	command    string
	withheld   bool
	unverified bool
	// lead overrides the withheld headline when nothing could be enumerated.
	lead     string
	destroys []skillDriftEntry
}

// withheldLead is the headline for a refusal, naming why it refused.
func (a skillRemedyAdvice) withheldLead() string {
	if a.lead != "" {
		return a.lead
	}
	return skillRemedyWithheldLead
}

// nextAction is what the operator does about a refusal.
func (a skillRemedyAdvice) nextAction() string {
	if len(a.destroys) == 0 {
		return skillRemedyNextActionUndecided
	}
	return skillRemedyNextActionDestroys
}

// composeSkillInstallRemedy is the single decision point every drift surface
// uses, so the surfaces cannot drift apart in their safety behaviour.
//
// overwrites reports whether an installed tree exists for the reinstall to
// write over; ancestry reports whether this checkout has history to consult.
// Without ancestry the direction is stated as unverified rather than silently
// assumed correct; with it, any path that is not a lossless forward fix
// withholds the command entirely.
func composeSkillInstallRemedy(payload selectedSkillPayload, entries []skillDriftEntry, ancestry, overwrites bool) skillRemedyAdvice {
	command := skillInstallRemedy(payload)
	if !overwrites {
		return skillRemedyAdvice{command: command}
	}
	if !ancestry {
		return skillRemedyAdvice{command: command, unverified: true}
	}
	var destroys []skillDriftEntry
	for _, entry := range entries {
		switch entry.direction {
		case skillDriftLossless, skillDriftBehind:
			// A reinstall here adds or moves forward; nothing is lost.
		case skillDriftUnrecoverable, skillDriftUnknown:
			destroys = append(destroys, entry)
		}
	}
	if len(destroys) == 0 {
		return skillRemedyAdvice{command: command}
	}
	return skillRemedyAdvice{withheld: true, destroys: destroys}
}

func (a skillRemedyAdvice) runLine() string {
	line := "run `" + a.command + "`"
	if a.unverified {
		line += " (" + skillRemedyUnverifiedNote + ")"
	}
	return line
}

// inline renders the advice as a clause appended to a one-line drift warning.
func (a skillRemedyAdvice) inline() string {
	if !a.withheld {
		return a.runLine()
	}
	if len(a.destroys) == 0 {
		return a.withheldLead() + "; " + a.nextAction()
	}
	paths := make([]string, 0, len(a.destroys))
	for _, entry := range a.destroys {
		paths = append(paths, entry.label())
	}
	return a.withheldLead() + " " + strings.Join(paths, ", ") + "; " + a.nextAction()
}

// lines renders the advice as indented report lines under a drift verdict.
func (a skillRemedyAdvice) lines(indent string) []string {
	if !a.withheld {
		return []string{indent + a.runLine()}
	}
	out := make([]string, 0, len(a.destroys)+2)
	out = append(out, indent+a.withheldLead())
	for _, entry := range a.destroys {
		out = append(out, indent+"  "+entry.label())
	}
	return append(out, indent+a.nextAction())
}

// skillCheckRemedyAdvice resolves the same remedy decision the gate makes, from
// whatever evidence `boss skills check` has. Comparing against the binary's
// embedded payload leaves no ancestry to consult, so that surface reports the
// direction as unverified instead of asserting the reinstall is correct.
func skillCheckRemedyAdvice(payload selectedSkillPayload, dir string, installed bool) (skillRemedyAdvice, error) {
	if !installed {
		return composeSkillInstallRemedy(payload, nil, false, false), nil
	}
	if !payload.fromSource {
		return composeSkillInstallRemedy(payload, nil, false, true), nil
	}
	repoRoot := repoRootFromSourceRoot(payload.srcRoot)
	if !skillCheckoutHasAncestry(repoRoot) {
		return composeSkillInstallRemedy(payload, nil, false, true), nil
	}
	report, err := libskillinstall.SourceDriftReport(dir, payload.srcRoot)
	if err != nil {
		// The failure happened while computing the safety decision, so it is
		// neither the no-ancestry case nor a clean one: surface it and refuse.
		return skillRemedyAdvice{withheld: true, lead: skillRemedyUndecidedLead}, fmt.Errorf("resolve reinstall safety: %w", err)
	}
	if len(report.Entries) == 0 {
		// The installed tree is stale against the payload yet matches the
		// checkout source, so it moved under us. Nothing enumerable means
		// nothing decided, not that the reinstall is safe.
		return skillRemedyAdvice{withheld: true, lead: skillRemedyUndecidedLead}, nil
	}
	return composeSkillInstallRemedy(payload, skillDriftEntriesWithDirection(repoRoot, dir, report.Entries), true, true), nil
}

// runSkillSync writes the embedded skill payload into the global skill dir of each
// agent found on PATH, per mode:
//
//   - skillSyncUpdateOnly (sync): EnsureUpdated — refresh a stale tree, no-op when
//     current, and never fresh-install an empty dir (points the caller at `install`).
//   - skillSyncInstall (install): additionally Extract into a not-yet-installed dir,
//     so it works as first-time setup.
//   - skillSyncForce (install --force): Extract unconditionally.
//
// A single agent may be selected via only ("claude"/"codex"); when set, an
// unresolvable/failed agent makes the command exit non-zero, whereas in the
// all-agents mode a per-agent failure is a non-fatal warning. Successful extracts
// record the installed manifest so a later interactive boss does not re-prompt.
func runSkillSync(out io.Writer, mode skillSyncMode, only string) error {
	if only != "" {
		known := false
		for _, t := range skillInstallAgents {
			if t.command == only {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("unknown agent %q (want claude or codex)", only)
		}
	}

	payload, err := skillPayload()
	if err != nil {
		return fmt.Errorf("select skill payload: %w", err)
	}
	manifest, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		return fmt.Errorf("compute skill manifest: %w", err)
	}

	// A malformed/unreadable settings.json makes config.Load return default
	// settings alongside an error. Saving those defaults would silently replace
	// the user's real file, so on a load error we still install/refresh the skill
	// trees (the point of the command) but skip all manifest bookkeeping and the
	// save. loadErr gates every recordSkillInstall below.
	settings, loadErr := config.Load()
	if loadErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Warning: skipping skill-manifest bookkeeping; failed to load settings: %v\n", loadErr)
	}
	settingsChanged := false
	var targetErr error
	refreshed := false
	installedTargetsCurrent := true

	recordErr := func(err error) {
		if only != "" {
			targetErr = err
		}
	}
	extract := func(target skillInstallAgent, dir, verb string) {
		if err := libskillinstall.Extract(dir, payload.fsys); err != nil {
			installedTargetsCurrent = false
			_, _ = fmt.Fprintf(os.Stderr, "Warning: failed to %s %s skills: %v\n", verb, target.command, err)
			recordErr(err)
			return
		}
		refreshed = true
		_, _ = fmt.Fprintf(out, "boss skills: %s %s (%s)\n", verb, target.command, dir)
		if loadErr == nil && recordSkillInstall(&settings, target.agent, manifest) {
			settingsChanged = true
		}
	}

	for _, target := range skillInstallAgents {
		if only != "" && target.command != only {
			continue
		}
		if _, err := skillInstallLookPath(target.command); err != nil {
			if dir, dirErr := libskillinstall.DirForAgent(target.agent); dirErr == nil && libskillinstall.IsInstalled(dir) {
				installedTargetsCurrent = false
			}
			_, _ = fmt.Fprintf(out, "boss skills: %s not found on PATH, skipping\n", target.command)
			recordErr(fmt.Errorf("%s not found on PATH", target.command))
			continue
		}
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			installedTargetsCurrent = false
			_, _ = fmt.Fprintf(os.Stderr, "Warning: %s skills dir: %v\n", target.command, err)
			recordErr(err)
			continue
		}

		switch {
		case mode == skillSyncForce:
			extract(target, dir, "reinstalled")
		case mode == skillSyncInstall && !libskillinstall.IsInstalled(dir):
			// install doubles as first-time setup: fresh-install an empty dir.
			extract(target, dir, "installed")
		default:
			// Update-only: refresh a stale tree, no-op when current. sync never
			// fresh-installs (falls through to the not-installed hint below).
			updated, err := libskillinstall.EnsureUpdated(dir, payload.fsys)
			if err != nil {
				installedTargetsCurrent = false
				_, _ = fmt.Fprintf(os.Stderr, "Warning: failed to update %s skills: %v\n", target.command, err)
				recordErr(err)
				continue
			}
			switch {
			case updated:
				refreshed = true
				_, _ = fmt.Fprintf(out, "boss skills: updated %s (%s)\n", target.command, dir)
				if loadErr == nil && recordSkillInstall(&settings, target.agent, manifest) {
					settingsChanged = true
				}
			case !libskillinstall.IsInstalled(dir):
				// sync is update-only, so "up to date" would be misleading for an
				// empty dir. Point the caller at the fresh-install path instead.
				_, _ = fmt.Fprintf(out, "boss skills: %s not installed (%s); run 'boss skills install' to install\n", target.command, dir)
			default:
				_, _ = fmt.Fprintf(out, "boss skills: %s up to date (%s)\n", target.command, dir)
			}
		}
	}

	if settingsChanged {
		_ = config.Save(settings)
	}
	warnBinarySkillsDrift(payload, refreshed && installedTargetsCurrent, manifest)
	return targetErr
}
