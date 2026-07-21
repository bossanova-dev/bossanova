package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// buildFakeOutputBase constructs a directory that mimics a Bazel output base:
//
//	<base>/execroot/_main/bazel-out/<files>
//	<base>/server/server.pid.txt
//
// When readOnly is set, the bazel-out subtree's children are marked read-only
// (0555 dirs / 0444 files) to reproduce the macOS RemoveAll failure mode that
// the mandatory `chmod -R u+w` reap step exists to defeat.
func buildFakeOutputBase(t *testing.T, base, pidContents string, readOnly bool) {
	t.Helper()
	outTree := filepath.Join(base, "execroot", "_main", "bazel-out", "k8-fastbuild", "bin")
	if err := os.MkdirAll(outTree, 0o755); err != nil {
		t.Fatalf("mkdir output tree: %v", err)
	}
	artifact := filepath.Join(outTree, "artifact.o")
	if err := os.WriteFile(artifact, []byte("built"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	if pidContents != "" {
		serverDir := filepath.Join(base, "server")
		if err := os.MkdirAll(serverDir, 0o755); err != nil {
			t.Fatalf("mkdir server dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(serverDir, "server.pid.txt"), []byte(pidContents), 0o644); err != nil {
			t.Fatalf("write pid file: %v", err)
		}
	}
	if readOnly {
		// Mark the file read-only, then the dirs, walking deepest-first so a
		// parent is not made non-writable before we chmod its children.
		if err := os.Chmod(artifact, 0o444); err != nil {
			t.Fatalf("chmod artifact ro: %v", err)
		}
		for dir := outTree; dir != filepath.Join(base, "execroot", "_main") && dir != "."; dir = filepath.Dir(dir) {
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatalf("chmod dir ro: %v", err)
			}
		}
	}
}

// linkBazelOut creates worktree/bazel-out -> target symlink, mimicking Bazel's
// convenience symlink inside a built worktree.
func linkBazelOut(t *testing.T, worktree, target string) {
	t.Helper()
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(worktree, "bazel-out")); err != nil {
		t.Fatalf("symlink bazel-out: %v", err)
	}
}

// bindBazelOutputBaseToWorktree mimics Bazel's execroot symlink forest, which
// links the module file in an output base back to its workspace.
func bindBazelOutputBaseToWorktree(t *testing.T, base, worktree string) {
	t.Helper()
	worktreeModule := filepath.Join(worktree, "MODULE.bazel")
	if _, err := os.Stat(worktreeModule); os.IsNotExist(err) {
		if err := os.WriteFile(worktreeModule, []byte("module(name = \"test\")\n"), 0o644); err != nil {
			t.Fatalf("write worktree module file: %v", err)
		}
	} else if err != nil {
		t.Fatalf("stat worktree module file: %v", err)
	}
	if err := os.Symlink(worktreeModule, filepath.Join(base, "execroot", "_main", "MODULE.bazel")); err != nil {
		t.Fatalf("link output-base module file: %v", err)
	}
}

// TestReapBazelOutputBase proves the mandatory chmod: a base whose children are
// read-only (0555/0444) is still fully removed. It also exercises the two-phase
// capture-before / reap-after contract via bazelOutputBaseForWorktree.
func TestReapBazelOutputBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink + POSIX read-only permissions are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		// root bypasses DAC write-permission checks, so RemoveAll would succeed
		// on the 0555 tree even without the mandatory chmod — the assertion would
		// then pass vacuously and stop proving chmod is load-bearing.
		t.Skip("running as root: the read-only-tree removal would not exercise chmod")
	}
	m := NewManager(zerolog.Nop())

	root := t.TempDir()
	// A base under Bazel's output-user-root (_bazel_<user>) with its execroot
	// marker bound to this worktree — the reap must recognize and remove it even
	// when the tree is read-only.
	base := filepath.Join(root, "_bazel_test", "befad95e")
	// server.pid.txt names a never-live pid; the reap ignores it either way.
	buildFakeOutputBase(t, base, "2147483000\n", true)

	worktree := filepath.Join(root, "worktree")
	linkBazelOut(t, worktree, filepath.Join(base, "execroot", "_main", "bazel-out"))
	bindBazelOutputBaseToWorktree(t, base, worktree)

	// Capture-before (symlink lives inside the worktree).
	got := m.bazelOutputBaseForWorktree(worktree)
	if got != base {
		t.Fatalf("bazelOutputBaseForWorktree = %q, want %q", got, base)
	}

	// Simulate teardown removing the worktree dir, then reap-after.
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	m.reapBazelOutputBase(got)

	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("base still present after reap (err=%v); read-only chmod must have failed", err)
	}
}

// TestClearStaleWorktreeReapsBase is an end-to-end check of the capture-before /
// reap-after wiring on a REAL registered worktree: clearStaleWorktree must reap
// the output base after removing the worktree dir, while a sibling directory
// mimicking the shared --disk_cache is left untouched.
func TestClearStaleWorktreeReapsBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink + POSIX read-only permissions are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: the read-only-tree removal would not exercise chmod")
	}
	ctx := context.Background()
	m := NewManager(zerolog.Nop())

	repoDir := initTestRepo(t)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "feature")
	if _, err := runGit(ctx, repoDir, "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatalf("worktree add: %v", err)
	}

	// Fake output base under a "_bazel_" root, with a read-only tree so the reap
	// exercises the mandatory chmod. server.pid.txt names a never-live pid.
	bazelRoot := filepath.Join(t.TempDir(), "_bazel_test")
	base := filepath.Join(bazelRoot, "deadbeef")
	buildFakeOutputBase(t, base, "2147483000\n", true)
	linkBazelOut(t, wtPath, filepath.Join(base, "execroot", "_main", "bazel-out"))
	bindBazelOutputBaseToWorktree(t, base, wtPath)

	// A sibling that mimics the shared content-addressed --disk_cache: it must
	// survive the reap (only the specific output base is removed).
	diskCache := filepath.Join(bazelRoot, "..", "bazel-bossanova-disk")
	if err := os.MkdirAll(diskCache, 0o755); err != nil {
		t.Fatalf("mkdir disk cache: %v", err)
	}

	m.clearStaleWorktree(ctx, repoDir, wtPath)

	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still present after clearStaleWorktree: err=%v", err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("bazel output base not reaped: err=%v", err)
	}
	if _, err := os.Stat(diskCache); err != nil {
		t.Fatalf("shared disk cache disturbed by reap: %v", err)
	}
}

// TestArchiveReapsBase covers the primary teardown path: Archive captures the
// base before removal and reaps it after the worktree dir (and its bazel-out
// symlink) is removed on the happy `git worktree remove` path. A regression that
// moved the capture after removal, or dropped the reap, would silently leak on
// the most common teardown path without failing any other test.
func TestArchiveReapsBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink + POSIX read-only permissions are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: the read-only-tree removal would not exercise chmod")
	}
	ctx := context.Background()
	m := NewManager(zerolog.Nop())

	repoDir := initTestRepo(t)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "feature")
	if _, err := runGit(ctx, repoDir, "worktree", "add", "-b", "feature", wtPath, "main"); err != nil {
		t.Fatalf("worktree add: %v", err)
	}

	base := filepath.Join(t.TempDir(), "_bazel_test", "abcd1234")
	buildFakeOutputBase(t, base, "2147483000\n", true)
	linkBazelOut(t, wtPath, filepath.Join(base, "execroot", "_main", "bazel-out"))
	bindBazelOutputBaseToWorktree(t, base, wtPath)

	if err := m.Archive(ctx, wtPath); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still present after Archive: err=%v", err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("bazel output base not reaped by Archive: err=%v", err)
	}
}

// TestArchiveDoesNotReapBaseWhenFallbackRemovalFails proves a failed direct
// removal leaves its output base untouched. Reaping first could SIGTERM the
// worktree's Bazel server while the worktree is still usable.
func TestArchiveDoesNotReapBaseWhenFallbackRemovalFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory removal bypasses parent write permission")
	}

	m := NewManager(zerolog.Nop())
	root := t.TempDir()
	parent := filepath.Join(root, "unremovable")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	worktree := filepath.Join(parent, "worktree")
	base := filepath.Join(root, "_bazel_test", "remove-failed")
	buildFakeOutputBase(t, base, "2147483000\n", false)
	linkBazelOut(t, worktree, filepath.Join(base, "execroot", "_main", "bazel-out"))
	bindBazelOutputBaseToWorktree(t, base, worktree)

	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("make worktree parent read-only: %v", err)
	}
	err := m.Archive(context.Background(), worktree)
	if restoreErr := os.Chmod(parent, 0o755); restoreErr != nil {
		t.Fatalf("restore worktree parent permissions: %v", restoreErr)
	}
	if err == nil {
		t.Fatal("Archive succeeded despite unwritable worktree parent")
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("Archive reaped output base after failed removal: %v", err)
	}
}

// TestClearStaleWorktreeDoesNotReapBaseWhenRemovalFails proves stale cleanup
// leaves the output base alone when neither git worktree remove nor its direct
// removal fallback can remove the worktree directory. Reaping it in that state
// could stop the Bazel server for a worktree that is still present.
func TestClearStaleWorktreeDoesNotReapBaseWhenRemovalFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory removal bypasses parent write permission")
	}

	ctx := context.Background()
	m := NewManager(zerolog.Nop())
	repoDir := initTestRepo(t)
	parent := filepath.Join(t.TempDir(), "unremovable")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("mkdir worktree parent: %v", err)
	}
	worktree := filepath.Join(parent, "feature")
	if _, err := runGit(ctx, repoDir, "worktree", "add", "-b", "feature", worktree, "main"); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	base := filepath.Join(t.TempDir(), "_bazel_test", "stale-remove-failed")
	buildFakeOutputBase(t, base, "2147483000\n", false)
	linkBazelOut(t, worktree, filepath.Join(base, "execroot", "_main", "bazel-out"))
	bindBazelOutputBaseToWorktree(t, base, worktree)

	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("make worktree parent read-only: %v", err)
	}
	m.clearStaleWorktree(ctx, repoDir, worktree)
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatalf("restore worktree parent permissions: %v", err)
	}

	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("stale worktree unexpectedly removed: %v", err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("stale cleanup reaped output base after failed removal: %v", err)
	}
}

// TestCreateForceReapsStaleBase proves the force-recreate path (branchExists &&
// Force) captures-before-removes and reaps the previously-built worktree's Bazel
// output base instead of leaking it — the one removal sub-path that removes the
// worktree dir before delegating to clearStaleWorktree (BOS-447 regression).
func TestCreateForceReapsStaleBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink + POSIX permissions are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	ctx := context.Background()
	m := NewManager(zerolog.Nop())

	repoDir := initTestRepo(t)
	wtBase := t.TempDir()
	opts := CreateOpts{
		RepoPath:        repoDir,
		RepoName:        "repo",
		WorktreeBaseDir: wtBase,
		BranchName:      "feature",
		BaseBranch:      "main",
	}

	res, err := m.Create(ctx, opts)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Plant a fake built output base + bazel-out symlink in the worktree.
	base := filepath.Join(t.TempDir(), "_bazel_test", "cafef00d")
	buildFakeOutputBase(t, base, "", false)
	linkBazelOut(t, res.WorktreePath, filepath.Join(base, "execroot", "_main", "bazel-out"))
	bindBazelOutputBaseToWorktree(t, base, res.WorktreePath)

	// Force-create the same branch → must reap the stale base, not leak it.
	forceOpts := opts
	forceOpts.Force = true
	if _, err := m.Create(ctx, forceOpts); err != nil {
		t.Fatalf("force create: %v", err)
	}

	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("force create leaked the stale output base: err=%v", err)
	}
}

// TestCreateForceDoesNotReapBaseWhenStaleDirRemovalFails proves that a force
// recreate preserves the Bazel base until an unregistered stale worktree
// directory has actually been removed. Otherwise the later Create failure
// leaves a usable worktree without its Bazel server or output base.
func TestCreateForceDoesNotReapBaseWhenStaleDirRemovalFails(t *testing.T) {
	ctx := context.Background()
	m := NewManager(zerolog.Nop())
	repoDir := initTestRepo(t)
	wtBase := t.TempDir()
	const repoName = "repo"
	const branch = "feature"
	if _, err := runGit(ctx, repoDir, "branch", branch, "main"); err != nil {
		t.Fatalf("create existing branch: %v", err)
	}

	worktreeParent := filepath.Join(wtBase, sanitizeDirName(repoName))
	worktree := filepath.Join(worktreeParent, branch)
	base := filepath.Join(t.TempDir(), "_bazel_test", "force-remove-failed")
	buildFakeOutputBase(t, base, "2147483000\n", false)
	linkBazelOut(t, worktree, filepath.Join(base, "execroot", "_main", "bazel-out"))
	bindBazelOutputBaseToWorktree(t, base, worktree)
	removeErr := errors.New("injected stale worktree removal failure")
	m.removeAll = func(path string) error {
		if path != worktree {
			t.Fatalf("removeAll path = %q, want %q", path, worktree)
		}
		return removeErr
	}
	_, err := m.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		RepoName:        repoName,
		WorktreeBaseDir: wtBase,
		BranchName:      branch,
		BaseBranch:      "main",
		Force:           true,
	})
	if err == nil {
		t.Fatal("force create succeeded despite stale worktree removal failure")
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("stale worktree unexpectedly removed: %v", err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("force create reaped output base after stale removal failed: %v", err)
	}
}

// TestReapBazelOutputBase_NoSymlink covers a worktree that was never built: no
// bazel-out symlink → empty base → reap is a clean no-op, nothing else touched.
func TestReapBazelOutputBase_NoSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-oriented test")
	}
	m := NewManager(zerolog.Nop())

	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	sibling := filepath.Join(root, "keep-me")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}

	if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
		t.Fatalf("bazelOutputBaseForWorktree = %q, want \"\"", got)
	}
	// No panic, no error return (best-effort), sibling untouched.
	m.reapBazelOutputBase("")
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling dir disturbed by no-op reap: %v", err)
	}
}

// TestReapBazelOutputBase_NoExecroot guards against RemoveAll on an unexpected
// path: a symlink target without "/execroot/" yields "" and never reaps.
func TestReapBazelOutputBase_NoExecroot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-oriented test")
	}
	m := NewManager(zerolog.Nop())

	root := t.TempDir()
	unexpected := filepath.Join(root, "not-a-bazel-tree")
	if err := os.MkdirAll(unexpected, 0o755); err != nil {
		t.Fatalf("mkdir unexpected: %v", err)
	}
	worktree := filepath.Join(root, "worktree")
	linkBazelOut(t, worktree, unexpected)

	if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
		t.Fatalf("bazelOutputBaseForWorktree = %q, want \"\" (no /execroot/)", got)
	}
	// The unexpected dir must survive: no RemoveAll should have fired.
	if _, err := os.Stat(unexpected); err != nil {
		t.Fatalf("unexpected dir was removed despite missing /execroot/: %v", err)
	}
}

// TestReapBazelOutputBase_PidHandling asserts that the reap ignores
// server.pid.txt entirely — whatever it holds, the reap never signals the
// recorded pid and still removes the base. The reap no longer sends SIGTERM
// because a recorded pid cannot be reliably tied back to this output base.
func TestReapBazelOutputBase_PidHandling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-oriented test")
	}
	m := NewManager(zerolog.Nop())

	t.Run("not-live pid", func(t *testing.T) {
		root := t.TempDir()
		base := filepath.Join(root, "base")
		// A huge pid that is never live on the test host.
		buildFakeOutputBase(t, base, strconv.Itoa(1<<30)+"\n", false)
		m.reapBazelOutputBase(base)
		if _, err := os.Stat(base); !os.IsNotExist(err) {
			t.Fatalf("base still present after reap with not-live pid: err=%v", err)
		}
	})

	t.Run("missing pid file", func(t *testing.T) {
		root := t.TempDir()
		base := filepath.Join(root, "base")
		buildFakeOutputBase(t, base, "", false) // no server/server.pid.txt
		m.reapBazelOutputBase(base)
		if _, err := os.Stat(base); !os.IsNotExist(err) {
			t.Fatalf("base still present after reap with missing pid file: err=%v", err)
		}
	})

	t.Run("unparsable pid file", func(t *testing.T) {
		root := t.TempDir()
		base := filepath.Join(root, "base")
		buildFakeOutputBase(t, base, "not-a-number\n", false)
		m.reapBazelOutputBase(base)
		if _, err := os.Stat(base); !os.IsNotExist(err) {
			t.Fatalf("base still present after reap with unparsable pid file: err=%v", err)
		}
	})

	// A live, same-user pid recorded in server.pid.txt — whether it is our own
	// process, or a pid the OS has recycled to an unrelated process or another
	// worktree's live Bazel server — must NOT be signalled. We plant the pid of a
	// real helper process and assert it keeps running after the reap. This is the
	// core guarantee of dropping the SIGTERM: the reap never kills the recorded
	// pid, so it can never interrupt an unrelated process or another build.
	t.Run("live recorded pid is never signalled", func(t *testing.T) {
		root := t.TempDir()
		base := filepath.Join(root, "base")
		helper := exec.Command("sleep", "30")
		if err := helper.Start(); err != nil {
			t.Skipf("cannot spawn helper process: %v", err)
		}
		// Wait in the background so an early exit is observable. A SIGTERM'd
		// child would otherwise linger as a zombie that still answers
		// kill(pid, 0), masking the very regression this test guards against.
		waitErr := make(chan error, 1)
		go func() { waitErr <- helper.Wait() }()
		defer func() {
			_ = helper.Process.Kill()
			<-waitErr
		}()
		buildFakeOutputBase(t, base, strconv.Itoa(helper.Process.Pid)+"\n", false)
		m.reapBazelOutputBase(base)
		// If the reap signalled the helper, `sleep` takes SIGTERM's default
		// action and Wait returns promptly. It must not: the helper must still
		// be running after a grace period.
		select {
		case err := <-waitErr:
			t.Fatalf("recorded pid was signalled by reap (helper exited: %v)", err)
		case <-time.After(500 * time.Millisecond):
		}
		if _, err := os.Stat(base); !os.IsNotExist(err) {
			t.Fatalf("base still present after reap with live recorded pid: err=%v", err)
		}
	})
}

// TestBazelOutputBaseForWorktree_Parsing unit-tests the key-parsing split on
// "/execroot/" directly, including the absent-symlink case.
func TestBazelOutputBaseForWorktree_Parsing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-oriented test")
	}
	m := NewManager(zerolog.Nop())

	t.Run("well-formed target", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		base := filepath.Join(root, "_bazel_x", "KEY")
		buildFakeOutputBase(t, base, "", false)
		linkBazelOut(t, worktree, filepath.Join(base, "execroot", "_main", "bazel-out"))
		bindBazelOutputBaseToWorktree(t, base, worktree)
		if got := m.bazelOutputBaseForWorktree(worktree); got != base {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want %q", got, base)
		}
	})

	t.Run("legacy WORKSPACE.bazel target", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		base := filepath.Join(root, "_bazel_x", "KEY")
		workspace := filepath.Join(worktree, "WORKSPACE.bazel")
		execrootWorkspace := filepath.Join(base, "execroot", "legacy_workspace")
		if err := os.MkdirAll(filepath.Join(execrootWorkspace, "bazel-out"), 0o755); err != nil {
			t.Fatalf("mkdir legacy execroot: %v", err)
		}
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatalf("mkdir worktree: %v", err)
		}
		if err := os.WriteFile(workspace, []byte("workspace(name = \"legacy_workspace\")\n"), 0o644); err != nil {
			t.Fatalf("write workspace file: %v", err)
		}
		if err := os.Symlink(workspace, filepath.Join(execrootWorkspace, "WORKSPACE.bazel")); err != nil {
			t.Fatalf("link execroot workspace file: %v", err)
		}
		linkBazelOut(t, worktree, filepath.Join(execrootWorkspace, "bazel-out"))

		if got := m.bazelOutputBaseForWorktree(worktree); got != base {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want %q", got, base)
		}
	})

	t.Run("legacy WORKSPACE target", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		base := filepath.Join(root, "_bazel_x", "KEY")
		workspace := filepath.Join(worktree, "WORKSPACE")
		execrootWorkspace := filepath.Join(base, "execroot", "legacy_workspace")
		if err := os.MkdirAll(filepath.Join(execrootWorkspace, "bazel-out"), 0o755); err != nil {
			t.Fatalf("mkdir legacy execroot: %v", err)
		}
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatalf("mkdir worktree: %v", err)
		}
		if err := os.WriteFile(workspace, []byte("workspace(name = \"legacy_workspace\")\n"), 0o644); err != nil {
			t.Fatalf("write workspace file: %v", err)
		}
		if err := os.Symlink(workspace, filepath.Join(execrootWorkspace, "WORKSPACE")); err != nil {
			t.Fatalf("link execroot workspace file: %v", err)
		}
		linkBazelOut(t, worktree, filepath.Join(execrootWorkspace, "bazel-out"))

		if got := m.bazelOutputBaseForWorktree(worktree); got != base {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want %q", got, base)
		}
	})

	// SECURITY: a worktree path supplied as a symlink may point at another live
	// workspace. Removing the supplied path removes only the symlink, so it must
	// never nominate the target workspace's Bazel output base for reaping.
	t.Run("symlinked worktree is rejected", func(t *testing.T) {
		root := t.TempDir()
		targetWorktree := filepath.Join(root, "target-worktree")
		base := filepath.Join(root, "_bazel_x", "KEY")
		buildFakeOutputBase(t, base, "", false)
		linkBazelOut(t, targetWorktree, filepath.Join(base, "execroot", "_main", "bazel-out"))
		bindBazelOutputBaseToWorktree(t, base, targetWorktree)

		worktreeLink := filepath.Join(root, "worktree-link")
		if err := os.Symlink(targetWorktree, worktreeLink); err != nil {
			t.Fatalf("symlink worktree: %v", err)
		}

		if got := m.bazelOutputBaseForWorktree(worktreeLink); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want empty for symlinked worktree", got)
		}
	})

	// SECURITY: the execroot path segment comes from the agent-writable
	// bazel-out target. It must not escape the execroot directory while matching
	// a workspace marker.
	t.Run("execroot parent segment is rejected", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		base := filepath.Join(root, "_bazel_x", "KEY")
		if err := os.MkdirAll(filepath.Join(base, "execroot"), 0o755); err != nil {
			t.Fatalf("mkdir execroot: %v", err)
		}
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatalf("mkdir worktree: %v", err)
		}
		worktreeModule := filepath.Join(worktree, "MODULE.bazel")
		if err := os.WriteFile(worktreeModule, []byte("module(name = \"test\")\n"), 0o644); err != nil {
			t.Fatalf("write worktree module file: %v", err)
		}
		if err := os.Symlink(worktreeModule, filepath.Join(base, "MODULE.bazel")); err != nil {
			t.Fatalf("link base module file: %v", err)
		}
		linkBazelOut(t, worktree, base+"/execroot/../bazel-out")

		if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want empty for parent execroot segment", got)
		}
	})

	// SECURITY: the agent-writable bazel-out symlink must not be enough to
	// nominate a disposable-looking directory. A valid output base has Bazel's
	// execroot symlink forest pointing its module file back at this worktree.
	t.Run("unbound output base is rejected", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		base := filepath.Join(root, "_bazel_fake", "important")
		buildFakeOutputBase(t, base, "", false)
		linkBazelOut(t, worktree, filepath.Join(base, "execroot", "_main", "bazel-out"))
		if err := os.WriteFile(filepath.Join(worktree, "MODULE.bazel"), []byte("module(name = \"test\")\n"), 0o644); err != nil {
			t.Fatalf("write worktree module file: %v", err)
		}

		if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want empty for unbound output base", got)
		}
	})

	// SECURITY: an agent can replace its marker with a link to another
	// workspace's marker. SameFile alone would then authorize that other
	// workspace's output base through a malicious bazel-out link.
	t.Run("symlinked workspace marker is rejected", func(t *testing.T) {
		root := t.TempDir()
		targetWorktree := filepath.Join(root, "target-worktree")
		targetBase := filepath.Join(root, "_bazel_target", "KEY")
		buildFakeOutputBase(t, targetBase, "", false)
		linkBazelOut(t, targetWorktree, filepath.Join(targetBase, "execroot", "_main", "bazel-out"))
		bindBazelOutputBaseToWorktree(t, targetBase, targetWorktree)

		attackerWorktree := filepath.Join(root, "attacker-worktree")
		linkBazelOut(t, attackerWorktree, filepath.Join(targetBase, "execroot", "_main", "bazel-out"))
		if err := os.Symlink(filepath.Join(targetWorktree, "MODULE.bazel"), filepath.Join(attackerWorktree, "MODULE.bazel")); err != nil {
			t.Fatalf("symlink attacker module file: %v", err)
		}

		if got := m.bazelOutputBaseForWorktree(attackerWorktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want empty for symlinked workspace marker", got)
		}
	})

	// SECURITY: hard links are indistinguishable to SameFile too. The execroot
	// marker must resolve to this worktree's marker path, not merely its inode.
	t.Run("hard-linked workspace marker is rejected", func(t *testing.T) {
		root := t.TempDir()
		targetWorktree := filepath.Join(root, "target-worktree")
		targetBase := filepath.Join(root, "_bazel_target", "KEY")
		buildFakeOutputBase(t, targetBase, "", false)
		linkBazelOut(t, targetWorktree, filepath.Join(targetBase, "execroot", "_main", "bazel-out"))
		bindBazelOutputBaseToWorktree(t, targetBase, targetWorktree)

		attackerWorktree := filepath.Join(root, "attacker-worktree")
		linkBazelOut(t, attackerWorktree, filepath.Join(targetBase, "execroot", "_main", "bazel-out"))
		if err := os.Link(filepath.Join(targetWorktree, "MODULE.bazel"), filepath.Join(attackerWorktree, "MODULE.bazel")); err != nil {
			t.Fatalf("hard-link attacker module file: %v", err)
		}

		if got := m.bazelOutputBaseForWorktree(attackerWorktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want empty for hard-linked workspace marker", got)
		}
	})

	t.Run("absent symlink", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatalf("mkdir worktree: %v", err)
		}
		if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want \"\"", got)
		}
	})

	// The base path itself embeds "/execroot/": a first-occurrence split would
	// truncate to a parent (/var/tmp) and reap far too much. Splitting on the
	// LAST "/execroot/" must recover the true base.
	t.Run("base path embeds execroot", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		base := filepath.Join(root, "execroot", "_bazel_x", "KEY")
		buildFakeOutputBase(t, base, "", false)
		linkBazelOut(t, worktree, filepath.Join(base, "execroot", "_main", "bazel-out"))
		bindBazelOutputBaseToWorktree(t, base, worktree)
		want := base
		if got := m.bazelOutputBaseForWorktree(worktree); got != want {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want %q", got, want)
		}
	})

	// A target that starts with "/execroot/" yields an empty base and must be
	// rejected (never RemoveAll "").
	t.Run("target starts with execroot", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		linkBazelOut(t, worktree, "/execroot/_main/bazel-out")
		if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want \"\"", got)
		}
	})

	// SECURITY: an agent-planted symlink to an arbitrary absolute path must
	// resolve to "" when its execroot does not bind it back to this worktree.
	t.Run("absolute non-bazel target is rejected", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		linkBazelOut(t, worktree, "/Users/victim/important/execroot/x")
		if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want \"\" (unbound target)", got)
		}
	})

	// A relative symlink target must be rejected: a relative base resolves
	// against the daemon CWD, not the worktree — an unexpected reap target.
	t.Run("relative target is rejected", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		linkBazelOut(t, worktree, "../../_bazel_x/KEY/execroot/_main/bazel-out")
		if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want \"\" (relative target)", got)
		}
	})

	// A "..\"-escaping absolute target that normalizes to an unbound path must
	// be rejected (Clean runs before marker binding).
	t.Run("dotdot escape out of bazel root is rejected", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		linkBazelOut(t, worktree, "/var/tmp/_bazel_x/../../../etc/execroot/x")
		if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want \"\" (dotdot escape)", got)
		}
	})

	// SECURITY: a target resolving to a custom output root itself must be rejected
	// without a binding marker; otherwise reaping it could wipe sibling bases.
	t.Run("custom output root itself is rejected", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		linkBazelOut(t, worktree, "/var/tmp/custom-output-root/execroot/_main/bazel-out")
		if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want \"\" (root, no binding)", got)
		}
	})

	// SECURITY (P1): the marker check alone is forgeable. An agent can point
	// bazel-out at an arbitrary same-user directory and plant its
	// execroot/<name>/MODULE.bazel as a symlink back to this worktree's marker,
	// so the base's marker DOES resolve to ours. The output-user-root anchor
	// (_bazel_* parent) must still reject it, because the directory is not a
	// Bazel scratch root — otherwise teardown would chmod+RemoveAll $HOME/important.
	t.Run("forged marker outside a _bazel_ root is rejected", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		// A victim directory NOT under any _bazel_<user> root.
		victim := filepath.Join(root, "victim-home", "important")
		buildFakeOutputBase(t, victim, "", false)
		linkBazelOut(t, worktree, filepath.Join(victim, "execroot", "_main", "bazel-out"))
		// Fully bind the marker — the forgeable half of the guard now passes.
		bindBazelOutputBaseToWorktree(t, victim, worktree)
		if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want \"\" (forged marker, not under _bazel_ root)", got)
		}
		// The victim directory must remain untouched.
		if _, err := os.Stat(victim); err != nil {
			t.Fatalf("victim directory disturbed: %v", err)
		}
	})

	// SECURITY (P1): the _bazel_ anchor must resolve symlinked ancestors, not
	// just check the lexical parent name. An agent can create `_bazel_fake` as a
	// symlink to a victim directory; lexically base's parent is `_bazel_fake`,
	// but RemoveAll follows the ancestor symlink to the real victim. EvalSymlinks
	// must collapse it so the real parent name (not `_bazel_*`) rejects the base.
	t.Run("symlinked _bazel_ ancestor is rejected", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		// A victim dir that is NOT itself under any _bazel_ root.
		victim := filepath.Join(root, "victim-home")
		if err := os.MkdirAll(victim, 0o755); err != nil {
			t.Fatalf("mkdir victim: %v", err)
		}
		// `_bazel_fake` looks like a Bazel root lexically but is a symlink to the
		// victim, so the real base resolves to victim/important.
		fakeRoot := filepath.Join(root, "_bazel_fake")
		if err := os.Symlink(victim, fakeRoot); err != nil {
			t.Fatalf("symlink fake bazel root: %v", err)
		}
		base := filepath.Join(fakeRoot, "important")
		buildFakeOutputBase(t, base, "", false)
		linkBazelOut(t, worktree, filepath.Join(base, "execroot", "_main", "bazel-out"))
		bindBazelOutputBaseToWorktree(t, base, worktree)
		if got := m.bazelOutputBaseForWorktree(worktree); got != "" {
			t.Fatalf("bazelOutputBaseForWorktree = %q, want \"\" (symlinked _bazel_ ancestor)", got)
		}
		// The victim's real contents must remain untouched.
		if _, err := os.Stat(filepath.Join(victim, "important")); err != nil {
			t.Fatalf("victim contents disturbed: %v", err)
		}
	})
}
