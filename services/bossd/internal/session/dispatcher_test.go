package session

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

func TestDispatcherChecksPassed(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	prNum := 42
	repos.repos["repo-1"] = &models.Repo{
		ID:           "repo-1",
		OriginURL:    "owner/repo",
		CanAutoMerge: true,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		State:    machine.AwaitingChecks,
		PRNumber: &prNum,
	}

	d := NewDispatcher(sessions, repos, vp, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ChecksPassed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	// Should transition to ReadyForReview (via GreenDraft → MarkReadyForReview → ReadyForReview).
	if sess.State != machine.ReadyForReview {
		t.Errorf("state = %v, want ReadyForReview", sess.State)
	}

	// Should have called MarkReadyForReview.
	if len(vp.markReadyCalls) != 1 || vp.markReadyCalls[0] != 42 {
		t.Errorf("markReadyCalls = %v, want [42]", vp.markReadyCalls)
	}
}

func TestDispatcherChecksFailed(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, logger)

	failure := vcs.CheckConclusionFailure
	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{
		SessionID: "sess-1",
		Event: vcs.ChecksFailed{
			PRID:         42,
			FailedChecks: []vcs.CheckResult{{Conclusion: &failure}},
		},
	}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.FixingChecks {
		t.Errorf("state = %v, want FixingChecks", sess.State)
	}
}

func TestDispatcherChecksFailedMaxAttempts(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.AwaitingChecks,
		AttemptCount: machine.MaxAttempts - 1, // One attempt away from max.
	}

	d := NewDispatcher(sessions, repos, vp, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ChecksFailed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Blocked {
		t.Errorf("state = %v, want Blocked", sess.State)
	}
	if sess.BlockedReason == nil || *sess.BlockedReason != sessionreason.FixLoopExhausted() {
		t.Errorf("BlockedReason = %v, want %q", sess.BlockedReason, sessionreason.FixLoopExhausted())
	}
}

func TestDispatcherConflictDetected(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ConflictDetected{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.FixingChecks {
		t.Errorf("state = %v, want FixingChecks", sess.State)
	}
}

// runFailLap drives one AwaitingChecks→FixingChecks→AwaitingChecks settle lap:
// dispatch ChecksFailed with headSHA, then return the persisted machine to
// AwaitingChecks via FixComplete (the repair plugin's clean-exit callback). If
// the lap Blocked, it stops there (FixComplete is invalid from Blocked).
func runFailLap(t *testing.T, d *Dispatcher, sessions *mockSessionStore, sessionID, headSHA string) {
	t.Helper()
	ctx := context.Background()
	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: sessionID, Event: vcs.ChecksFailed{PRID: 42, HeadSHA: headSHA}}
	close(ch)
	d.Run(ctx, ch)

	if sessions.sessions[sessionID].State == machine.Blocked {
		return
	}

	// FixComplete: FixingChecks → AwaitingChecks. The dispatcher has no direct
	// FixComplete VCS event (the repair plugin drives it via a host callback),
	// so transition the persisted state through the machine directly.
	sess := sessions.sessions[sessionID]
	sm := machine.NewWithContext(sess.State, &machine.SessionContext{
		AttemptCount: sess.AttemptCount,
		MaxAttempts:  machine.MaxAttempts,
	})
	if err := sm.FireCtx(ctx, machine.FixComplete); err != nil {
		t.Fatalf("fire fix_complete: %v", err)
	}
	newState := int(sm.State())
	if _, err := sessions.Update(ctx, sessionID, db.UpdateSessionParams{State: &newState}); err != nil {
		t.Fatalf("update after fix_complete: %v", err)
	}
}

// TestDispatcherSameSHASettleLoopDoesNotBlock is BOS-235 acceptance criterion 1:
// repeated AwaitingChecks↔FixingChecks laps at an UNCHANGED head SHA (CI merely
// re-settling, no fix pushed) must not drive attempt_count to MaxAttempts and
// must not Block the session.
func TestDispatcherSameSHASettleLoopDoesNotBlock(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}
	d := NewDispatcher(sessions, repos, vp, logger)

	// Run many more laps than MaxAttempts, all at the same head SHA.
	for i := 0; i < machine.MaxAttempts+5; i++ {
		runFailLap(t, d, sessions, "sess-1", "sha-settle")
	}

	sess := sessions.sessions["sess-1"]
	if sess.State == machine.Blocked {
		t.Fatalf("session Blocked on same-SHA settle loop; want not blocked (state=%v)", sess.State)
	}
	// The very first failure counts (one real attempt); subsequent same-SHA laps
	// are free, so attempt_count must stay well under MaxAttempts.
	if sess.AttemptCount >= machine.MaxAttempts {
		t.Fatalf("attempt_count = %d, want < %d (settle laps must be free)", sess.AttemptCount, machine.MaxAttempts)
	}
	if sess.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1 (only the first lap counts)", sess.AttemptCount)
	}
}

// TestDispatcherNewSHAEachLapStillBlocks is BOS-235 acceptance criterion 2: a
// genuine repeated-failure loop where each attempt pushes a NEW commit (head
// SHA changes each lap) still reaches Blocked+FixLoopExhausted after
// MaxAttempts real attempts. Preserves real exhaustion.
func TestDispatcherNewSHAEachLapStillBlocks(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}
	d := NewDispatcher(sessions, repos, vp, logger)

	for i := 0; i < machine.MaxAttempts; i++ {
		runFailLap(t, d, sessions, "sess-1", fmt.Sprintf("sha-%d", i))
		if sessions.sessions["sess-1"].State == machine.Blocked {
			break
		}
	}

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Blocked {
		t.Fatalf("state = %v, want Blocked after %d distinct-SHA attempts", sess.State, machine.MaxAttempts)
	}
	if sess.BlockedReason == nil || *sess.BlockedReason != sessionreason.FixLoopExhausted() {
		t.Fatalf("BlockedReason = %v, want %q", sess.BlockedReason, sessionreason.FixLoopExhausted())
	}
}

// runConflictLap drives one AwaitingChecks→FixingChecks→AwaitingChecks lap via
// ConflictDetected (the head-SHA-gated conflict path), mirroring runFailLap. If
// the lap Blocked, it stops there (FixComplete is invalid from Blocked).
func runConflictLap(t *testing.T, d *Dispatcher, sessions *mockSessionStore, sessionID, headSHA string) {
	t.Helper()
	ctx := context.Background()
	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: sessionID, Event: vcs.ConflictDetected{PRID: 42, HeadSHA: headSHA}}
	close(ch)
	d.Run(ctx, ch)

	if sessions.sessions[sessionID].State == machine.Blocked {
		return
	}

	sess := sessions.sessions[sessionID]
	sm := machine.NewWithContext(sess.State, &machine.SessionContext{
		AttemptCount: sess.AttemptCount,
		MaxAttempts:  machine.MaxAttempts,
	})
	if err := sm.FireCtx(ctx, machine.FixComplete); err != nil {
		t.Fatalf("fire fix_complete: %v", err)
	}
	newState := int(sm.State())
	if _, err := sessions.Update(ctx, sessionID, db.UpdateSessionParams{State: &newState}); err != nil {
		t.Fatalf("update after fix_complete: %v", err)
	}
}

// TestDispatcherSameSHAConflictLoopDoesNotBlock pins the head-SHA gating of the
// ConflictDetected path (BOS-235): a conflict re-observed on an UNCHANGED head
// SHA (base moved, repair pushed nothing) is a free settle lap and must not
// drive attempt_count to MaxAttempts or Block via the machine.
func TestDispatcherSameSHAConflictLoopDoesNotBlock(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}
	d := NewDispatcher(sessions, repos, vp, logger)

	for i := 0; i < machine.MaxAttempts+5; i++ {
		runConflictLap(t, d, sessions, "sess-1", "sha-conflict")
	}

	sess := sessions.sessions["sess-1"]
	if sess.State == machine.Blocked {
		t.Fatalf("session Blocked on same-SHA conflict loop; want not blocked (state=%v)", sess.State)
	}
	if sess.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1 (only the first conflict lap counts)", sess.AttemptCount)
	}
	if sess.LastAttemptHeadSHA == nil || *sess.LastAttemptHeadSHA != "sha-conflict" {
		t.Fatalf("LastAttemptHeadSHA = %v, want sha-conflict (recorded on the counted lap)", sess.LastAttemptHeadSHA)
	}
}

// TestDispatcherNewSHAConflictEachLapStillBlocks confirms the conflict path
// preserves genuine exhaustion: when each lap carries a NEW head SHA (a real fix
// pushed a commit that still conflicts) the session still reaches
// Blocked+FixLoopExhausted after MaxAttempts.
func TestDispatcherNewSHAConflictEachLapStillBlocks(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}
	d := NewDispatcher(sessions, repos, vp, logger)

	for i := 0; i < machine.MaxAttempts; i++ {
		runConflictLap(t, d, sessions, "sess-1", fmt.Sprintf("conflict-sha-%d", i))
		if sessions.sessions["sess-1"].State == machine.Blocked {
			break
		}
	}

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Blocked {
		t.Fatalf("state = %v, want Blocked after %d distinct-SHA conflict laps", sess.State, machine.MaxAttempts)
	}
	if sess.BlockedReason == nil || *sess.BlockedReason != sessionreason.FixLoopExhausted() {
		t.Fatalf("BlockedReason = %v, want %q", sess.BlockedReason, sessionreason.FixLoopExhausted())
	}
}

// TestDispatcherChecksPassedResetsAttemptCount is BOS-235 acceptance criterion
// 3: reaching green (ChecksPassed) resets attempt_count to 0 and clears the
// last-counted attempt SHA.
func TestDispatcherChecksPassedResetsAttemptCount(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	prNum := 42
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo", CanAutoMerge: false}
	priorSHA := "sha-prior"
	sessions.sessions["sess-1"] = &models.Session{
		ID:                 "sess-1",
		RepoID:             "repo-1",
		State:              machine.AwaitingChecks,
		PRNumber:           &prNum,
		AttemptCount:       3,
		LastAttemptHeadSHA: &priorSHA,
	}
	d := NewDispatcher(sessions, repos, vp, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ChecksPassed{PRID: 42}}
	close(ch)
	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.AttemptCount != 0 {
		t.Fatalf("attempt_count = %d, want 0 after green", sess.AttemptCount)
	}
	if sess.LastAttemptHeadSHA != nil {
		t.Fatalf("LastAttemptHeadSHA = %v, want nil (cleared on green)", *sess.LastAttemptHeadSHA)
	}
}

func TestDispatcherPRMerged(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRMerged{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if sessions.sessions["sess-1"].State != machine.Merged {
		t.Errorf("state = %v, want Merged", sessions.sessions["sess-1"].State)
	}
}

func TestDispatcherPRClosed(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRClosed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if sessions.sessions["sess-1"].State != machine.Closed {
		t.Errorf("state = %v, want Closed", sessions.sessions["sess-1"].State)
	}
}

// A Blocked session that receives a PR-merged webhook must clear the persisted
// block metadata (blocked_reason, attempt_count, last_attempt_head_sha) alongside
// advancing to Merged. Otherwise a stale non-gating "finalize failed" hint lingers
// on the merged row forever, since this handler seeds a terminal display status
// that makes the display poller skip the session (BOS-246).
func TestDispatcherPRMerged_ClearsBlockMetadataFromBlocked(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	reason := sessionreason.FinalizeFailure("finalize_failed", nil)
	sha := "sha-stale"
	sessions.sessions["sess-1"] = &models.Session{
		ID:                 "sess-1",
		RepoID:             "repo-1",
		State:              machine.Blocked,
		BlockedReason:      &reason,
		AttemptCount:       3,
		LastAttemptHeadSHA: &sha,
	}

	d := NewDispatcher(sessions, repos, vp, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRMerged{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Merged {
		t.Errorf("state = %v, want Merged", sess.State)
	}
	if sess.BlockedReason != nil {
		t.Errorf("BlockedReason = %q, want nil (cleared on terminal transition)", *sess.BlockedReason)
	}
	if sess.AttemptCount != 0 {
		t.Errorf("AttemptCount = %d, want 0 (cleared on terminal transition)", sess.AttemptCount)
	}
	if sess.LastAttemptHeadSHA != nil {
		t.Errorf("LastAttemptHeadSHA = %q, want nil (cleared on terminal transition)", *sess.LastAttemptHeadSHA)
	}
}

// The closed counterpart of the merged case above: a Blocked session receiving a
// PR-closed webhook must also clear the persisted block metadata when it advances
// to Closed.
func TestDispatcherPRClosed_ClearsBlockMetadataFromBlocked(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	reason := sessionreason.FinalizeFailure("finalize_failed", nil)
	sha := "sha-stale"
	sessions.sessions["sess-1"] = &models.Session{
		ID:                 "sess-1",
		RepoID:             "repo-1",
		State:              machine.Blocked,
		BlockedReason:      &reason,
		AttemptCount:       3,
		LastAttemptHeadSHA: &sha,
	}

	d := NewDispatcher(sessions, repos, vp, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRClosed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Closed {
		t.Errorf("state = %v, want Closed", sess.State)
	}
	if sess.BlockedReason != nil {
		t.Errorf("BlockedReason = %q, want nil (cleared on terminal transition)", *sess.BlockedReason)
	}
	if sess.AttemptCount != 0 {
		t.Errorf("AttemptCount = %d, want 0 (cleared on terminal transition)", sess.AttemptCount)
	}
	if sess.LastAttemptHeadSHA != nil {
		t.Errorf("LastAttemptHeadSHA = %q, want nil (cleared on terminal transition)", *sess.LastAttemptHeadSHA)
	}
}

// fakeArchiver records ArchiveSession calls on a buffered channel so the async
// (safego.Go) archive path can be observed without racing.
type fakeArchiver struct {
	calls chan string
}

func newFakeArchiver() *fakeArchiver {
	return &fakeArchiver{calls: make(chan string, 8)}
}

func (f *fakeArchiver) ArchiveSession(_ context.Context, id string) error {
	f.calls <- id
	return nil
}

// When a PR merges and the repo has ShouldArchiveSessionsAfterMerge on, the session is
// archived exactly once, asynchronously, after reaching Merged.
func TestDispatcherPRMerged_ArchivesWhenEnabled(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", ShouldArchiveSessionsAfterMerge: true}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	arch := newFakeArchiver()
	d := NewDispatcher(sessions, repos, vp, logger)
	d.SetArchiver(arch, nil)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRMerged{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if got := sessions.sessions["sess-1"].State; got != machine.Merged {
		t.Errorf("state = %v, want Merged", got)
	}
	select {
	case id := <-arch.calls:
		if id != "sess-1" {
			t.Errorf("archived %q, want sess-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session was not archived after merge")
	}
	// Exactly once: no further archive call should arrive.
	select {
	case id := <-arch.calls:
		t.Errorf("session archived more than once (extra: %q)", id)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestDispatcherPRMerged_ArchivesWhenAlreadyReconciledToMerged pins the
// interaction BOS-534 exposes. MergeSession's post-merge display refresh
// reconciles the session to Merged synchronously, before its RPC returns — so
// by the time GitHub's PR-merged webhook arrives, the row is already Merged.
// machine.Merged permits no outbound PRMerged, so the handler's FireCtx would
// fail and return early, silently skipping archive-after-merge (and the
// branch deletion chained off it) on every user-initiated merge. The handler
// must treat an already-Merged row as the transition having happened and still
// run the side effects.
func TestDispatcherPRMerged_ArchivesWhenAlreadyReconciledToMerged(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", ShouldArchiveSessionsAfterMerge: true}
	// Already reconciled to Merged by the display refresh MergeSession fired.
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.Merged,
	}

	arch := newFakeArchiver()
	d := NewDispatcher(sessions, repos, vp, logger)
	d.SetArchiver(arch, nil)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRMerged{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if got := sessions.sessions["sess-1"].State; got != machine.Merged {
		t.Errorf("state = %v, want Merged", got)
	}
	select {
	case id := <-arch.calls:
		if id != "sess-1" {
			t.Errorf("archived %q, want sess-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an already-Merged session was not archived when its merge webhook arrived")
	}
}

// With the flag off, a merged session is not archived.
func TestDispatcherPRMerged_DoesNotArchiveWhenDisabled(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", ShouldArchiveSessionsAfterMerge: false}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	arch := newFakeArchiver()
	d := NewDispatcher(sessions, repos, vp, logger)
	d.SetArchiver(arch, nil)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRMerged{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if got := sessions.sessions["sess-1"].State; got != machine.Merged {
		t.Errorf("state = %v, want Merged", got)
	}
	select {
	case id := <-arch.calls:
		t.Errorf("session %q archived despite flag off", id)
	case <-time.After(150 * time.Millisecond):
	}
}

// With no archiver wired, handling a merge neither panics nor errors.
func TestDispatcherPRMerged_NilArchiverIsSafe(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", ShouldArchiveSessionsAfterMerge: true}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, logger) // no SetArchiver

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRMerged{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if got := sessions.sessions["sess-1"].State; got != machine.Merged {
		t.Errorf("state = %v, want Merged", got)
	}
}

func TestDispatcherContextCancellation(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	d := NewDispatcher(sessions, repos, vp, logger)

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan SessionEvent)

	done := make(chan struct{})
	go func() {
		d.Run(ctx, ch)
		close(done)
	}()

	// Cancel context should stop the dispatcher.
	cancel()
	select {
	case <-done:
		// Expected.
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not stop on context cancellation")
	}
}

func TestDispatcherChecksPassedAutoMergeDisabled(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	prNum := 42
	repos.repos["repo-1"] = &models.Repo{
		ID:           "repo-1",
		OriginURL:    "owner/repo",
		CanAutoMerge: false,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		State:    machine.AwaitingChecks,
		PRNumber: &prNum,
	}

	d := NewDispatcher(sessions, repos, vp, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ChecksPassed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	// Should stop at GreenDraft — not transition to ReadyForReview.
	if sess.State != machine.GreenDraft {
		t.Errorf("state = %v, want GreenDraft", sess.State)
	}
	if len(vp.markReadyCalls) != 0 {
		t.Errorf("markReadyCalls = %v, want none", vp.markReadyCalls)
	}
}

// TestDispatcherReviewChangesRequested verifies a changes-requested review
// transitions the session to FixingChecks (repair is then the plugin's job).
func TestDispatcherReviewChangesRequested(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.ReadyForReview,
	}

	d := NewDispatcher(sessions, repos, vp, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{
		SessionID: "sess-1",
		Event:     vcs.ReviewSubmitted{PRID: 42, State: vcs.ReviewStateChangesRequested, Comments: []vcs.ReviewComment{{Body: "fix this"}}},
	}
	close(ch)

	d.Run(ctx, ch)

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.FixingChecks {
		t.Errorf("state = %v, want FixingChecks", sess.State)
	}
	if sess.LastObservedReviewState != int(vcs.ReviewStateChangesRequested) {
		t.Errorf("last observed review state = %v, want ChangesRequested", sess.LastObservedReviewState)
	}
}

// TestDispatcherReviewSubmittedNonActionableStates verifies that approved or
// commented reviews record the review state without transitioning to repair.
func TestDispatcherReviewSubmittedNonActionableStates(t *testing.T) {
	tests := []struct {
		name  string
		state vcs.ReviewState
	}{
		{name: "approved", state: vcs.ReviewStateApproved},
		{name: "commented", state: vcs.ReviewStateCommented},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			vp := newMockVCSProvider()
			logger := zerolog.Nop()

			sessions.sessions["sess-1"] = &models.Session{
				ID:     "sess-1",
				RepoID: "repo-1",
				State:  machine.ReadyForReview,
			}

			d := NewDispatcher(sessions, repos, vp, logger)

			ch := make(chan SessionEvent, 1)
			ch <- SessionEvent{
				SessionID: "sess-1",
				Event:     vcs.ReviewSubmitted{PRID: 42, State: tt.state, Comments: []vcs.ReviewComment{{Body: "looks good"}}},
			}
			close(ch)

			d.Run(ctx, ch)

			sess := sessions.sessions["sess-1"]
			if sess.State != machine.ReadyForReview {
				t.Errorf("state = %v, want ReadyForReview", sess.State)
			}
			if sess.LastObservedReviewState != int(tt.state) {
				t.Errorf("last observed review state = %v, want %v", sess.LastObservedReviewState, tt.state)
			}
		})
	}
}

// --- Mock SessionCompletionNotifier ---

type mockCompletionNotifier struct {
	calls []completionCall
}

type completionCall struct {
	sessionID string
	outcome   models.TaskMappingStatus
}

func (m *mockCompletionNotifier) HandleSessionCompleted(_ context.Context, sessionID string, outcome models.TaskMappingStatus) {
	m.calls = append(m.calls, completionCall{sessionID: sessionID, outcome: outcome})
}

// --- Completion Notifier Tests ---

func TestDispatcherPRMerged_NotifiesCompletion(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	notifier := &mockCompletionNotifier{}
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, logger)
	d.SetCompletionNotifier(notifier)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRMerged{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notifier call, got %d", len(notifier.calls))
	}
	if notifier.calls[0].sessionID != "sess-1" {
		t.Errorf("session = %q, want sess-1", notifier.calls[0].sessionID)
	}
	if notifier.calls[0].outcome != models.TaskMappingStatusCompleted {
		t.Errorf("outcome = %d, want Completed", notifier.calls[0].outcome)
	}
}

func TestDispatcherPRClosed_NotifiesCompletion(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	notifier := &mockCompletionNotifier{}
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	d := NewDispatcher(sessions, repos, vp, logger)
	d.SetCompletionNotifier(notifier)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRClosed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notifier call, got %d", len(notifier.calls))
	}
	if notifier.calls[0].outcome != models.TaskMappingStatusFailed {
		t.Errorf("outcome = %d, want Failed", notifier.calls[0].outcome)
	}
}

func TestDispatcherChecksFailedBlocked_NotifiesCompletion(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	notifier := &mockCompletionNotifier{}
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.AwaitingChecks,
		AttemptCount: machine.MaxAttempts - 1, // will transition to Blocked
	}

	d := NewDispatcher(sessions, repos, vp, logger)
	d.SetCompletionNotifier(notifier)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.ChecksFailed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if sessions.sessions["sess-1"].State != machine.Blocked {
		t.Fatalf("state = %v, want Blocked", sessions.sessions["sess-1"].State)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notifier call, got %d", len(notifier.calls))
	}
	if notifier.calls[0].outcome != models.TaskMappingStatusFailed {
		t.Errorf("outcome = %d, want Failed", notifier.calls[0].outcome)
	}
}

func TestDispatcherChecksFailedNotBlocked_DoesNotNotify(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	notifier := &mockCompletionNotifier{}
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		State:        machine.AwaitingChecks,
		AttemptCount: 0, // will transition to FixingChecks, not Blocked
	}

	d := NewDispatcher(sessions, repos, vp, logger)
	d.SetCompletionNotifier(notifier)

	failure := vcs.CheckConclusionFailure
	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{
		SessionID: "sess-1",
		Event:     vcs.ChecksFailed{PRID: 42, FailedChecks: []vcs.CheckResult{{Conclusion: &failure}}},
	}
	close(ch)

	d.Run(ctx, ch)

	if len(notifier.calls) != 0 {
		t.Errorf("expected 0 notifier calls for non-blocked state, got %d", len(notifier.calls))
	}
}

func TestDispatcherNilNotifier_DoesNotPanic(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	// No SetCompletionNotifier call — notifier is nil.
	d := NewDispatcher(sessions, repos, vp, logger)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRMerged{PRID: 42}}
	close(ch)

	// Should not panic.
	d.Run(ctx, ch)

	if sessions.sessions["sess-1"].State != machine.Merged {
		t.Errorf("state = %v, want Merged", sessions.sessions["sess-1"].State)
	}
}

// --- Display status setter ---

// fakeDisplayStatusSetter records Set calls so tests can assert the dispatcher
// pushes a terminal status into the tracker on merge/close.
type fakeDisplayStatusSetter struct {
	calls []struct {
		sessionID string
		info      vcs.DisplayInfo
	}
}

func (f *fakeDisplayStatusSetter) Set(sessionID string, info vcs.DisplayInfo) {
	f.calls = append(f.calls, struct {
		sessionID string
		info      vcs.DisplayInfo
	}{sessionID, info})
}

func TestDispatcherPRMerged_SetsDisplayStatusMerged(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	setter := &fakeDisplayStatusSetter{}
	d := NewDispatcher(sessions, repos, vp, logger)
	d.SetDisplayStatusSetter(setter)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRMerged{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if sessions.sessions["sess-1"].State != machine.Merged {
		t.Errorf("state = %v, want Merged", sessions.sessions["sess-1"].State)
	}
	if len(setter.calls) != 1 {
		t.Fatalf("display setter calls = %d, want 1", len(setter.calls))
	}
	if got := setter.calls[0]; got.sessionID != "sess-1" || got.info.Status != vcs.DisplayStatusMerged {
		t.Errorf("Set(%q, %v), want (sess-1, Merged)", got.sessionID, got.info.Status)
	}
}

func TestDispatcherPRClosed_SetsDisplayStatusClosed(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.AwaitingChecks,
	}

	setter := &fakeDisplayStatusSetter{}
	d := NewDispatcher(sessions, repos, vp, logger)
	d.SetDisplayStatusSetter(setter)

	ch := make(chan SessionEvent, 1)
	ch <- SessionEvent{SessionID: "sess-1", Event: vcs.PRClosed{PRID: 42}}
	close(ch)

	d.Run(ctx, ch)

	if sessions.sessions["sess-1"].State != machine.Closed {
		t.Errorf("state = %v, want Closed", sessions.sessions["sess-1"].State)
	}
	if len(setter.calls) != 1 {
		t.Fatalf("display setter calls = %d, want 1", len(setter.calls))
	}
	if got := setter.calls[0]; got.sessionID != "sess-1" || got.info.Status != vcs.DisplayStatusClosed {
		t.Errorf("Set(%q, %v), want (sess-1, Closed)", got.sessionID, got.info.Status)
	}
}
