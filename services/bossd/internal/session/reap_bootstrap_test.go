package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"
)

// reapWorktreeRecorder records the cleanup calls the stranded-bootstrap reaper
// makes. It wraps the shared mockWorktreeManager rather than extending it so the
// recording slices stay local to these tests.
type reapWorktreeRecorder struct {
	*mockWorktreeManager
	mu     sync.Mutex
	purged []string
	reaped []string

	// purgeErr stands in for a purge that never ran because the shared clone was
	// busy — the one thing the real PurgeWorktree reports back.
	purgeErr error

	// safeByRef, when non-nil, answers BranchSafeToDelete PER BASE REF, and
	// errors ("unknown revision") for any ref not listed. The embedded mock
	// answers one hardcoded bool for every ref, which cannot express the case
	// that matters here — a branch created from origin/<base> is not an ancestor
	// of a STALE LOCAL base — so a single-bool fake would let that premise error
	// pass unnoticed, as it did for three review rounds.
	safeByRef   map[string]bool
	queriedRefs []string
}

func (r *reapWorktreeRecorder) BranchSafeToDelete(_ context.Context, _, _, baseRef string) (bool, error) {
	r.mu.Lock()
	r.queriedRefs = append(r.queriedRefs, baseRef)
	byRef := r.safeByRef
	fallback, fallbackErr := r.branchSafeToDelete, r.branchSafeToDeleteErr
	r.mu.Unlock()

	if byRef == nil {
		return fallback, fallbackErr
	}
	safe, known := byRef[baseRef]
	if !known {
		return false, fmt.Errorf("unknown revision %q", baseRef)
	}
	return safe, nil
}

func (r *reapWorktreeRecorder) refsQueried() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queriedRefs...)
}

func (r *reapWorktreeRecorder) PurgeWorktree(_ context.Context, _, _, _, branch string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purged = append(r.purged, branch)
	return r.purgeErr
}

func (r *reapWorktreeRecorder) ReapLocalBranches(_ context.Context, _ string, branches []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reaped = append(r.reaped, branches...)
	return nil
}

func (r *reapWorktreeRecorder) records() (purged, reaped []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.purged...), append([]string(nil), r.reaped...)
}

func newBootstrapReapLifecycle(t *testing.T) (*Lifecycle, *mockSessionStore, *reapWorktreeRecorder) {
	t.Helper()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		DisplayName:       "repo",
		LocalPath:         "/tmp/repo",
		WorktreeBaseDir:   "/tmp/worktrees",
		DefaultBaseBranch: "main",
	}
	// A branch a bootstrap created sits at origin/<base>, so the real ancestry
	// check says "safe". Tests that need the other answer override it.
	worktrees := &reapWorktreeRecorder{mockWorktreeManager: &mockWorktreeManager{branchSafeToDelete: true}}
	lc := newTestLifecycle(sessions, repos, nil, nil, worktrees, nil, nil, nil, zerolog.Nop())
	return lc, sessions, worktrees
}

// bootstrapStrand builds a row that got PAST `git worktree add` — the shape
// recordCreatedWorktree leaves behind — because that is what makes the branch
// this session's to reap. A row without WorktreePath is a create that died
// before the add, and strandedBranchIsOurs deliberately refuses to touch its
// branch; TestReapStrandedBootstrap_KeepsABranchWithNoRecordedWorktree covers
// that shape explicitly.
func bootstrapStrand(id string, state machine.State, createdAt time.Time) *models.Session {
	return &models.Session{
		ID:           id,
		RepoID:       "repo-1",
		State:        state,
		BranchName:   "branch-" + id,
		WorktreePath: "/tmp/worktrees/repo/branch-" + id,
		AgentName:    "claude",
		CreatedAt:    createdAt,
	}
}

// TestReapStrandedBootstrap_OldPreAgentSessionIsReclaimed is the AC-5 proof:
// a session stranded in an early bootstrap state past the bootstrap deadline is
// reclaimed automatically — marked failed, with its worktree and branch removed.
func TestReapStrandedBootstrap_OldPreAgentSessionIsReclaimed(t *testing.T) {
	lc, sessions, worktrees := newBootstrapReapLifecycle(t)
	old := time.Now().Add(-lc.bootstrapReapThreshold() - time.Minute)
	sessions.sessions["s1"] = bootstrapStrand("s1", machine.CreatingWorktree, old)

	n, err := lc.ReapStrandedBootstrapSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped=%d, want 1", n)
	}
	got := sessions.sessions["s1"]
	if got.State != machine.Blocked {
		t.Fatalf("state = %v, want Blocked", got.State)
	}
	if got.BlockedReason == nil || *got.BlockedReason == "" {
		t.Fatal("BlockedReason not persisted; the failure would be invisible")
	}
	purged, reaped := worktrees.records()
	if len(purged) != 1 || purged[0] != "branch-s1" {
		t.Fatalf("purged = %v, want [branch-s1]", purged)
	}
	if len(reaped) != 1 || reaped[0] != "branch-s1" {
		t.Fatalf("reaped branches = %v, want [branch-s1]", reaped)
	}
}

// TestReapStrandedBootstrap_PurgeThatNeverRanSuppressesTheReap covers the
// reaper's half of the purge-before-reap ordering.
//
// PurgeWorktree declines to run when it cannot serialize against a busy clone —
// and this reaper is the caller most likely to meet that, since it runs from the
// daemon poller concurrently with live creates by design. A skipped purge leaves
// the worktree REGISTERED, and `git branch -D` refuses a branch checked out in a
// registered worktree, so reaping anyway leaks the branch behind a generic
// delete failure.
//
// There is no next tick for this row, which is the second half of the contract.
// The reap marks the session Blocked BEFORE cleaning up (that transition is the
// atomic claim that keeps a late-returning bootstrap from being cleaned up
// underneath itself), and the sweep lists only pre-agent states — so the
// artifacts are abandoned, not deferred, and the log has to say so and name
// them. The session is still reclaimed either way: marking it Blocked is what
// makes the failure visible, independent of whether the disk artifacts came
// away.
func TestReapStrandedBootstrap_PurgeThatNeverRanSuppressesTheReap(t *testing.T) {
	var logs bytes.Buffer
	lc, sessions, worktrees := newBootstrapReapLifecycle(t)
	lc.logger = zerolog.New(&logs)
	worktrees.purgeErr = errors.New("acquire repo-clone lock \"/tmp/repo\": context deadline exceeded")
	old := time.Now().Add(-lc.bootstrapReapThreshold() - time.Minute)
	sessions.sessions["s1"] = bootstrapStrand("s1", machine.CreatingWorktree, old)

	n, err := lc.ReapStrandedBootstrapSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped=%d, want 1: the row is still reclaimed even when its artifacts are abandoned", n)
	}
	if got := sessions.sessions["s1"].State; got != machine.Blocked {
		t.Fatalf("state = %v, want Blocked", got)
	}
	// The default fake answers "safe to delete", so without the suppression this
	// row's branch would be reaped.
	purged, reaped := worktrees.records()
	if len(purged) != 1 {
		t.Fatalf("purged = %v, want the purge to have been attempted once", purged)
	}
	if len(reaped) != 0 {
		t.Fatalf("reaped = %v, want none: the worktree is still registered, so `git branch -D` would be refused and the branch would leak", reaped)
	}
	// The row is Blocked now, so it has left the state set this sweep lists on:
	// no later tick revisits it. Saying "deferred" here would tell an operator to
	// wait for a retry that never comes.
	if !strings.Contains(logs.String(), "abandoning this session's worktree and branch") {
		t.Fatalf("a suppressed reap did not report its artifacts as abandoned; log:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "deferring") {
		t.Fatalf("the log promises a later sweep, but a Blocked row is never listed again; log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `"level":"error"`) {
		t.Fatalf("abandoned artifacts were not logged at error level; log:\n%s", logs.String())
	}
}

// TestReapStrandedBootstrap_YoungSessionIsNotReclaimed is the explicit BOS-426
// regression guard. A running daemon owns CreatingWorktree/StartingAgent through
// the live create path; reaping a mid-creation row deleted it out from under an
// in-flight start and failed it with `update worktree path: sql: no rows in
// result set`. The age gate — derived from the bootstrap deadline, past which a
// live create would already have failed itself — is what makes the periodic
// sweep safe, so it has to actually hold.
func TestReapStrandedBootstrap_YoungSessionIsNotReclaimed(t *testing.T) {
	lc, sessions, worktrees := newBootstrapReapLifecycle(t)
	sessions.sessions["s1"] = bootstrapStrand("s1", machine.CreatingWorktree, time.Now())

	n, err := lc.ReapStrandedBootstrapSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 0 {
		t.Fatalf("reaped=%d, want 0 (a live create must never be touched)", n)
	}
	if got := sessions.sessions["s1"].State; got != machine.CreatingWorktree {
		t.Fatalf("state = %v, want CreatingWorktree (untouched)", got)
	}
	if purged, reaped := worktrees.records(); len(purged) != 0 || len(reaped) != 0 {
		t.Fatalf("cleaned up a live create: purged=%v reaped=%v", purged, reaped)
	}
}

// TestReapStrandedBootstrap_ThresholdTracksTheBootstrapDeadline pins the
// derivation: shortening the bootstrap deadline shortens the reap threshold, so
// the two can never disagree about when a bootstrap is dead.
func TestReapStrandedBootstrap_ThresholdTracksTheBootstrapDeadline(t *testing.T) {
	lc, _, _ := newBootstrapReapLifecycle(t)
	if got, want := lc.bootstrapReapThreshold(), BootstrapTimeout+bootstrapReapMargin; got != want {
		t.Fatalf("default threshold = %s, want %s", got, want)
	}
	lc.SetBootstrapTimeout(time.Second)
	if got, want := lc.bootstrapReapThreshold(), time.Second+bootstrapReapMargin; got != want {
		t.Fatalf("overridden threshold = %s, want %s", got, want)
	}
}

// TestReapStrandedBootstrap_StartupReclaimsRowsPredatingTheProcess covers the
// restart case: a strand can be seconds old and still dead, because the daemon
// that was creating it is gone. Only rows that predate this process qualify.
func TestReapStrandedBootstrap_StartupReclaimsRowsPredatingTheProcess(t *testing.T) {
	lc, sessions, _ := newBootstrapReapLifecycle(t)
	sessions.sessions["old"] = bootstrapStrand("old", machine.StartingAgent, lc.startedAt.Add(-time.Second))
	sessions.sessions["new"] = bootstrapStrand("new", machine.StartingAgent, lc.startedAt.Add(time.Second))

	n, err := lc.ReapStrandedBootstrapSessionsAtStartup(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped=%d, want 1", n)
	}
	if got := sessions.sessions["old"].State; got != machine.Blocked {
		t.Fatalf("pre-restart strand state = %v, want Blocked", got)
	}
	if got := sessions.sessions["new"].State; got != machine.StartingAgent {
		t.Fatalf("post-restart create state = %v, want StartingAgent (untouched)", got)
	}
}

// TestReapStrandedBootstrap_SkipsRowsThatReachedAnAgent keeps the boundary with
// the stranded-cron sweep: once a row has an agent id or a tmux pane it is past
// bootstrap, and its recovery belongs to the sweep that understands liveness.
func TestReapStrandedBootstrap_SkipsRowsThatReachedAnAgent(t *testing.T) {
	lc, sessions, _ := newBootstrapReapLifecycle(t)
	old := time.Now().Add(-lc.bootstrapReapThreshold() - time.Minute)

	withAgent := bootstrapStrand("agent", machine.StartingAgent, old)
	withAgent.AgentSessionID = ptr("a1")
	sessions.sessions["agent"] = withAgent

	withPane := bootstrapStrand("pane", machine.StartingAgent, old)
	withPane.TmuxSessionName = ptr("boss-pane")
	sessions.sessions["pane"] = withPane

	archived := bootstrapStrand("archived", machine.CreatingWorktree, old)
	archivedAt := time.Now().Add(-time.Hour)
	archived.ArchivedAt = &archivedAt
	sessions.sessions["archived"] = archived

	n, err := lc.ReapStrandedBootstrapSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 0 {
		t.Fatalf("reaped=%d, want 0", n)
	}
}

// TestReapStrandedBootstrap_PRSessionKeepsItsBranch guards the one branch we
// must never delete: a PR head branch belongs to the PR, not to the session.
func TestReapStrandedBootstrap_PRSessionKeepsItsBranch(t *testing.T) {
	lc, sessions, worktrees := newBootstrapReapLifecycle(t)
	old := time.Now().Add(-lc.bootstrapReapThreshold() - time.Minute)
	strand := bootstrapStrand("pr", machine.CreatingWorktree, old)
	pr := 42
	strand.PRNumber = &pr
	sessions.sessions["pr"] = strand

	if _, err := lc.ReapStrandedBootstrapSessionsPeriodic(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	purged, reaped := worktrees.records()
	if len(purged) != 1 {
		t.Fatalf("purged = %v, want the worktree removed", purged)
	}
	if len(reaped) != 0 {
		t.Fatalf("reaped branches = %v, want none (a PR head branch is not ours)", reaped)
	}
}

// TestReapStrandedBootstrap_LeavesATargetSomeoneElseHolds is the proof that the
// reaper serializes with the create/cleanup/retry lifecycle rather than trusting
// elapsed age.
//
// The row here is old enough to reap by every age test the sweep applies — and
// must still be left alone, because a create for the same target holds the
// target lock. That is the shape a bootstrap whose failure cleanup overran
// bootstrapReapMargin leaves behind: the margin budgets for the cleanup's WAITS,
// not for its git, and a single purge command is capped at
// gitpkg.GitCommandTimeout (five minutes) on its own. Reaping under that holder
// is what let a delayed cleanup purge the worktree a RETRY had since recreated.
func TestReapStrandedBootstrap_LeavesATargetSomeoneElseHolds(t *testing.T) {
	lc, sessions, worktrees := newBootstrapReapLifecycle(t)
	// Keep the declined acquisition from costing the suite the real timeout.
	prev := TargetReapLockTimeout
	TargetReapLockTimeout = 10 * time.Millisecond
	t.Cleanup(func() { TargetReapLockTimeout = prev })

	old := time.Now().Add(-lc.bootstrapReapThreshold() - time.Minute)
	sessions.sessions["s1"] = bootstrapStrand("s1", machine.CreatingWorktree, old)

	// Stand in for the create that is still cleaning this target up. Keyed
	// exactly as the create path keys it: repo + branch, no PR.
	release, _, err := AcquireTargetStart(context.Background(), "repo-1", "branch-s1", nil)
	if err != nil {
		t.Fatalf("acquire target lock: %v", err)
	}

	n, err := lc.ReapStrandedBootstrapSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 0 {
		t.Fatalf("reaped=%d, want 0: the target is still owned by a live create", n)
	}
	if got := sessions.sessions["s1"]; got.State != machine.CreatingWorktree {
		t.Fatalf("state = %v, want CreatingWorktree left untouched", got.State)
	}
	purged, reaped := worktrees.records()
	if len(purged) != 0 || len(reaped) != 0 {
		t.Fatalf("purged=%v reaped=%v, want neither: cleaning up under the holder is the race this guards", purged, reaped)
	}

	// Once the holder is done, the same sweep reclaims the row — proving the
	// skip above is a deferral and not a permanent refusal.
	release()
	n, err = lc.ReapStrandedBootstrapSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("reap after release: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped=%d after the holder released, want 1", n)
	}
}

// TestReapStrandedBootstrap_KeepsABranchWithNoRecordedWorktree guards the branch
// an ancestry check alone would have deleted.
//
// The daemon dying between the session INSERT and `git worktree add` leaves a row
// naming an explicit branch with no worktree path. When that name belongs to a
// PRE-EXISTING branch of the user's that is fully merged into the base, ancestry
// says "safe to delete" — correctly, in the only sense it can: no unique commits
// are lost. It is still not this bootstrap's branch. The recorded worktree path
// is the only evidence that separates the two, so its absence has to stop the
// reap even when every other test here would pass.
//
// safeByRef is pinned to the answer that makes the wrong outcome available: if
// the ownership gate is ever removed, ancestry returns true for both refs and
// this branch is force-deleted.
func TestReapStrandedBootstrap_KeepsABranchWithNoRecordedWorktree(t *testing.T) {
	lc, sessions, worktrees := newBootstrapReapLifecycle(t)
	worktrees.safeByRef = map[string]bool{"main": true, "refs/remotes/origin/main": true}
	old := time.Now().Add(-lc.bootstrapReapThreshold() - time.Minute)
	strand := bootstrapStrand("preexisting", machine.CreatingWorktree, old)
	// The shape a daemon death before `git worktree add` leaves: a branch name
	// taken from the request, and no proof the add ever ran.
	strand.WorktreePath = ""
	sessions.sessions["preexisting"] = strand

	n, err := lc.ReapStrandedBootstrapSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	// The session is still reclaimed — only the branch delete is refused.
	if n != 1 {
		t.Fatalf("reaped=%d, want 1 (the row is still reclaimed)", n)
	}
	if got := sessions.sessions["preexisting"]; got.State != machine.Blocked {
		t.Fatalf("state = %v, want Blocked", got.State)
	}
	_, reaped := worktrees.records()
	if len(reaped) != 0 {
		t.Fatalf("reaped branches = %v, want none: with no recorded worktree this branch was never ours to delete", reaped)
	}
}

// TestReapStrandedBootstrap_AbandonedArtifactsNeverInviteAnUnownedBranchDelete
// is the intersection of the two guards above, and the one an operator acts on.
//
// When the purge is refused the reaper reports what it abandoned, at Error, so
// the artifacts can be reclaimed by hand — but `ours` was already decided, and
// for a PR head branch it is false. A message that told an operator to reclaim
// "the worktree and branch" would talk them into running the exact
// `git branch -D` that strandedBranchIsOurs exists to refuse, by hand, on a
// branch the reaper itself declined to touch.
func TestReapStrandedBootstrap_AbandonedArtifactsNeverInviteAnUnownedBranchDelete(t *testing.T) {
	var logs bytes.Buffer
	lc, sessions, worktrees := newBootstrapReapLifecycle(t)
	lc.logger = zerolog.New(&logs)
	worktrees.purgeErr = errors.New("acquire repo-clone lock \"/tmp/repo\": context deadline exceeded")
	old := time.Now().Add(-lc.bootstrapReapThreshold() - time.Minute)
	strand := bootstrapStrand("pr", machine.CreatingWorktree, old)
	pr := 42
	strand.PRNumber = &pr
	sessions.sessions["pr"] = strand

	if _, err := lc.ReapStrandedBootstrapSessionsPeriodic(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	got := logs.String()

	if !strings.Contains(got, `"level":"error"`) {
		t.Fatalf("abandoned artifacts were not logged at error level; log:\n%s", got)
	}
	if strings.Contains(got, "abandoning this session's worktree and branch") {
		t.Fatalf("the log invites reclaiming a PR head branch by hand; log:\n%s", got)
	}
	if !strings.Contains(got, "The branch is left alone deliberately") {
		t.Fatalf("an unowned branch was not reported as deliberately left alone; log:\n%s", got)
	}
	if _, reaped := worktrees.records(); len(reaped) != 0 {
		t.Fatalf("reaped branches = %v, want none", reaped)
	}
}

// TestReapStrandedBootstrap_KeepsABranchCarryingWork is the data-loss guard.
//
// ReapLocalBranches is `git branch -D`, which does not ask. A row's branch_name
// is written at INSERT time from the request, BEFORE Create has checked whether
// that branch already exists — so a create naming a PRE-EXISTING branch, killed
// by a daemon restart before Create could refuse it with ErrBranchExists, leaves
// a row pointing at a branch this session never made. The server's failure
// cleanup has durable evidence for this — a recorded worktree path proves
// `git worktree add` ran — but the reaper is looking at a row that may never
// have got that far, so it asks git whether the branch is an ancestor of the
// base. A branch carrying unmerged work is not, and must survive.
func TestReapStrandedBootstrap_KeepsABranchCarryingWork(t *testing.T) {
	lc, sessions, worktrees := newBootstrapReapLifecycle(t)
	worktrees.branchSafeToDelete = false
	old := time.Now().Add(-lc.bootstrapReapThreshold() - time.Minute)
	sessions.sessions["s1"] = bootstrapStrand("s1", machine.CreatingWorktree, old)

	n, err := lc.ReapStrandedBootstrapSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped=%d, want 1 (the session is still reclaimed)", n)
	}
	purged, reaped := worktrees.records()
	if len(reaped) != 0 {
		t.Fatalf("reaped branches = %v, want none — a branch with unmerged work was force-deleted", reaped)
	}
	// The worktree is still ours to remove either way.
	if len(purged) != 1 {
		t.Fatalf("purged = %v, want the worktree removed", purged)
	}
}

// TestReapStrandedBootstrap_ReapsABranchAheadOfAStaleLocalBase is the guard on
// the ancestry check's premise.
//
// Create branches from origin/<base>, and FetchBase writes ONLY the
// remote-tracking ref — the local base is fast-forwarded separately, by
// SyncBaseBranch, at unrelated moments. So on any active repo a freshly created
// session branch is AHEAD of refs/heads/<base> while sitting exactly at
// refs/remotes/origin/<base>. Asking only about the local base answers "not an
// ancestor" for precisely the branches this reaper exists to clean up, and logs
// "carries work not on the base" about a branch with no commits of its own.
func TestReapStrandedBootstrap_ReapsABranchAheadOfAStaleLocalBase(t *testing.T) {
	lc, sessions, worktrees := newBootstrapReapLifecycle(t)
	worktrees.safeByRef = map[string]bool{
		"main":                     false, // stale local base
		"refs/remotes/origin/main": true,  // what Create actually branched from
	}
	old := time.Now().Add(-lc.bootstrapReapThreshold() - time.Minute)
	sessions.sessions["s1"] = bootstrapStrand("s1", machine.CreatingWorktree, old)

	if _, err := lc.ReapStrandedBootstrapSessionsPeriodic(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if _, reaped := worktrees.records(); len(reaped) != 1 || reaped[0] != "branch-s1" {
		t.Fatalf("reaped branches = %v, want [branch-s1] — a branch at origin/main was treated as carrying work", reaped)
	}
	if refs := worktrees.refsQueried(); !slices.Contains(refs, "refs/remotes/origin/main") {
		t.Fatalf("base refs queried = %v, want the remote-tracking ref among them", refs)
	}
}

// TestReapStrandedBootstrap_KeepsTheBranchWhenSafetyIsUnknown pins the fail-safe
// direction: an ancestry check that errors on every base ref leaves the branch
// alone. An orphaned branch is recoverable; a deleted one is not.
func TestReapStrandedBootstrap_KeepsTheBranchWhenSafetyIsUnknown(t *testing.T) {
	lc, sessions, worktrees := newBootstrapReapLifecycle(t)
	worktrees.branchSafeToDelete = true
	worktrees.branchSafeToDeleteErr = errors.New("git exploded")
	old := time.Now().Add(-lc.bootstrapReapThreshold() - time.Minute)
	sessions.sessions["s1"] = bootstrapStrand("s1", machine.CreatingWorktree, old)

	if _, err := lc.ReapStrandedBootstrapSessionsPeriodic(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if _, reaped := worktrees.records(); len(reaped) != 0 {
		t.Fatalf("reaped branches = %v, want none when the safety check could not answer", reaped)
	}
}

// TestPreAgentReapStatesDerivedFromStrandedSet pins the derivation so the reap
// set cannot drift from the shared definition of a pre-agent transition.
func TestPreAgentReapStatesDerivedFromStrandedSet(t *testing.T) {
	got := preAgentReapStates()
	if len(got) != 2 {
		t.Fatalf("preAgentReapStates = %v, want exactly the two pre-agent states", got)
	}
	for _, s := range got {
		if !isPreAgentState(s) {
			t.Fatalf("preAgentReapStates contains non-pre-agent state %v", s)
		}
	}
	// Every pre-agent member of the shared stranded set must be present.
	for _, s := range strandedReapStates {
		if !isPreAgentState(s) {
			continue
		}
		found := false
		for _, g := range got {
			if g == s {
				found = true
			}
		}
		if !found {
			t.Fatalf("pre-agent state %v missing from preAgentReapStates", s)
		}
	}
}
