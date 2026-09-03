package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	bossskillinstall "github.com/recurser/boss/internal/skillinstall"
	"github.com/recurser/bossalib/config"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
)

// runSkillSync and the interactive prompt share the skillInstall* seams; reuse
// setupSkillStartupTest for the temp HOME + isolated settings file, then stub the
// agents on PATH per case.

func writeSkillSources(t *testing.T, root string, fsys fs.FS) string {
	t.Helper()
	srcRoot := filepath.Join(root, libskillinstall.SourceRelPath)
	if err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(srcRoot, path), 0o755)
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(srcRoot, path)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	markTrustedCheckout(t, root)
	return srcRoot
}

const checkoutSkillFixture = "checkout-authoritative skill payload\n"

func setupCheckoutSkillSource(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	sourcePath := filepath.Join(srcRoot, "skills", "boss", "SKILL.md")
	if err := os.WriteFile(sourcePath, []byte(checkoutSkillFixture), 0o644); err != nil {
		t.Fatalf("write checkout skill source: %v", err)
	}
	t.Chdir(root)
	return srcRoot
}

func gateSkillFS() fstest.MapFS {
	return fstest.MapFS{
		"skills/boss/SKILL.md":       {Data: []byte("boss skill\n")},
		"skills/boss-build/SKILL.md": {Data: []byte("boss build skill\n")},
	}
}

func commitCheckoutAsOriginHead(t *testing.T, root string) {
	t.Helper()
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Test User", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")
	runGit(t, root, "branch", "-M", "main")
	runGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func markTrustedCheckout(t *testing.T, root string) {
	t.Helper()
	t.Setenv("BOSS_TRUST_CHECKOUT_SKILLS", "1")
	if out, err := exec.Command("git", "init", "--quiet", root).CombinedOutput(); err != nil {
		t.Fatalf("git init %q: %v\n%s", root, err, out)
	}
	if out, err := exec.Command("git", "-C", root, "remote", "add", "origin", "git@github.com:recurser/bossanova.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add origin: %v\n%s", err, out)
	}
}

func TestSameFilesystemPathUsesFilesystemIdentity(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original")
	alias := filepath.Join(dir, "alias")
	if err := os.WriteFile(original, []byte("content"), 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	if err := os.Link(original, alias); err != nil {
		t.Fatalf("create hard link: %v", err)
	}
	if !sameFilesystemPath(original, alias) {
		t.Fatal("sameFilesystemPath rejected hard-link aliases of the same file")
	}
}

func TestSkillPayloadSnapshotsCheckoutBytes(t *testing.T) {
	srcRoot := setupCheckoutSkillSource(t)

	payload, err := skillPayload()
	if err != nil {
		t.Fatalf("skillPayload: %v", err)
	}
	if !payload.fromSource || payload.srcRoot != srcRoot {
		t.Fatalf("skillPayload() = (%q, %t), want (%q, true)", payload.srcRoot, payload.fromSource, srcRoot)
	}
	wantManifest, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		t.Fatalf("manifest selected payload: %v", err)
	}

	if err := os.WriteFile(
		filepath.Join(srcRoot, "skills", "boss", "SKILL.md"),
		[]byte("source changed after payload selection\n"),
		0o644,
	); err != nil {
		t.Fatalf("mutate checkout source: %v", err)
	}

	dest := t.TempDir()
	if err := libskillinstall.Extract(dest, payload.fsys); err != nil {
		t.Fatalf("extract selected payload: %v", err)
	}
	got, err := os.ReadFile(installedBossSkillPath(dest))
	if err != nil {
		t.Fatalf("read installed skill: %v", err)
	}
	if string(got) != checkoutSkillFixture {
		t.Fatalf("installed bytes = %q, want snapshotted %q", got, checkoutSkillFixture)
	}
	gotManifest, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		t.Fatalf("recompute selected payload manifest: %v", err)
	}
	if gotManifest != wantManifest {
		t.Fatalf("selected payload manifest changed after source mutation: got %q, want %q", gotManifest, wantManifest)
	}
}

// switchingSkillFS models a multi-file checkout update that completes after the
// first file has been copied. The directory walk still sees both names, but a
// one-pass snapshot would contain old boss bytes and new claude bytes.
type switchingSkillFS struct {
	current    fs.FS
	next       fs.FS
	switchPath string
	switched   bool
}

func (f *switchingSkillFS) Open(name string) (fs.File, error) {
	file, err := f.current.Open(name)
	if name == f.switchPath && !f.switched {
		f.current = f.next
		f.switched = true
	}
	return file, err
}

func TestSnapshotSkillFSRetriesWhenSourceChangesDuringCapture(t *testing.T) {
	oldSource := fstest.MapFS{
		"skills/boss/SKILL.md":       &fstest.MapFile{Data: []byte("old boss\n")},
		"skills/boss-build/SKILL.md": &fstest.MapFile{Data: []byte("old boss build\n")},
		"skills/claude/SKILL.md":     &fstest.MapFile{Data: []byte("old claude\n")},
	}
	newSource := fstest.MapFS{
		"skills/boss/SKILL.md":       &fstest.MapFile{Data: []byte("new boss\n")},
		"skills/boss-build/SKILL.md": &fstest.MapFile{Data: []byte("new boss build\n")},
		"skills/claude/SKILL.md":     &fstest.MapFile{Data: []byte("new claude\n")},
	}
	source := &switchingSkillFS{
		current:    oldSource,
		next:       newSource,
		switchPath: "skills/boss/SKILL.md",
	}

	snapshot, err := snapshotSkillFS(source)
	if err != nil {
		t.Fatalf("snapshotSkillFS: %v", err)
	}
	if !source.switched {
		t.Fatal("source was not changed during capture")
	}
	for path, want := range map[string]string{
		"skills/boss/SKILL.md":   "new boss\n",
		"skills/claude/SKILL.md": "new claude\n",
	} {
		got, err := fs.ReadFile(snapshot, path)
		if err != nil {
			t.Fatalf("read snapshotted %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("snapshotted %s = %q, want stable retry bytes %q", path, got, want)
		}
	}
}

func TestSnapshotSkillFSWithGuardRetriesWhenRewriteGenerationChangesAfterCapture(t *testing.T) {
	generation := "before"
	guardCalls := 0
	guard := func() (bool, error) {
		guardCalls++
		if guardCalls == 2 {
			// Model a rewrite that starts after the captures then finishes before
			// the post-capture stability guard observes the absent lock.
			generation = "after"
		}
		return true, nil
	}

	_, err := snapshotSkillFSWithGuardAndGeneration(
		bossskillinstall.SkillsFS,
		guard,
		func() (string, error) { return generation, nil },
	)
	if err != nil {
		t.Fatalf("snapshotSkillFSWithGuardAndGeneration: %v", err)
	}
	if guardCalls < 4 {
		t.Fatalf("guard calls = %d, want retry after rewrite generation changed", guardCalls)
	}
}

// continuouslyChangingSkillFS advances the source revision after every boss
// skill read, so no pair of captures can represent the same checkout state.
type continuouslyChangingSkillFS struct{ revision int }

func (f *continuouslyChangingSkillFS) Open(name string) (fs.File, error) {
	files := fstest.MapFS{
		"skills/boss/SKILL.md":   &fstest.MapFile{Data: []byte(fmt.Sprintf("boss revision %d\n", f.revision))},
		"skills/claude/SKILL.md": &fstest.MapFile{Data: []byte(fmt.Sprintf("claude revision %d\n", f.revision))},
	}
	file, err := files.Open(name)
	if name == "skills/boss/SKILL.md" {
		f.revision++
	}
	return file, err
}

func TestSnapshotSkillFSRejectsContinuouslyChangingSource(t *testing.T) {
	_, err := snapshotSkillFS(&continuouslyChangingSkillFS{})
	if err == nil {
		t.Fatal("snapshotSkillFS succeeded for a source that changed during every capture")
	}
	if !strings.Contains(err.Error(), "changed during capture") {
		t.Fatalf("snapshotSkillFS error = %q, want source-change error", err)
	}
}

func TestSnapshotSkillFSRejectsPayloadWithoutBossSkills(t *testing.T) {
	for name, source := range map[string]fstest.MapFS{
		"empty": {
			"skills": &fstest.MapFile{Mode: fs.ModeDir},
		},
		"unrelated files only": {
			"skills/claude/SKILL.md": &fstest.MapFile{Data: []byte("not a boss skill\n")},
		},
		"base boss skill only": {
			"skills/boss/SKILL.md": &fstest.MapFile{Data: []byte("base boss skill\n")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := snapshotSkillFS(source)
			if err == nil {
				t.Fatal("snapshotSkillFS succeeded for a payload without the required boss skills")
			}
		})
	}
}

func TestSnapshotSkillFSRejectsPayloadWithIncompletePublishedSkill(t *testing.T) {
	source := fstest.MapFS{
		"skills/boss/SKILL.md":         &fstest.MapFile{Data: []byte("base boss skill\n")},
		"skills/boss-build/SKILL.md":   &fstest.MapFile{Data: []byte("published skill\n")},
		"skills/boss-repair/README.md": &fstest.MapFile{Data: []byte("not a skill\n")},
	}

	_, err := snapshotSkillFS(source)
	if err == nil {
		t.Fatal("snapshotSkillFS succeeded with an incomplete published skill")
	}
	if !strings.Contains(err.Error(), `missing published skill "boss-repair"`) {
		t.Fatalf("snapshotSkillFS error = %q, want incomplete published skill error", err)
	}
}

func TestSnapshotSkillFSRejectsNonDirectoryPublishedSkill(t *testing.T) {
	source := fstest.MapFS{
		"skills/boss/SKILL.md":       &fstest.MapFile{Data: []byte("base boss skill\n")},
		"skills/boss-build/SKILL.md": &fstest.MapFile{Data: []byte("published skill\n")},
		"skills/boss-repair":         &fstest.MapFile{Data: []byte("not a directory\n")},
	}

	_, err := snapshotSkillFS(source)
	if err == nil {
		t.Fatal("snapshotSkillFS succeeded with a non-directory published skill")
	}
	if !strings.Contains(err.Error(), `non-directory published skill "boss-repair"`) {
		t.Fatalf("snapshotSkillFS error = %q, want non-directory published skill error", err)
	}
}

func TestSnapshotCheckoutSkillFSRejectsConcurrentGitUpdate(t *testing.T) {
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	lockPath := filepath.Join(root, ".git", "index.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("create checkout update lock: %v", err)
	}

	_, err := snapshotCheckoutSkillFS(srcRoot)
	if err == nil {
		t.Fatal("snapshotCheckoutSkillFS succeeded while the checkout was updating")
	}
	if !strings.Contains(err.Error(), "Git update lock remained held") {
		t.Fatalf("snapshotCheckoutSkillFS error = %q, want bounded Git update wait error", err)
	}
}

func TestSnapshotCheckoutSkillFSWaitsForGitUpdate(t *testing.T) {
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	lockPath := filepath.Join(root, ".git", "index.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("create checkout update lock: %v", err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := os.Remove(lockPath); err != nil {
			t.Errorf("release checkout update lock: %v", err)
		}
		close(released)
	}()
	defer func() { <-released }()

	if _, err := snapshotCheckoutSkillFS(srcRoot); err != nil {
		t.Fatalf("snapshotCheckoutSkillFS rejected source after Git update released: %v", err)
	}
}

func TestSnapshotCheckoutSkillFSRejectsConcurrentNonGitSkillRewrite(t *testing.T) {
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	lockPath := filepath.Join(root, ".git", skillSourceRewriteLock)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("create non-Git skill rewrite lock: %v", err)
	}

	_, err := snapshotCheckoutSkillFS(srcRoot)
	if err == nil {
		t.Fatal("snapshotCheckoutSkillFS succeeded while a non-Git skill rewrite was in progress")
	}
	if !strings.Contains(err.Error(), "skill rewrite lock remained held") {
		t.Fatalf("snapshotCheckoutSkillFS error = %q, want bounded rewrite-lock wait error", err)
	}
}

func TestSnapshotCheckoutSkillFSMarksReclaimedRewriteAsIncomplete(t *testing.T) {
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	lockPath := filepath.Join(root, ".git", skillSourceRewriteLock)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("create abandoned non-Git skill rewrite lock: %v", err)
	}
	if err := os.Chtimes(lockPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("age abandoned non-Git skill rewrite lock: %v", err)
	}

	if _, err := snapshotCheckoutSkillFS(srcRoot); err == nil || !strings.Contains(err.Error(), "rewrite is incomplete") {
		t.Fatalf("snapshotCheckoutSkillFS error = %v, want incomplete rewrite rejection", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("abandoned non-Git skill rewrite lock remained after recovery: %v", err)
	}
	if _, err := snapshotCheckoutSkillFS(srcRoot); err == nil || !strings.Contains(err.Error(), "rewrite is incomplete") {
		t.Fatalf("snapshotCheckoutSkillFS error after recovery = %v, want incomplete rewrite rejection", err)
	}
	generationPath := filepath.Join(root, ".git", skillSourceRewriteGeneration)
	if err := os.WriteFile(generationPath, []byte("repaired\n"), 0o600); err != nil {
		t.Fatalf("record completed rewrite generation: %v", err)
	}
	if _, err := snapshotCheckoutSkillFS(srcRoot); err != nil {
		t.Fatalf("snapshotCheckoutSkillFS rejected repaired source: %v", err)
	}
}

func TestSnapshotCheckoutSkillFSMarksReclaimedRewriteClaimAsIncomplete(t *testing.T) {
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	lockPath := filepath.Join(root, ".git", skillSourceRewriteLock)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("create abandoned non-Git skill rewrite lock: %v", err)
	}
	if err := os.Chtimes(lockPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("age abandoned non-Git skill rewrite lock: %v", err)
	}
	claimPath := lockPath + skillSourceRewriteLockReclaimSuffix
	if err := os.Mkdir(claimPath, 0o700); err != nil {
		t.Fatalf("create abandoned non-Git skill rewrite claim: %v", err)
	}
	if err := os.Chtimes(claimPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("age abandoned non-Git skill rewrite claim: %v", err)
	}

	if _, err := snapshotCheckoutSkillFS(srcRoot); err == nil || !strings.Contains(err.Error(), "rewrite is incomplete") {
		t.Fatalf("snapshotCheckoutSkillFS error = %v, want incomplete rewrite rejection", err)
	}
	if _, err := os.Stat(claimPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("abandoned non-Git skill rewrite claim remained after recovery: %v", err)
	}
}

func TestCheckoutSkillSnapshotLockPublishesReclaimOwnerAtomically(t *testing.T) {
	root := t.TempDir()
	writeSkillSources(t, root, bossskillinstall.SkillsFS)
	lockPath := filepath.Join(root, ".git", skillSourceRewriteLock)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("create abandoned skill rewrite lock: %v", err)
	}
	if err := os.Chtimes(lockPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("age abandoned skill rewrite lock: %v", err)
	}

	oldClaimPublished := checkoutSkillSourceRewriteLockClaimPublished
	t.Cleanup(func() { checkoutSkillSourceRewriteLockClaimPublished = oldClaimPublished })
	checkoutSkillSourceRewriteLockClaimPublished = func(claimPath string) {
		identity, abandoned, err := checkoutSkillSourceRewriteLockClaimAbandoned(claimPath)
		if err != nil {
			t.Fatalf("inspect published reclaim claim: %v", err)
		}
		if abandoned || !identity.ownerPresent {
			t.Fatal("published reclaim claim was visible without a live owner")
		}
	}

	if _, _, err := acquireCheckoutSkillSourceRewriteLock(root); err != nil {
		t.Fatalf("acquire checkout skill snapshot lock: %v", err)
	}
}

func TestSnapshotCheckoutSkillFSWaitsForLiveRewriteLock(t *testing.T) {
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	lock, acquired, err := acquireCheckoutSkillSourceRewriteLock(root)
	if err != nil || !acquired {
		t.Fatalf("acquire live skill rewrite lock = (%t, %v), want (true, nil)", acquired, err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond)
		if err := lock.release(); err != nil {
			t.Errorf("release live skill rewrite lock: %v", err)
		}
		close(released)
	}()

	if _, err := snapshotCheckoutSkillFS(srcRoot); err != nil {
		t.Fatalf("snapshotCheckoutSkillFS rejected source after live writer released: %v", err)
	}
	<-released
}

func TestCheckoutSkillSnapshotLockKeepsSuccessorToAbandonedClaim(t *testing.T) {
	root := t.TempDir()
	claimPath := filepath.Join(root, "claim")
	if err := os.Mkdir(claimPath, 0o700); err != nil {
		t.Fatalf("create abandoned claim: %v", err)
	}
	if err := os.Chtimes(claimPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("age abandoned claim: %v", err)
	}

	oldRecoveryAcquired := checkoutSkillSourceRewriteLockClaimRecoveryAcquired
	t.Cleanup(func() { checkoutSkillSourceRewriteLockClaimRecoveryAcquired = oldRecoveryAcquired })
	checkoutSkillSourceRewriteLockClaimRecoveryAcquired = func() {
		if err := os.Rename(claimPath, claimPath+".abandoned"); err != nil {
			t.Fatalf("retire abandoned claim: %v", err)
		}
		if err := os.Mkdir(claimPath, 0o700); err != nil {
			t.Fatalf("create successor claim: %v", err)
		}
		createdAt := time.Now().UnixMilli()
		metadata, err := json.Marshal(checkoutSkillSourceRewriteLockMetadata{CreatedAt: &createdAt, PID: os.Getpid()})
		if err != nil {
			t.Fatalf("encode successor claim owner: %v", err)
		}
		if err := os.WriteFile(filepath.Join(claimPath, skillSourceRewriteLockReclaimOwnerFile), metadata, 0o600); err != nil {
			t.Fatalf("write successor claim owner: %v", err)
		}
	}

	reclaimed, err := reclaimAbandonedCheckoutSkillSourceRewriteLockClaim(claimPath)
	if err != nil {
		t.Fatalf("reclaim abandoned claim: %v", err)
	}
	if reclaimed {
		t.Fatal("reclaim abandoned claim reclaimed a live successor")
	}
	if _, err := os.Stat(filepath.Join(claimPath, skillSourceRewriteLockReclaimOwnerFile)); err != nil {
		t.Fatalf("live successor claim was removed: %v", err)
	}
}

func TestCheckoutSkillSnapshotLockReclaimsAbandonedClaimRecovery(t *testing.T) {
	root := t.TempDir()
	claimPath := filepath.Join(root, "claim")
	if err := os.Mkdir(claimPath, 0o700); err != nil {
		t.Fatalf("create abandoned claim: %v", err)
	}
	if err := os.Chtimes(claimPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("age abandoned claim: %v", err)
	}
	recoveryPath := filepath.Join(claimPath, skillSourceRewriteLockReclaimRecoveryFile)
	if err := os.WriteFile(recoveryPath, nil, 0o600); err != nil {
		t.Fatalf("create abandoned claim recovery: %v", err)
	}
	if err := os.Chtimes(recoveryPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("age abandoned claim recovery: %v", err)
	}
	if err := os.Chtimes(claimPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("re-age abandoned claim after recovery: %v", err)
	}

	reclaimed, err := reclaimAbandonedCheckoutSkillSourceRewriteLockClaim(claimPath)
	if err != nil {
		t.Fatalf("reclaim abandoned claim with abandoned recovery: %v", err)
	}
	if !reclaimed {
		t.Fatal("single recovery pass did not reclaim the abandoned claim")
	}
	if _, err := os.Stat(claimPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("abandoned claim remained after nested recovery: %v", err)
	}
}

func TestCheckoutSkillSnapshotLockRecoveryKeepsSuccessorMarker(t *testing.T) {
	root := t.TempDir()
	recoveryPath := filepath.Join(root, skillSourceRewriteLockReclaimRecoveryFile)
	if err := os.WriteFile(recoveryPath, nil, 0o600); err != nil {
		t.Fatalf("create abandoned recovery marker: %v", err)
	}
	if err := os.Chtimes(recoveryPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("age abandoned recovery marker: %v", err)
	}

	oldBeforeRecoveryReclaim := checkoutSkillSourceRewriteLockBeforeRecoveryReclaim
	t.Cleanup(func() { checkoutSkillSourceRewriteLockBeforeRecoveryReclaim = oldBeforeRecoveryReclaim })
	successor := []byte(`{"createdAt":1,"pid":1,"token":"successor"}`)
	checkoutSkillSourceRewriteLockBeforeRecoveryReclaim = func() {
		if err := os.Remove(recoveryPath); err != nil {
			t.Fatalf("remove abandoned recovery marker: %v", err)
		}
		if err := os.WriteFile(recoveryPath, successor, 0o600); err != nil {
			t.Fatalf("publish recovery successor: %v", err)
		}
	}

	reclaimed, err := reclaimAbandonedCheckoutSkillSourceRewriteLockRecovery(recoveryPath)
	if err != nil {
		t.Fatalf("reclaim abandoned recovery marker: %v", err)
	}
	if reclaimed {
		t.Fatal("reclaim abandoned recovery marker = true, want false after successor publication")
	}
	contents, err := os.ReadFile(recoveryPath)
	if err != nil {
		t.Fatalf("live recovery successor was removed: %v", err)
	}
	if string(contents) != string(successor) {
		t.Fatalf("recovery marker contents = %q, want successor", contents)
	}
}

func TestCheckoutSkillSnapshotLockRecoveryIdentityRejectsReusedFileID(t *testing.T) {
	recoveryPath := filepath.Join(t.TempDir(), skillSourceRewriteLockReclaimRecoveryFile)
	if err := os.WriteFile(recoveryPath, nil, 0o600); err != nil {
		t.Fatalf("create recovery marker: %v", err)
	}
	info, err := os.Stat(recoveryPath)
	if err != nil {
		t.Fatalf("stat recovery marker: %v", err)
	}

	if sameCheckoutSkillSourceRewriteLockRecovery(info, "", info, `{"createdAt":1,"pid":1,"token":"successor"}`) {
		t.Fatal("recovery marker identity accepted a successor reusing the observed file ID")
	}
}

func TestSnapshotCheckoutSkillFSKeepsExpiredLockOwnedByLiveWriter(t *testing.T) {
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	lockPath := filepath.Join(root, ".git", skillSourceRewriteLock)
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf(`{"createdAt":0,"pid":%d,"token":"live-writer"}`, os.Getpid())), 0o600); err != nil {
		t.Fatalf("create expired live-writer lock: %v", err)
	}
	if err := os.Chtimes(lockPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("age live-writer lock: %v", err)
	}

	_, err := snapshotCheckoutSkillFS(srcRoot)
	if err == nil {
		t.Fatal("snapshotCheckoutSkillFS succeeded while an expired lock owner was still live")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("live-writer lock was reclaimed: %v", statErr)
	}
}

func TestSnapshotCheckoutSkillFSMarksExpiredReclaimedLockAsIncomplete(t *testing.T) {
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	lockPath := filepath.Join(root, ".git", skillSourceRewriteLock)
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf(`{"createdAt":0,"pid":%d,"ownerStartTime":"different process","token":"dead-writer"}`, os.Getpid())), 0o600); err != nil {
		t.Fatalf("create expired reused-PID lock: %v", err)
	}
	if err := os.Chtimes(lockPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("age reused-PID lock: %v", err)
	}

	if _, err := snapshotCheckoutSkillFS(srcRoot); err == nil || !strings.Contains(err.Error(), "rewrite is incomplete") {
		t.Fatalf("snapshotCheckoutSkillFS error = %v, want incomplete rewrite rejection", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reused-PID lock remained after recovery: %v", err)
	}
}

func TestCheckoutSkillSnapshotLockSerializesStaleReclaimers(t *testing.T) {
	root := t.TempDir()
	writeSkillSources(t, root, bossskillinstall.SkillsFS)
	lockPath := filepath.Join(root, ".git", skillSourceRewriteLock)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("create abandoned lock: %v", err)
	}

	oldBeforeReclaim := checkoutSkillSourceRewriteLockBeforeReclaim
	t.Cleanup(func() { checkoutSkillSourceRewriteLockBeforeReclaim = oldBeforeReclaim })
	secondAcquired := false
	checkoutSkillSourceRewriteLockBeforeReclaim = func() {
		checkoutSkillSourceRewriteLockBeforeReclaim = func() {}
		_, _, _ = acquireCheckoutSkillSourceRewriteLock(root) // reclaim the stale sentinel
		_, secondAcquired, _ = acquireCheckoutSkillSourceRewriteLock(root)
	}

	if _, _, err := acquireCheckoutSkillSourceRewriteLock(root); err != nil {
		t.Fatalf("acquire checkout skill snapshot lock: %v", err)
	}
	if secondAcquired {
		t.Fatal("concurrent stale reclaimer published a successor before the first reclaimer finished")
	}
}

func TestCheckoutSkillSnapshotLockExcludesRewriteWritersDuringCapture(t *testing.T) {
	root := t.TempDir()
	writeSkillSources(t, root, bossskillinstall.SkillsFS)

	lock, acquired, err := acquireCheckoutSkillSourceRewriteLock(root)
	if err != nil {
		t.Fatalf("acquire checkout skill snapshot lock: %v", err)
	}
	if !acquired {
		t.Fatal("acquire checkout skill snapshot lock = false, want true")
	}
	if _, acquired, err := acquireCheckoutSkillSourceRewriteLock(root); err != nil || acquired {
		t.Fatalf("concurrent rewrite lock acquisition = (%t, %v), want (false, nil)", acquired, err)
	}
	if err := lock.release(); err != nil {
		t.Fatalf("release checkout skill snapshot lock: %v", err)
	}
	if _, acquired, err := acquireCheckoutSkillSourceRewriteLock(root); err != nil || !acquired {
		t.Fatalf("post-release rewrite lock acquisition = (%t, %v), want (true, nil)", acquired, err)
	}
}

func TestCaptureSkillFSRejectsNonRegularEntries(t *testing.T) {
	root := t.TempDir()
	skillsDir := filepath.Join(root, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("make skills directory: %v", err)
	}
	secret := filepath.Join(root, "secret")
	if err := os.WriteFile(secret, []byte("must not be read"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(skillsDir, "linked-skill")); err != nil {
		t.Fatalf("create skill symlink: %v", err)
	}

	_, err := captureSkillFS(os.DirFS(root))
	if err == nil {
		t.Fatal("captureSkillFS succeeded for a symlinked skill")
	}
	if !strings.Contains(err.Error(), "skills/linked-skill") || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("captureSkillFS error = %q, want non-regular skill error", err)
	}

	_, err = captureSkillFS(fstest.MapFS{
		"skills/special": &fstest.MapFile{Data: []byte("must not be read"), Mode: fs.ModeNamedPipe},
	})
	if err == nil {
		t.Fatal("captureSkillFS succeeded for a non-regular skill")
	}
	if !strings.Contains(err.Error(), "skills/special") || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("captureSkillFS error = %q, want non-regular skill error", err)
	}
}

func TestSkillPayloadUsesCheckoutSource(t *testing.T) {
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	t.Chdir(root)

	payload, err := skillPayload()
	if err != nil {
		t.Fatalf("skillPayload: %v", err)
	}
	if !payload.fromSource || payload.srcRoot != srcRoot {
		t.Fatalf("skillPayload() = (%q, %t), want (%q, true)", payload.srcRoot, payload.fromSource, srcRoot)
	}
	got, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		t.Fatalf("checkout manifest: %v", err)
	}
	want, err := libskillinstall.Manifest(os.DirFS(srcRoot))
	if err != nil {
		t.Fatalf("source manifest: %v", err)
	}
	if got != want {
		t.Fatalf("selected manifest = %q, want checkout manifest %q", got, want)
	}
}

func TestSkillPayloadAcceptsMixedCaseCanonicalOriginHost(t *testing.T) {
	root := t.TempDir()
	writeSkillSources(t, root, bossskillinstall.SkillsFS)
	if out, err := exec.Command("git", "-C", root, "remote", "set-url", "origin", "git@GitHub.com:recurser/bossanova.git").CombinedOutput(); err != nil {
		t.Fatalf("set canonical origin: %v\n%s", err, out)
	}
	t.Chdir(root)

	payload, err := skillPayload()
	if err != nil {
		t.Fatalf("skillPayload: %v", err)
	}
	if !payload.fromSource {
		t.Fatal("skillPayload rejected canonical origin with mixed-case host")
	}
}

func TestSkillPayloadRequiresExplicitCheckoutTrust(t *testing.T) {
	root := t.TempDir()
	writeSkillSources(t, root, bossskillinstall.SkillsFS)
	t.Setenv("BOSS_TRUST_CHECKOUT_SKILLS", "")
	t.Chdir(root)

	payload, err := skillPayload()
	if err != nil {
		t.Fatalf("skillPayload: %v", err)
	}
	if payload.fromSource {
		t.Fatal("skillPayload trusted checkout bytes without explicit opt-in")
	}
	got, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		t.Fatalf("embedded manifest: %v", err)
	}
	want, err := libskillinstall.Manifest(bossskillinstall.SkillsFS)
	if err != nil {
		t.Fatalf("embedded manifest: %v", err)
	}
	if got != want {
		t.Fatalf("selected manifest = %q, want embedded manifest %q", got, want)
	}
}

func TestSkillPayloadRejectsUntrustedCheckoutSource(t *testing.T) {
	root := t.TempDir()
	writeSkillSources(t, root, bossskillinstall.SkillsFS)
	if out, err := exec.Command("git", "-C", root, "remote", "set-url", "origin", "git@github.com:attacker/lookalike.git").CombinedOutput(); err != nil {
		t.Fatalf("set untrusted origin: %v\n%s", err, out)
	}
	t.Chdir(root)

	payload, err := skillPayload()
	if err != nil {
		t.Fatalf("skillPayload: %v", err)
	}
	if payload.fromSource {
		t.Fatal("skillPayload selected an untrusted checkout source")
	}
	got, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		t.Fatalf("embedded manifest: %v", err)
	}
	want, err := libskillinstall.Manifest(bossskillinstall.SkillsFS)
	if err != nil {
		t.Fatalf("want embedded manifest: %v", err)
	}
	if got != want {
		t.Fatalf("selected manifest = %q, want embedded manifest %q", got, want)
	}
}

func TestSkillPayloadRejectsCheckoutFromNonGitHubHost(t *testing.T) {
	root := t.TempDir()
	writeSkillSources(t, root, bossskillinstall.SkillsFS)
	if out, err := exec.Command("git", "-C", root, "remote", "set-url", "origin", "https://evil.example/recurser/bossanova.git").CombinedOutput(); err != nil {
		t.Fatalf("set untrusted origin: %v\n%s", err, out)
	}
	t.Chdir(root)

	payload, err := skillPayload()
	if err != nil {
		t.Fatalf("skillPayload: %v", err)
	}
	if payload.fromSource {
		t.Fatal("skillPayload selected an untrusted checkout source")
	}
}

func installedBossSkillPath(dir string) string {
	return filepath.Join(dir, libskillinstall.Namespace, "boss", "SKILL.md")
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = write
	t.Cleanup(func() { os.Stderr = oldStderr })

	fn()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// assertNamesFullSkillRebuildRemedy pins the *complete* remedy at every site that
// surfaces the stale-embed warning. Naming only `make build` is not enough:
// bin/bossd-plugin-claude embeds its own mirror of the skill payload and calls
// EnsureUpdated on the installed tree at daemon startup, so rebuilding the CLI
// alone lets a later daemon start restore the old skills. Nor is a bare
// `boss skills install` enough for the handoff: the warning can be emitted by a
// globally installed binary, which `make build plugins` does not replace, so the
// unqualified command would not exercise the rebuilt CLI. Asserting all three
// parts makes an incomplete message impossible to satisfy.
//
// Both commands are pinned ROOT-ANCHORED (`make -C <root> …`, `<root>/bin/boss …`)
// rather than as bare `make build plugins` / `./bin/boss skills install`. The
// warning fires from any directory below the root, where the cwd-relative forms
// address the wrong Makefile and a nonexistent binary — and a bare substring is
// satisfied by exactly the message this guards against, so it would be vacuous.
//
// Either spelling of root is accepted. The warning is built from os.Getwd(),
// which on macOS reports the unresolved /var/... that t.TempDir() handed out,
// while a platform or Go version that resolved it would report /private/var/...
// instead. Both are the same directory, and pinning one spelling would red on a
// perfectly correct message; the assertion stays strict because it still requires
// the full root-anchored command with an exact absolute path.
func assertNamesFullSkillRebuildRemedy(t *testing.T, got, root string) {
	t.Helper()
	roots := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		roots = append(roots, resolved)
	}
	for _, form := range []string{"make -C %s build plugins", "%s/bin/boss skills install"} {
		// Temp roots are shell-safe, so the message carries them unquoted; the
		// quoted form is covered directly by TestBinarySkillsDriftWarningQuotes*.
		matched := false
		for _, candidate := range roots {
			if strings.Contains(got, fmt.Sprintf(form, candidate)) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("output = %q, want the stale-binary warning to name %q for one of %v", got, form, roots)
		}
	}
}

// A single test pins the warning's literal text against a synthetic srcRoot, so a
// future edit that drops `plugins`, drops the `-C`/root anchoring, or breaks the
// message onto two lines reds here immediately rather than degrading the per-site
// assertions above into a weaker substring check. The srcRoot is synthetic (not a
// TempDir) so the expectation is exact and symlink-independent.
func TestBinarySkillsDriftWarningPinsFullRemedy(t *testing.T) {
	srcRoot := filepath.Join("/repo", libskillinstall.SourceRelPath)
	const want = "⚠ drift — this running boss binary's embedded skills are behind checkout source /repo/services/boss/internal/skillinstall/skills;" +
		" installed skills were refreshed from source and are current; to make the binary shippable, rebuild with `make -C /repo build plugins`, then run `/repo/bin/boss skills install`"
	got := binarySkillsDriftWarning(srcRoot)
	if got != want {
		t.Fatalf("binarySkillsDriftWarning(%q) = %q, want %q", srcRoot, got, want)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("binarySkillsDriftWarning must stay a single line, got %q", got)
	}
}

func TestWarnBinarySkillsDriftUsesSelectedSnapshot(t *testing.T) {
	srcRoot := setupCheckoutSkillSource(t)
	payload, err := skillPayload()
	if err != nil {
		t.Fatalf("skillPayload: %v", err)
	}
	installedManifest, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		t.Fatalf("manifest selected payload: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(srcRoot, "skills", "boss", "SKILL.md"),
		[]byte("source changed after install snapshot\n"),
		0o644,
	); err != nil {
		t.Fatalf("mutate checkout source: %v", err)
	}

	got := captureStderr(t, func() {
		warnBinarySkillsDrift(payload, true, installedManifest)
	})
	if !strings.Contains(got, binarySkillsDriftWarning(srcRoot)) {
		t.Fatalf("warning = %q, want snapshot-current warning %q", got, binarySkillsDriftWarning(srcRoot))
	}
}

// A checkout path with whitespace or shell metacharacters must survive being
// pasted into a shell: unquoted, `make -C /home/me/Boss Nova build plugins` splits
// into separate arguments and neither command reaches the intended directory or
// binary. The ordinary path stays unquoted so the common message is readable.
func TestBinarySkillsDriftWarningQuotesUnsafeRoots(t *testing.T) {
	for _, tc := range []struct {
		name, root, wantMake, wantBoss string
	}{
		{"ordinary path is left unquoted", "/repo", "make -C /repo build plugins", "`/repo/bin/boss skills install`"},
		{"space is quoted", "/home/me/Boss Nova", "make -C '/home/me/Boss Nova' build plugins", "`'/home/me/Boss Nova/bin/boss' skills install`"},
		{"single quote is escaped", "/home/o'brien/repo", `make -C '/home/o'\''brien/repo' build plugins`, "`'/home/o'\\''brien/repo/bin/boss' skills install`"},
		{"metacharacter is quoted", "/repo$(whoami)", "make -C '/repo$(whoami)' build plugins", "`'/repo$(whoami)/bin/boss' skills install`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := binarySkillsDriftWarning(filepath.Join(tc.root, libskillinstall.SourceRelPath))
			if !strings.Contains(got, tc.wantMake) {
				t.Fatalf("warning for root %q = %q, want it to contain %q", tc.root, got, tc.wantMake)
			}
			if !strings.Contains(got, tc.wantBoss) {
				t.Fatalf("warning for root %q = %q, want it to contain %q", tc.root, got, tc.wantBoss)
			}
		})
	}
}

// repoRootFromSourceRoot must invert FindSourceRoot's join exactly; a drift here
// would silently send operators to the wrong directory.
func TestRepoRootFromSourceRootInvertsFindSourceRootJoin(t *testing.T) {
	for _, root := range []string{"/repo", "/nested/checkout/repo"} {
		if got := repoRootFromSourceRoot(filepath.Join(root, libskillinstall.SourceRelPath)); got != root {
			t.Fatalf("repoRootFromSourceRoot for root %q = %q, want %q", root, got, root)
		}
	}
}

func TestRunSkillCheck(t *testing.T) {
	t.Run("checkout reports and compares against checkout payload without write opt-in", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		claudeDir := filepath.Join(home, ".claude", "skills")
		srcRoot := setupCheckoutSkillSource(t)
		gitDir := filepath.Join(repoRootFromSourceRoot(srcRoot), ".git")
		t.Cleanup(func() {
			if err := os.Chmod(gitDir, 0o700); err != nil {
				t.Errorf("restore checkout Git directory permissions: %v", err)
			}
		})
		if err := os.Chmod(gitDir, 0o500); err != nil {
			t.Fatalf("make checkout Git directory read-only: %v", err)
		}
		t.Setenv("BOSS_TRUST_CHECKOUT_SKILLS", "")
		if err := libskillinstall.Extract(claudeDir, os.DirFS(srcRoot)); err != nil {
			t.Fatal(err)
		}
		setAvailableSkillAgents(map[string]bool{"claude": true})

		var out bytes.Buffer
		if err := runSkillCheck(&out, "claude"); err == nil || err.Error() != "skill drift detected" {
			t.Fatalf("runSkillCheck error = %v, want skill drift detected for stale binary", err)
		}
		wantSource := "payload source: checkout (" + srcRoot + ")"
		if got := strings.Count(out.String(), "  "+wantSource+"\n"); got != 1 {
			t.Fatalf("output = %q, want exactly one %q line (got %d)", out.String(), wantSource, got)
		}
		if !strings.Contains(out.String(), "payload: up to date") {
			t.Fatalf("output = %q, want installed payload compared with checkout source", out.String())
		}
		if !strings.Contains(out.String(), "installed skills match checkout source and are current") {
			t.Fatalf("output = %q, want current-state claim for verified-current install", out.String())
		}
		if strings.Contains(out.String(), "installed skills were refreshed") {
			t.Fatalf("output = %q, read-only check must not claim it refreshed skills", out.String())
		}
	})

	t.Run("outside checkout reports binary embed payload", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
			t.Fatal(err)
		}
		setAvailableSkillAgents(map[string]bool{"claude": true})
		t.Chdir(t.TempDir())

		var out bytes.Buffer
		if err := runSkillCheck(&out, "claude"); err != nil {
			t.Fatalf("runSkillCheck: %v", err)
		}
		if got := strings.Count(out.String(), "  payload source: binary embed\n"); got != 1 {
			t.Fatalf("output = %q, want exactly one binary payload source line (got %d)", out.String(), got)
		}
	})

	t.Run("missing installed tree fails closed for detected agent", func(t *testing.T) {
		setupSkillStartupTest(t)
		setAvailableSkillAgents(map[string]bool{"claude": true})
		t.Chdir(t.TempDir())

		var out bytes.Buffer
		err := runSkillCheck(&out, "claude")
		if err == nil || err.Error() != "skill drift detected" {
			t.Fatalf("runSkillCheck error = %v, want skill drift detected", err)
		}
		if !strings.Contains(out.String(), "installed: no") || !strings.Contains(out.String(), "payload: missing") {
			t.Fatalf("output = %q, want missing installed payload reported", out.String())
		}
		if !strings.Contains(out.String(), "run `boss skills install`") {
			t.Fatalf("output = %q, want install remedy", out.String())
		}
	})

	t.Run("checkout payload drift fails closed", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
			t.Fatal(err)
		}
		srcRoot := setupCheckoutSkillSource(t)
		t.Setenv(trustCheckoutSkillSourcesEnv, "")
		setAvailableSkillAgents(map[string]bool{"claude": true})

		var out bytes.Buffer
		err := runSkillCheck(&out, "claude")
		if err == nil || err.Error() != "skill drift detected" {
			t.Fatalf("runSkillCheck error = %v, want skill drift detected", err)
		}
		if !strings.Contains(out.String(), "payload: stale") {
			t.Fatalf("output = %q, want stale against checkout payload %q", out.String(), srcRoot)
		}
		wantRemedy := "run `BOSS_TRUST_CHECKOUT_SKILLS=1 " + filepath.Join(repoRootFromSourceRoot(srcRoot), "bin", "boss") + " skills install`"
		if !strings.Contains(out.String(), wantRemedy) {
			t.Fatalf("output = %q, want checkout-selecting install remedy %q", out.String(), wantRemedy)
		}
		if strings.Contains(out.String(), "installed skills were refreshed from source and are current") {
			t.Fatalf("output = %q, must not claim a stale install is current", out.String())
		}
		assertNamesFullSkillRebuildRemedy(t, out.String(), repoRootFromSourceRoot(srcRoot))
	})

	t.Run("checkout payload selected with trust env unset", func(t *testing.T) {
		setupSkillStartupTest(t)
		srcRoot := setupCheckoutSkillSource(t)
		t.Setenv(trustCheckoutSkillSourcesEnv, "")

		payload, err := skillCheckPayload()
		if err != nil {
			t.Fatalf("skillCheckPayload: %v", err)
		}
		if !payload.fromSource {
			t.Fatalf("skillCheckPayload().fromSource = false, want true")
		}
		if payload.srcRoot != srcRoot {
			t.Fatalf("skillCheckPayload().srcRoot = %q, want %q", payload.srcRoot, srcRoot)
		}
	})

	t.Run("no sources exits cleanly", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
			t.Fatal(err)
		}
		setAvailableSkillAgents(map[string]bool{"claude": true})
		t.Chdir(t.TempDir())
		var out bytes.Buffer
		if err := runSkillCheck(&out, ""); err != nil {
			t.Fatalf("runSkillCheck: %v", err)
		}
		if !strings.Contains(out.String(), "no skill sources in this checkout") {
			t.Fatalf("output = %q", out.String())
		}
	})

	t.Run("current binary reports current", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
			t.Fatal(err)
		}
		setAvailableSkillAgents(map[string]bool{"claude": true})
		root := t.TempDir()
		writeSkillSources(t, root, bossskillinstall.SkillsFS)
		nested := filepath.Join(root, "nested")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(nested)
		var out bytes.Buffer
		if err := runSkillCheck(&out, ""); err != nil {
			t.Fatalf("runSkillCheck: %v", err)
		}
		if !strings.Contains(out.String(), "binary: current") {
			t.Fatalf("output = %q", out.String())
		}
	})

	t.Run("stale binary names rebuild remedy", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
			t.Fatal(err)
		}
		setAvailableSkillAgents(map[string]bool{"claude": true})
		root := t.TempDir()
		srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
		if err := os.WriteFile(filepath.Join(srcRoot, "skills", "boss", "SKILL.md"), []byte("newer source"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)
		var out bytes.Buffer
		if err := runSkillCheck(&out, ""); err == nil {
			t.Fatal("runSkillCheck stale binary returned nil")
		}
		assertNamesFullSkillRebuildRemedy(t, out.String(), root)
	})

	t.Run("stale binary fails without an agent on PATH", func(t *testing.T) {
		setupSkillStartupTest(t)
		setAvailableSkillAgents(map[string]bool{})
		root := t.TempDir()
		srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
		if err := os.WriteFile(filepath.Join(srcRoot, "skills", "boss", "SKILL.md"), []byte("newer source"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)
		var out bytes.Buffer
		if err := runSkillCheck(&out, ""); err == nil {
			t.Fatal("runSkillCheck stale binary without agents returned nil")
		}
		assertNamesFullSkillRebuildRemedy(t, out.String(), root)
		if strings.Contains(out.String(), "installed skills were refreshed from source and are current") {
			t.Fatalf("output = %q, must not claim an unchecked install is current", out.String())
		}
	})

	t.Run("stale installed tree fails without an agent on PATH", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
			t.Fatal(err)
		}
		srcRoot := setupCheckoutSkillSource(t)
		setAvailableSkillAgents(map[string]bool{})

		var out bytes.Buffer
		err := runSkillCheck(&out, "")
		if err == nil || err.Error() != "skill drift detected" {
			t.Fatalf("runSkillCheck error = %v, want skill drift detected", err)
		}
		if !strings.Contains(out.String(), "boss skills: claude") || !strings.Contains(out.String(), "payload: stale") {
			t.Fatalf("output = %q, want stale installed Claude tree checked against %q", out.String(), srcRoot)
		}
		if strings.Contains(out.String(), "no supported agents found on PATH") {
			t.Fatalf("output = %q, installed known-agent tree must be reported independently of PATH", out.String())
		}
	})

	t.Run("continues after one payload cannot be read", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		claudeDir := filepath.Join(home, ".claude", "skills")
		codexDir := filepath.Join(home, ".codex", "skills")
		for _, dir := range []string{claudeDir, codexDir} {
			if err := libskillinstall.Extract(dir, bossskillinstall.SkillsFS); err != nil {
				t.Fatal(err)
			}
		}

		claudeSkill := filepath.Join(claudeDir, "bossanova", "boss", "SKILL.md")
		if err := os.Remove(claudeSkill); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(claudeSkill, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(codexDir, "bossanova", "boss", "SKILL.md"), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}

		setAvailableSkillAgents(map[string]bool{"claude": true, "codex": true})
		t.Chdir(t.TempDir())
		var out bytes.Buffer
		err := runSkillCheck(&out, "")
		if err == nil || !strings.Contains(err.Error(), "check claude skills") {
			t.Fatalf("runSkillCheck error = %v, want Claude payload error", err)
		}
		if !strings.Contains(out.String(), "boss skills: claude") || !strings.Contains(out.String(), "payload: unable to check") || !strings.Contains(out.String(), "boss skills: codex") || !strings.Contains(out.String(), "payload: stale") {
			t.Fatalf("output = %q", out.String())
		}
	})
}

func TestRunSkillGate(t *testing.T) {
	t.Run("unexplained installed drift fails closed with remedy", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		root := t.TempDir()
		srcRoot := writeSkillSources(t, root, gateSkillFS())
		commitCheckoutAsOriginHead(t, root)
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, gateSkillFS()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md"), []byte("stale install\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)

		var out bytes.Buffer
		err := runSkillGate(&out, "claude")
		if err == nil || !strings.Contains(err.Error(), "skill drift detected") {
			t.Fatalf("runSkillGate error = %v, want skill drift detected", err)
		}
		for _, want := range []string{"skill drift detected", "boss/SKILL.md", "skills install", repoRootFromSourceRoot(srcRoot)} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("output = %q, want %q", out.String(), want)
			}
		}
	})

	t.Run("unqualified gate scopes to current skills home", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		root := t.TempDir()
		srcRoot := writeSkillSources(t, root, gateSkillFS())
		commitCheckoutAsOriginHead(t, root)
		claudeDir := filepath.Join(home, ".claude", "skills")
		codexDir := filepath.Join(home, ".codex", "skills")
		if err := libskillinstall.Extract(claudeDir, os.DirFS(srcRoot)); err != nil {
			t.Fatal(err)
		}
		if err := libskillinstall.Extract(codexDir, os.DirFS(srcRoot)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(codexDir, libskillinstall.Namespace, "boss", "SKILL.md"), []byte("stale install\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BOSS_SKILLS_HOME", claudeDir)
		t.Chdir(root)

		var out bytes.Buffer
		if err := runSkillGate(&out, ""); err != nil {
			t.Fatalf("runSkillGate: %v\n%s", err, out.String())
		}
		if strings.Contains(out.String(), "codex") || strings.Contains(out.String(), "skill drift detected") {
			t.Fatalf("output = %q, want current skills home only", out.String())
		}
	})

	t.Run("self edited checkout drift exits cleanly", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		root := t.TempDir()
		srcRoot := writeSkillSources(t, root, gateSkillFS())
		commitCheckoutAsOriginHead(t, root)
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, os.DirFS(srcRoot)); err != nil {
			t.Fatal(err)
		}
		edited := filepath.Join(srcRoot, "skills", "boss-build", "SKILL.md")
		if err := os.WriteFile(edited, []byte("edited on this branch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)

		var out bytes.Buffer
		if err := runSkillGate(&out, "claude"); err != nil {
			t.Fatalf("runSkillGate: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "self-edited") || !strings.Contains(out.String(), "boss-build/SKILL.md") {
			t.Fatalf("output = %q, want self-edited drift path", out.String())
		}
	})

	t.Run("branch behind origin is not self edited drift", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		root := t.TempDir()
		srcRoot := writeSkillSources(t, root, gateSkillFS())
		commitCheckoutAsOriginHead(t, root)
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, os.DirFS(srcRoot)); err != nil {
			t.Fatal(err)
		}
		edited := filepath.Join(srcRoot, "skills", "boss-build", "SKILL.md")
		if err := os.WriteFile(edited, []byte("changed on origin\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, root, "add", ".")
		runGit(t, root, "-c", "user.name=Test User", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "origin update")
		runGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
		runGit(t, root, "reset", "--hard", "HEAD~1")
		if err := os.WriteFile(filepath.Join(claudeDir, libskillinstall.Namespace, "boss-build", "SKILL.md"), []byte("stale install\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)

		var out bytes.Buffer
		err := runSkillGate(&out, "claude")
		if err == nil || !strings.Contains(err.Error(), "skill drift detected") {
			t.Fatalf("runSkillGate error = %v, want skill drift detected", err)
		}
		if strings.Contains(out.String(), "self-edited") {
			t.Fatalf("output = %q, branch-behind source must not be self-edited", out.String())
		}
		if !strings.Contains(out.String(), "  - boss-build/SKILL.md") {
			t.Fatalf("output = %q, want unexplained branch-behind drift path", out.String())
		}
	})

	t.Run("stale binary does not fail gate when installed matches checkout", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		srcRoot := setupCheckoutSkillSource(t)
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, os.DirFS(srcRoot)); err != nil {
			t.Fatal(err)
		}
		setAvailableSkillAgents(map[string]bool{"claude": true})

		var gateOut bytes.Buffer
		if err := runSkillGate(&gateOut, "claude"); err != nil {
			t.Fatalf("runSkillGate: %v\n%s", err, gateOut.String())
		}

		var checkOut bytes.Buffer
		if err := runSkillCheck(&checkOut, "claude"); err == nil || err.Error() != "skill drift detected" {
			t.Fatalf("runSkillCheck error = %v, want skill drift detected for stale binary", err)
		}
	})

	t.Run("outside checkout exits cleanly without verdict", func(t *testing.T) {
		setupSkillStartupTest(t)
		t.Chdir(t.TempDir())

		var out bytes.Buffer
		if err := runSkillGate(&out, ""); err != nil {
			t.Fatalf("runSkillGate: %v", err)
		}
		if out.String() != "" {
			t.Fatalf("output = %q, want no verdict outside checkout", out.String())
		}
	})

	t.Run("origin head fallback is named for self edited drift", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		root := t.TempDir()
		writeSkillSources(t, root, gateSkillFS())
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, gateSkillFS()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md"), []byte("stale install\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)

		var out bytes.Buffer
		if err := runSkillGate(&out, "claude"); err != nil {
			t.Fatalf("runSkillGate: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "origin/HEAD unavailable; used git status fallback") {
			t.Fatalf("output = %q, want fallback disclosure", out.String())
		}
	})

	t.Run("mixed drift fails only unexplained paths", func(t *testing.T) {
		home := setupSkillStartupTest(t)
		root := t.TempDir()
		srcRoot := writeSkillSources(t, root, gateSkillFS())
		commitCheckoutAsOriginHead(t, root)
		claudeDir := filepath.Join(home, ".claude", "skills")
		if err := libskillinstall.Extract(claudeDir, os.DirFS(srcRoot)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(srcRoot, "skills", "boss-build", "SKILL.md"), []byte("edited source\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md"), []byte("stale install\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)

		var out bytes.Buffer
		err := runSkillGate(&out, "claude")
		if err == nil || !strings.Contains(err.Error(), "skill drift detected") {
			t.Fatalf("runSkillGate error = %v, want skill drift detected", err)
		}
		if !strings.Contains(out.String(), "self-edited drift: boss-build/SKILL.md") {
			t.Fatalf("output = %q, want self-edited path", out.String())
		}
		if !strings.Contains(out.String(), "  - boss/SKILL.md") {
			t.Fatalf("output = %q, want unexplained path", out.String())
		}
	})
}

func TestSkillInstallRemedyKeepsCheckoutTrustAndUsesCheckoutBinary(t *testing.T) {
	t.Setenv(trustCheckoutSkillSourcesEnv, "1")
	payload := selectedSkillPayload{
		srcRoot:    filepath.Join("/repo", libskillinstall.SourceRelPath),
		fromSource: true,
	}

	const want = "BOSS_TRUST_CHECKOUT_SKILLS=1 /repo/bin/boss skills install"
	if got := skillInstallRemedy(payload); got != want {
		t.Fatalf("skillInstallRemedy() = %q, want %q", got, want)
	}
}

func TestRunSkillSyncWarnsWhenBinarySkillsAreStale(t *testing.T) {
	home := setupSkillStartupTest(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	if err := os.WriteFile(filepath.Join(srcRoot, "skills", "boss", "SKILL.md"), []byte("newer source"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	var out bytes.Buffer
	got := captureStderr(t, func() {
		if err := runSkillSync(&out, skillSyncInstall, ""); err != nil {
			t.Fatalf("runSkillSync: %v", err)
		}
	})
	assertNamesFullSkillRebuildRemedy(t, got, root)
	if !strings.Contains(got, binarySkillsDriftWarning(srcRoot)) {
		t.Fatalf("warning = %q, want exact post-refresh wording %q", got, binarySkillsDriftWarning(srcRoot))
	}
}

func TestRunSkillSyncStaleBinaryDoesNotClaimUninstalledSkillsCurrent(t *testing.T) {
	setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true})
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, bossskillinstall.SkillsFS)
	if err := os.WriteFile(filepath.Join(srcRoot, "skills", "boss", "SKILL.md"), []byte("newer source"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	var out bytes.Buffer
	got := captureStderr(t, func() {
		if err := runSkillSync(&out, skillSyncUpdateOnly, "claude"); err != nil {
			t.Fatalf("runSkillSync: %v", err)
		}
	})
	if strings.Contains(got, "installed skills were refreshed from source and are current") {
		t.Fatalf("warning = %q, must not claim update-only refreshed an uninstalled tree", got)
	}
	if !strings.Contains(got, binarySkillsDriftCheckWarning(srcRoot)) {
		t.Fatalf("warning = %q, want claim-free warning %q", got, binarySkillsDriftCheckWarning(srcRoot))
	}
}

func TestRunSkillSyncUpToDateWritesNothing(t *testing.T) {
	home := setupSkillStartupTest(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stamp := filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md")
	before, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncUpdateOnly, ""); err != nil {
		t.Fatalf("runSkillSync: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "boss skills: claude up to date") {
		t.Fatalf("output = %q, want an 'up to date' line", got)
	}
	if strings.Contains(got, "[Y/n]") {
		t.Fatalf("output prompted: %q", got)
	}
	after, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("current tree was rewritten by sync")
	}
}

func TestRunSkillSyncUpdatesStaleTree(t *testing.T) {
	home := setupSkillStartupTest(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stalePath := filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncUpdateOnly, ""); err != nil {
		t.Fatalf("runSkillSync: %v", err)
	}
	if !strings.Contains(out.String(), "boss skills: updated claude") {
		t.Fatalf("output = %q, want an 'updated' line", out.String())
	}
	data, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "stale" {
		t.Fatal("stale tree not refreshed by sync")
	}
}

func TestRunSkillSyncDoesNotClobberInstalledCheckoutSkillPayload(t *testing.T) {
	home := setupSkillStartupTest(t)
	srcRoot := setupCheckoutSkillSource(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, os.DirFS(srcRoot)); err != nil {
		t.Fatalf("Extract source payload: %v", err)
	}
	installedPath := installedBossSkillPath(claudeDir)
	before, err := os.ReadFile(installedPath)
	if err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncUpdateOnly, "claude"); err != nil {
		t.Fatalf("runSkillSync: %v", err)
	}
	after, err := os.ReadFile(installedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		embed, readErr := fs.ReadFile(bossskillinstall.SkillsFS, "skills/boss/SKILL.md")
		if readErr != nil {
			t.Fatalf("read embedded skill: %v", readErr)
		}
		if bytes.Equal(after, embed) {
			t.Fatal("installed checkout payload was clobbered with the embedded payload")
		}
		t.Fatal("installed checkout payload changed to an unexpected payload")
	}
	if string(after) != checkoutSkillFixture {
		t.Fatalf("installed skill = %q, want checkout source %q", after, checkoutSkillFixture)
	}
}

func TestRunSkillSyncWritesCheckoutSkillPayloadWhenSourceIsAhead(t *testing.T) {
	home := setupSkillStartupTest(t)
	setupCheckoutSkillSource(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract embedded payload: %v", err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncUpdateOnly, "claude"); err != nil {
		t.Fatalf("runSkillSync: %v", err)
	}
	got, err := os.ReadFile(installedBossSkillPath(claudeDir))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != checkoutSkillFixture {
		embed, readErr := fs.ReadFile(bossskillinstall.SkillsFS, "skills/boss/SKILL.md")
		if readErr != nil {
			t.Fatalf("read embedded skill: %v", readErr)
		}
		if bytes.Equal(got, embed) {
			t.Fatal("installed skill came from the embedded payload, want checkout source")
		}
		t.Fatal("installed skill did not match checkout source")
	}
}

func TestRunSkillSyncOutsideCheckoutWritesEmbeddedSkillPayload(t *testing.T) {
	home := setupSkillStartupTest(t)
	t.Chdir(t.TempDir())
	claudeDir := filepath.Join(home, ".claude", "skills")
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncInstall, "claude"); err != nil {
		t.Fatalf("runSkillSync install: %v", err)
	}
	got, err := os.ReadFile(installedBossSkillPath(claudeDir))
	if err != nil {
		t.Fatal(err)
	}
	want, err := fs.ReadFile(bossskillinstall.SkillsFS, "skills/boss/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded skill: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("installed skill = %q, want embedded payload", got)
	}
}

func TestRunSkillSyncRecordsCheckoutSkillPayloadManifest(t *testing.T) {
	home := setupSkillStartupTest(t)
	srcRoot := setupCheckoutSkillSource(t)
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncInstall, "claude"); err != nil {
		t.Fatalf("runSkillSync install: %v", err)
	}
	settings, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	got := settings.SkillsInstalledManifestByAgent["claude"]
	want, err := libskillinstall.SourceManifest(srcRoot)
	if err != nil {
		t.Fatalf("SourceManifest: %v", err)
	}
	embed, err := libskillinstall.Manifest(bossskillinstall.SkillsFS)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if got != want {
		t.Fatalf("recorded manifest = %q, want source manifest %q", got, want)
	}
	if got == embed {
		t.Fatalf("recorded manifest = embedded manifest %q, want checkout source manifest", got)
	}
	if !libskillinstall.IsInstalled(filepath.Join(home, ".claude", "skills")) {
		t.Fatal("source payload was not installed")
	}
}

// A malformed settings.json makes config.Load return defaults plus an error.
// runSkillSync must still refresh the stale tree without persisting those
// defaults — otherwise config.Save would clobber the user's real (broken) file.
func TestRunSkillSyncPreservesMalformedSettings(t *testing.T) {
	home := setupSkillStartupTest(t)
	settingsPath := filepath.Join(home, "settings.json")
	malformed := []byte("{ this is not valid json")
	if err := os.WriteFile(settingsPath, malformed, 0o644); err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stalePath := filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncUpdateOnly, ""); err != nil {
		t.Fatalf("runSkillSync: %v", err)
	}
	// The stale tree is still refreshed.
	if !strings.Contains(out.String(), "boss skills: updated claude") {
		t.Fatalf("output = %q, want an 'updated' line", out.String())
	}
	data, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "stale" {
		t.Fatal("stale tree not refreshed by sync")
	}
	// The malformed settings file is left untouched, not overwritten with defaults.
	saved, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, malformed) {
		t.Fatalf("settings.json was rewritten to %q, want malformed content preserved", saved)
	}
}

// sync is strictly update-only: it must never fresh-install into an empty dir,
// and it must report that honestly (not as "up to date"), pointing at `install`.
func TestRunSkillSyncDoesNotFreshInstall(t *testing.T) {
	home := setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncUpdateOnly, ""); err != nil {
		t.Fatalf("runSkillSync: %v", err)
	}
	if libskillinstall.IsInstalled(filepath.Join(home, ".claude", "skills")) {
		t.Fatal("sync fresh-installed into an empty dir, want update-only no-op")
	}
	got := out.String()
	if !strings.Contains(got, "not installed") || !strings.Contains(got, "boss skills install") {
		// Quotes two non-adjacent fragments of one output line: ellipsis: literal-dots ok
		t.Fatalf("output = %q, want a 'not installed ... boss skills install' line for the empty tree", got)
	}
	if strings.Contains(got, "up to date") {
		t.Fatalf("output = %q, must not report a not-installed tree as 'up to date'", got)
	}
}

// install (without --force) doubles as first-time setup: it fresh-installs an
// empty dir, unlike sync.
func TestRunSkillSyncInstallFreshInstallsEmptyDir(t *testing.T) {
	home := setupSkillStartupTest(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncInstall, ""); err != nil {
		t.Fatalf("runSkillSync install: %v", err)
	}
	if !libskillinstall.IsInstalled(claudeDir) {
		t.Fatal("install did not fresh-install into an empty dir")
	}
	if !strings.Contains(out.String(), "boss skills: installed claude") {
		t.Fatalf("output = %q, want an 'installed' line", out.String())
	}
	assertAgentSkillsInstalled(t, claudeDir)
}

// install (without --force) is a no-op on an already-current tree — it does not
// rewrite files, only fresh-installs what is missing.
func TestRunSkillSyncInstallNoOpOnCurrentTree(t *testing.T) {
	home := setupSkillStartupTest(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stamp := filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md")
	before, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncInstall, ""); err != nil {
		t.Fatalf("runSkillSync install: %v", err)
	}
	if !strings.Contains(out.String(), "boss skills: claude up to date") {
		t.Fatalf("output = %q, want an 'up to date' line", out.String())
	}
	after, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("current tree was rewritten by install")
	}
}

func TestRunSkillSyncForceReinstallsCurrentTree(t *testing.T) {
	home := setupSkillStartupTest(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncForce, ""); err != nil {
		t.Fatalf("runSkillSync --force: %v", err)
	}
	if !strings.Contains(out.String(), "boss skills: reinstalled claude") {
		t.Fatalf("output = %q, want a 'reinstalled' line", out.String())
	}
	assertAgentSkillsInstalled(t, claudeDir)
}

func TestRunSkillSyncAgentFilterTouchesOnlySelectedAgent(t *testing.T) {
	home := setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true, "codex": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncForce, "codex"); err != nil {
		t.Fatalf("runSkillSync --agent codex: %v", err)
	}
	if libskillinstall.IsInstalled(filepath.Join(home, ".claude", "skills")) {
		t.Fatal("--agent codex touched the claude dir")
	}
	assertAgentSkillsInstalled(t, filepath.Join(home, ".codex", "skills"))
	if strings.Contains(out.String(), "claude") {
		t.Fatalf("output mentioned claude under --agent codex: %q", out.String())
	}
}

func TestRunSkillSyncUnknownAgentErrors(t *testing.T) {
	setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	err := runSkillSync(&out, skillSyncUpdateOnly, "gemini")
	if err == nil {
		t.Fatal("runSkillSync with unknown agent returned nil, want error")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("err = %v, want 'unknown agent'", err)
	}
}

func TestRunSkillSyncSelectedAgentMissingBinaryErrors(t *testing.T) {
	setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true}) // codex absent

	var out bytes.Buffer
	err := runSkillSync(&out, skillSyncUpdateOnly, "codex")
	if err == nil {
		t.Fatal("runSkillSync for an absent selected agent returned nil, want error")
	}
	if !strings.Contains(out.String(), "not found on PATH") {
		t.Fatalf("output = %q, want a 'not found on PATH' note", out.String())
	}
}

// The leaf subcommands select an agent via --agent, so a positional operand like
// `boss skills sync codex` must error (cobra.NoArgs) instead of silently ignoring
// "codex" and mutating every agent's global skill dir on PATH.
func TestSkillsSubcommandsRejectPositionalArgs(t *testing.T) {
	setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true, "codex": true})
	skillInstallReadAnswer = func() string {
		t.Fatal("skills subcommands must not prompt")
		return ""
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"sync positional", []string{"skills", "sync", "codex"}},
		{"install positional", []string{"skills", "install", "codex"}},
		{"check positional", []string{"skills", "check", "codex"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := skillsCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("%v accepted a positional agent, want an error", tc.args)
			}
			if !strings.Contains(err.Error(), "unknown command") &&
				!strings.Contains(err.Error(), "accepts") {
				t.Fatalf("err = %v, want a cobra positional-args rejection", err)
			}
		})
	}
}
