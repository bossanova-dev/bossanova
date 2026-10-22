package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// sessionPushFixture is a real bare origin, a clone of it, and a session
// worktree of that clone on branch "feature" whose first commit (A, the
// receipted head) has been pushed from the worktree. No git is mocked: the
// probe's whole value is how real git writes the remote-tracking reflog.
type sessionPushFixture struct {
	clone, worktree, origin, branch string
	receipted                       string
}

func newSessionPushFixture(t *testing.T) *sessionPushFixture {
	t.Helper()
	clone := initTestRepo(t)
	f := &sessionPushFixture{
		clone:    clone,
		worktree: filepath.Join(t.TempDir(), "wt"),
		origin:   mustGit(t, clone, "remote", "get-url", "origin"),
		branch:   "feature",
	}
	mustGit(t, clone, "worktree", "add", "-b", f.branch, f.worktree, "main")
	f.receipted = f.commitAndPush(t, "A")
	return f
}

// commit makes an empty commit in the session worktree and returns its SHA.
func (f *sessionPushFixture) commit(t *testing.T, msg string) string {
	t.Helper()
	mustGit(t, f.worktree, "commit", "--allow-empty", "-m", msg)
	return mustGit(t, f.worktree, "rev-parse", "HEAD")
}

// commitAndPush commits in the session worktree and pushes from it.
func (f *sessionPushFixture) commitAndPush(t *testing.T, msg string) string {
	t.Helper()
	sha := f.commit(t, msg)
	mustGit(t, f.worktree, "push", "origin", f.branch)
	return sha
}

// foreignPush pushes a commit on the session branch from an unrelated clone,
// as a human pushing from elsewhere would.
func (f *sessionPushFixture) foreignPush(t *testing.T) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	mustGit(t, t.TempDir(), "clone", "-b", f.branch, f.origin, other)
	mustGit(t, other, "config", "user.email", "human@test.com")
	mustGit(t, other, "config", "user.name", "Human")
	mustGit(t, other, "commit", "--allow-empty", "-m", "foreign")
	mustGit(t, other, "push", "origin", f.branch)
	return mustGit(t, other, "rev-parse", "HEAD")
}

func (f *sessionPushFixture) probe(t *testing.T, fromSHA, newSHA string) (bool, error) {
	t.Helper()
	return NewManager(zerolog.Nop()).SessionPushedHead(context.Background(), f.worktree, f.branch, fromSHA, newSHA)
}

func TestSessionPushedHead_PushOnReceiptedHead(t *testing.T) {
	f := newSessionPushFixture(t)
	b := f.commitAndPush(t, "B")
	ok, err := f.probe(t, f.receipted, b)
	if err != nil || !ok {
		t.Fatalf("SessionPushedHead(A, B) = %v, %v; want true for the session's own push", ok, err)
	}
}

func TestSessionPushedHead_PushChainCheckedAtTheTip(t *testing.T) {
	f := newSessionPushFixture(t)
	f.commitAndPush(t, "B")
	c := f.commitAndPush(t, "C")
	ok, err := f.probe(t, f.receipted, c)
	if err != nil || !ok {
		t.Fatalf("SessionPushedHead(A, C) = %v, %v; want true across an unbroken push chain", ok, err)
	}
}

func TestSessionPushedHead_HeadMismatch(t *testing.T) {
	f := newSessionPushFixture(t)
	b := f.commitAndPush(t, "B")
	f.commit(t, "D (local, unpushed)")
	ok, err := f.probe(t, f.receipted, b)
	if err != nil || ok {
		t.Fatalf("SessionPushedHead with HEAD != newSHA = %v, %v; want false, nil", ok, err)
	}
}

func TestSessionPushedHead_ForcePushRewroteReceiptedHead(t *testing.T) {
	f := newSessionPushFixture(t)
	mustGit(t, f.worktree, "reset", "--hard", f.receipted+"^")
	rewritten := f.commit(t, "A rewritten")
	mustGit(t, f.worktree, "push", "--force", "origin", f.branch)
	ok, err := f.probe(t, f.receipted, rewritten)
	if err != nil || ok {
		t.Fatalf("SessionPushedHead after a force-push = %v, %v; want false, nil", ok, err)
	}
}

func TestSessionPushedHead_FetchedForeignPushBreaksTheChain(t *testing.T) {
	f := newSessionPushFixture(t)
	foreign := f.foreignPush(t)
	mustGit(t, f.worktree, "fetch", "origin")
	mustGit(t, f.worktree, "merge", "--ff-only", "origin/"+f.branch)
	if got := mustGit(t, f.worktree, "rev-parse", "HEAD"); got != foreign {
		t.Fatalf("worktree HEAD = %s after fast-forward, want the foreign head %s", got, foreign)
	}
	s := f.commitAndPush(t, "S on top of the foreign push")

	// The two conditions HEAD equality and descent alone would accept: this
	// pins that only the reflog chain refuses it.
	if got := mustGit(t, f.worktree, "rev-parse", "HEAD"); got != s {
		t.Fatalf("worktree HEAD = %s, want %s", got, s)
	}
	mustGit(t, f.worktree, "merge-base", "--is-ancestor", f.receipted, s)

	ok, err := f.probe(t, f.receipted, s)
	if err != nil || ok {
		t.Fatalf("SessionPushedHead over a fetched foreign push = %v, %v; want false, nil", ok, err)
	}
}

func TestSessionPushedHead_PeerWorktreePushFastForwardedIsNotOwn(t *testing.T) {
	f := newSessionPushFixture(t)
	peer := filepath.Join(t.TempDir(), "peer")
	mustGit(t, f.clone, "worktree", "add", "-b", "peer", peer, f.receipted)
	mustGit(t, peer, "commit", "--allow-empty", "-m", "B from a peer worktree")
	b := mustGit(t, peer, "rev-parse", "HEAD")
	mustGit(t, peer, "push", "origin", "peer:"+f.branch)
	mustGit(t, f.worktree, "merge", "--ff-only", "origin/"+f.branch)

	// HEAD, descent and the shared push-only reflog chain all accept it; only
	// the worktree's own HEAD reflog shows B arrived by fast-forward.
	if got := mustGit(t, f.worktree, "rev-parse", "HEAD"); got != b {
		t.Fatalf("worktree HEAD = %s, want the peer head %s", got, b)
	}
	reflog := mustGit(t, f.worktree, "reflog", "show", "--format=%H%x00%gs", "refs/remotes/origin/"+f.branch)
	if !reflogIsPushChain(reflog, f.receipted, b) {
		t.Fatalf("shared origin/%s reflog is not a push chain; fixture does not exercise the peer case:\n%s", f.branch, reflog)
	}
	ok, err := f.probe(t, f.receipted, b)
	if err != nil || ok {
		t.Fatalf("SessionPushedHead over a peer worktree's push = %v, %v; want false, nil", ok, err)
	}
}

func TestHeadReflogAuthoredLocally(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reflog []string
		want   bool
	}{
		{"commit", []string{"b\x00commit: B", "a\x00commit: A"}, true},
		{"amend", []string{"b\x00commit (amend): B", "a\x00commit: A"}, true},
		{"stash after commit", []string{"b\x00reset: moving to HEAD", "b\x00commit: B"}, true},
		{"rebase pick", []string{"b\x00rebase (finish): returning to refs/heads/f", "b\x00rebase (pick): B", "m\x00rebase (start): checkout main"}, true},
		{"fast-forward", []string{"b\x00merge origin/f: Fast-forward", "a\x00commit: A"}, false},
		{"pull", []string{"b\x00pull: Fast-forward"}, false},
		{"reset", []string{"b\x00reset: moving to origin/f"}, false},
		{"ff rebase", []string{"b\x00rebase (finish): returning to refs/heads/f", "b\x00rebase (start): checkout origin/f"}, false},
		{"head elsewhere", []string{"c\x00commit: C", "b\x00commit: B"}, false},
		{"empty", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := headReflogAuthoredLocally(strings.Join(tc.reflog, "\n"), "b"); got != tc.want {
				t.Fatalf("headReflogAuthoredLocally = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSessionPushedHead_FromSHAAbsentFromReflog(t *testing.T) {
	f := newSessionPushFixture(t)
	b := f.commitAndPush(t, "B")
	// main's initial commit is an ancestor of B but origin/feature never
	// pointed at it, so the reflog walk runs out before reaching it.
	base := mustGit(t, f.clone, "rev-parse", "main")
	ok, err := f.probe(t, base, b)
	if err != nil || ok {
		t.Fatalf("SessionPushedHead from a SHA absent from the reflog = %v, %v; want false, nil", ok, err)
	}
}

func TestSessionPushedHead_MissingWorktreeNeverTrue(t *testing.T) {
	f := newSessionPushFixture(t)
	b := f.commitAndPush(t, "B")
	mgr := NewManager(zerolog.Nop())
	missing := filepath.Join(t.TempDir(), "gone")
	if ok, _ := mgr.SessionPushedHead(context.Background(), missing, f.branch, f.receipted, b); ok {
		t.Fatal("SessionPushedHead on a missing worktree = true, want false or an error")
	}
	if ok, _ := mgr.SessionPushedHead(context.Background(), "", f.branch, f.receipted, b); ok {
		t.Fatal("SessionPushedHead with an empty worktree path = true, want false or an error")
	}
}

func TestSameTree(t *testing.T) {
	f := newSessionPushFixture(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	empty := f.commit(t, "empty, no content change")
	if same, err := mgr.SameTree(ctx, f.worktree, f.receipted, empty); err != nil || !same {
		t.Fatalf("SameTree(A, empty commit) = %v, %v; want true, nil", same, err)
	}

	if err := os.WriteFile(filepath.Join(f.worktree, "changed.txt"), []byte("new content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, f.worktree, "add", "changed.txt")
	changed := f.commit(t, "content change")
	if same, err := mgr.SameTree(ctx, f.worktree, f.receipted, changed); err != nil || same {
		t.Fatalf("SameTree(A, content change) = %v, %v; want false, nil", same, err)
	}

	if same, err := mgr.SameTree(ctx, f.worktree, f.receipted, strings.Repeat("0", 40)); err == nil || same {
		t.Fatalf("SameTree with a missing object = %v, %v; want false and an error", same, err)
	}
	if same, err := mgr.SameTree(ctx, "", f.receipted, empty); err == nil || same {
		t.Fatalf("SameTree with an empty worktree path = %v, %v; want false and an error", same, err)
	}
	// rev-parse would reject "--all^{tree}" on its own, so assert the guard's
	// own error: the option-shaped commit must never reach git, on either side.
	for _, pair := range [][2]string{{"--all", empty}, {empty, "--all"}} {
		same, err := mgr.SameTree(ctx, f.worktree, pair[0], pair[1])
		if err == nil || same || !strings.Contains(err.Error(), "invalid commit") {
			t.Fatalf("SameTree(%q, %q) = %v, %v; want false and the invalid-commit guard error", pair[0], pair[1], same, err)
		}
	}
}

func TestReflogIsPushChain(t *testing.T) {
	entry := func(sha, subject string) string { return sha + "\x00" + subject }
	for _, tc := range []struct {
		name, newSHA string
		reflog       []string
		want         bool
	}{
		{"push on from", "b", []string{entry("b", "update by push"), entry("a", "fetch: storing head")}, true},
		{"chain", "c", []string{entry("c", "update by push"), entry("b", "update by push"), entry("a", "update by push")}, true},
		{"tip is not new", "b", []string{entry("x", "update by push"), entry("a", "update by push")}, false},
		{"fetch in the chain", "b", []string{entry("b", "update by push"), entry("h", "fetch: fast-forward"), entry("a", "update by push")}, false},
		{"tip was a pull", "b", []string{entry("b", "pull: fast-forward"), entry("a", "update by push")}, false},
		{"from absent", "b", []string{entry("b", "update by push"), entry("z", "update by push")}, false},
		{"empty", "b", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reflogIsPushChain(strings.Join(tc.reflog, "\n"), "a", tc.newSHA); got != tc.want {
				t.Fatalf("reflogIsPushChain = %v, want %v", got, tc.want)
			}
		})
	}
}
