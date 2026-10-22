package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/status"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/adapter/proofenv"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/taskorchestrator"
)

type refusingStarter struct{ started []string }

func (s *refusingStarter) StartSession(_ context.Context, id string, _ session.StartSessionOpts) error {
	s.started = append(s.started, id)
	return nil
}
func (s *refusingStarter) CleanUpFailedBootstrapArtifacts(context.Context, string) {}

type boundRotation struct{}

func (boundRotation) CurrentBinding(context.Context, *models.Session) (session.RotationBinding, bool, error) {
	return session.RotationBinding{CappedAccountID: "acct-capped", Provider: "claude", RotationCapable: true}, true, nil
}

type fixedMaterializer struct{}

func (fixedMaterializer) Materialize(context.Context, *models.AgentAccount) (map[string]string, error) {
	return map[string]string{}, nil
}

type revivalWindowFixture struct {
	ctx      context.Context
	sessions *status.RecomputingSessionStore
	repos    db.RepoStore
	chats    db.AgentChatStore
	repoID   string
	reviving string
	creator  taskorchestrator.SessionCreator
	starter  *refusingStarter
	lc       *session.Lifecycle
	runner   *setupStreamAgent
}

const revivalWindowPR = 42

// newRevivalWindowFixture puts one session on PR 42 in the shape a background
// revival picks up, wired to the production store, the task orchestrator's
// create path and its liveness check.
func newRevivalWindowFixture(t *testing.T, park func(*db.UpdateSessionParams)) (*revivalWindowFixture, func(ctx context.Context, id string) error) {
	t.Helper()
	ctx := context.Background()
	database := setupServerTestDB(t)
	repos := db.NewRepoStore(database)
	sessions := status.NewRecomputingSessionStore(db.NewSessionStore(database), &dismissRecomputerFake{})
	chats := db.NewAgentChatStore(database)
	repo, err := repos.Create(ctx, db.CreateRepoParams{DisplayName: "revival", LocalPath: t.TempDir(), OriginURL: "https://github.com/test/revival", DefaultBaseBranch: "main", WorktreeBaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	canRotate := true
	if _, err = repos.Update(ctx, repo.ID, db.UpdateRepoParams{CanAutoRotate: &canRotate}); err != nil {
		t.Fatal(err)
	}
	pr := revivalWindowPR
	row, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "Bump deps", Plan: "update the lockfile", WorktreePath: t.TempDir(), BranchName: "dependabot/npm/lodash", AgentName: "claude", PRNumber: &pr})
	if err != nil {
		t.Fatal(err)
	}
	prior := "old-run"
	priorPtr := &prior
	params := db.UpdateSessionParams{AgentSessionID: &priorPtr}
	park(&params)
	if _, err = sessions.Update(ctx, row.ID, params); err != nil {
		t.Fatal(err)
	}
	if _, err = chats.Create(ctx, db.CreateAgentChatParams{SessionID: row.ID, AgentSessionID: prior, AgentName: "claude", Title: "Bump deps"}); err != nil {
		t.Fatal(err)
	}

	runner := &setupStreamAgent{}
	lc := session.NewLifecycle(sessions, repos, chats, nil, nil, runner, nil, nil, zerolog.Nop())
	lc.SetProofEnvResolver(proofenv.NewNoop())
	enabled := true
	lc.SetRotationConfig(config.ManagedAgentAccountsConfig{AutoResumeOrphans: &enabled})
	lc.SetRotationBinding(boundRotation{})
	lc.SetAgentAccountMaterializer(fixedMaterializer{})
	lc.SetRotationDecider(func(context.Context, rotation.Signal) (rotation.Outcome, error) {
		return rotation.Outcome{Kind: rotation.OutcomeRotate, NextAccount: &models.AgentAccount{ID: "acct-next"}}, nil
	})

	liveness := taskorchestrator.NewLivenessChecker(sessions, chats, func(*models.Session) agent.Runner { return runner }, nil, lc.StartedAt())
	starter := &refusingStarter{}
	creator := taskorchestrator.NewSessionCreatorWithDefaultAgentProviderAndLiveness(sessions, starter, func() string { return "claude" }, liveness, zerolog.Nop())
	f := &revivalWindowFixture{ctx: ctx, sessions: sessions, repos: repos, chats: chats, repoID: repo.ID, reviving: row.ID, creator: creator, starter: starter, lc: lc, runner: runner}
	backdate := func(ctx context.Context, id string) error {
		_, err := database.ExecContext(ctx, `UPDATE sessions SET state_entered_at = ? WHERE id = ?`, time.Now().Add(-session.RevivalStartGrace-time.Second).UTC().Format(sqlutil.TimeLayout), id)
		return err
	}
	return f, backdate
}

// restart stands up the lifecycle a daemon that started at startedAt builds,
// wired to the production liveness check, and runs its startup pass.
func (f *revivalWindowFixture) restart(startedAt time.Time) {
	runner := &setupStreamAgent{}
	lc := session.NewLifecycle(f.sessions, f.repos, f.chats, nil, &setupStreamWorktree{}, runner, nil, nil, zerolog.Nop())
	lc.SetProofEnvResolver(proofenv.NewNoop())
	lc.SetSessionLiveness(taskorchestrator.NewLivenessChecker(f.sessions, f.chats, func(*models.Session) agent.Runner { return runner }, nil, startedAt))
	lc.Bootstrap(f.ctx)
}

func (f *revivalWindowFixture) createOnPR() error {
	pr := revivalWindowPR
	_, err := f.creator.CreateSession(f.ctx, taskorchestrator.CreateSessionOpts{
		RepoID: f.repoID, Title: "Bump deps again", Plan: "update the lockfile", BaseBranch: "main",
		HeadBranch: "dependabot/npm/lodash", PRNumber: &pr, PreventDuplicateActiveSession: true,
	})
	return err
}

func parkOrphaned(p *db.UpdateSessionParams) {
	state := int(machine.Orphaned)
	reason := session.OrphanedHeadlessRunReason
	reasonPtr := &reason
	p.State, p.BlockedReason = &state, &reasonPtr
}

func parkForRotation(p *db.UpdateSessionParams) {
	state := int(machine.Blocked)
	resumeAt := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	resumePtr := &resumeAt
	p.State, p.RotationResumeAt = &state, &resumePtr
}

// TestTaskCreateWaitsOutARevivalBetweenItsWriteAndItsRun pins the window the
// #3198 verifier found: the orphan resume and the parked rotation write
// ImplementingPlan, release the start-path lock and then spawn, so the row
// still names the dead run while the spawn runs. A task create for the same PR
// in that window used to read the row as dead and start a second session.
func TestTaskCreateWaitsOutARevivalBetweenItsWriteAndItsRun(t *testing.T) {
	revivals := []struct {
		name   string
		park   func(*db.UpdateSessionParams)
		revive func(*session.Lifecycle, context.Context) int
	}{
		{"orphan resume", parkOrphaned, (*session.Lifecycle).ResumeOrphanedHeadlessRuns},
		{"parked rotation", parkForRotation, (*session.Lifecycle).SweepParkedRotations},
	}
	for _, rv := range revivals {
		t.Run(rv.name, func(t *testing.T) {
			f, _ := newRevivalWindowFixture(t, rv.park)
			var createErr error
			spawned := 0
			f.runner.startFn = func(context.Context, string, string, *string, string) (string, error) {
				spawned++
				createErr = f.createOnPR()
				return "new-run", nil
			}

			if n := rv.revive(f.lc, f.ctx); n != 1 {
				t.Fatalf("revived = %d, want 1", n)
			}
			if spawned != 1 {
				t.Fatalf("spawned = %d, want 1", spawned)
			}
			var dup *session.DuplicateActivePRSessionError
			if !errors.As(createErr, &dup) || dup.ExistingSessionID != f.reviving {
				t.Fatalf("create during the spawn = %v, want it refused as a duplicate of %s", createErr, f.reviving)
			}
			if len(f.starter.started) != 0 {
				t.Fatalf("started sessions %v, want none", f.starter.started)
			}
			rows, err := f.sessions.ListByRepoAndPR(f.ctx, f.repoID, revivalWindowPR)
			if err != nil || len(rows) != 1 {
				t.Fatalf("sessions on PR %d = %d (err %v), want 1", revivalWindowPR, len(rows), err)
			}
			revived, err := f.sessions.Get(f.ctx, f.reviving)
			if err != nil || revived.State != machine.ImplementingPlan || revived.AgentSessionID == nil || *revived.AgentSessionID != "new-run" {
				t.Fatalf("revived session = %+v (err %v), want ImplementingPlan on new-run", revived, err)
			}
			if session.RevivalStarting(revived, time.Time{}, time.Now()) {
				t.Fatal("the committed revival still reads as starting")
			}
		})
	}

	// A daemon that dies mid-spawn leaves the claimed row behind. It reads as
	// starting only for RevivalStartGrace, then as the dead run it names.
	t.Run("a spawn that never returned stops holding the PR after the grace", func(t *testing.T) {
		f, backdate := newRevivalWindowFixture(t, parkOrphaned)
		claimed, err := f.sessions.MarkUnarchivedOrphanClaimed(f.ctx, f.reviving, session.OrphanedHeadlessRunReason)
		if err != nil || !claimed {
			t.Fatalf("claim = %v, %v", claimed, err)
		}
		var dup *session.DuplicateActivePRSessionError
		if err := f.createOnPR(); !errors.As(err, &dup) {
			t.Fatalf("create inside the grace = %v, want a duplicate refusal", err)
		}
		if err := backdate(f.ctx, f.reviving); err != nil {
			t.Fatal(err)
		}
		if err := f.createOnPR(); err != nil {
			t.Fatalf("create after the grace = %v, want it to go through", err)
		}
		if len(f.starter.started) != 1 {
			t.Fatalf("started sessions %v, want one", f.starter.started)
		}
	})
}

// TestRestartMidRevivalOrphansTheClaimedRow pins finding 1 of the #3204
// verifier. A daemon that restarts while a revival spawns leaves the claimed
// row in ImplementingPlan naming the dead run. No spawn of the new daemon is in
// flight for it, so its one startup sweep must read the row as dead and orphan
// it for the orphan resume to retry, even inside RevivalStartGrace.
func TestRestartMidRevivalOrphansTheClaimedRow(t *testing.T) {
	claims := []struct {
		name  string
		park  func(*db.UpdateSessionParams)
		claim func(*revivalWindowFixture) (bool, error)
	}{
		{"orphan resume", parkOrphaned, func(f *revivalWindowFixture) (bool, error) {
			return f.sessions.MarkUnarchivedOrphanClaimed(f.ctx, f.reviving, session.OrphanedHeadlessRunReason)
		}},
		{"parked rotation", parkForRotation, func(f *revivalWindowFixture) (bool, error) {
			return f.sessions.UpdateStateConditional(f.ctx, f.reviving, int(machine.ImplementingPlan), int(machine.Blocked))
		}},
	}
	for _, c := range claims {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newRevivalWindowFixture(t, c.park)
			claimed, err := c.claim(f)
			if err != nil || !claimed {
				t.Fatalf("claim = %v, %v", claimed, err)
			}
			row, err := f.sessions.Get(f.ctx, f.reviving)
			if err != nil || !session.RevivalStarting(row, time.Time{}, time.Now()) {
				t.Fatalf("claimed row = %+v (err %v), want it inside the start grace", row, err)
			}

			f.restart(row.StateEnteredAt.Add(time.Second))

			after, err := f.sessions.Get(f.ctx, f.reviving)
			if err != nil || after.State != machine.Orphaned {
				t.Fatalf("after restart Bootstrap state = %v (err %v), want orphaned", after.State, err)
			}
		})
	}
}

// TestLeavingAParkedRotationOtherwiseDisarmsIt pins finding 2 of the #3204
// verifier. A parked row that comes back to life by another path, here an
// unblock, must not keep its rotation stamp: re-entering ImplementingPlan would
// read as a revival in flight, and a later Blocked for an unrelated reason
// would be redispatched by the rotation sweep.
func TestLeavingAParkedRotationOtherwiseDisarmsIt(t *testing.T) {
	f, _ := newRevivalWindowFixture(t, parkForRotation)
	implementing, blocked := int(machine.ImplementingPlan), int(machine.Blocked)
	revived, err := f.sessions.Update(f.ctx, f.reviving, db.UpdateSessionParams{State: &implementing})
	if err != nil {
		t.Fatal(err)
	}
	if session.RevivalStarting(revived, time.Time{}, time.Now()) {
		t.Fatal("an unblocked row reads as a revival in flight")
	}
	if _, err := f.sessions.Update(f.ctx, f.reviving, db.UpdateSessionParams{State: &blocked}); err != nil {
		t.Fatal(err)
	}
	spawned := 0
	f.runner.startFn = func(context.Context, string, string, *string, string) (string, error) {
		spawned++
		return "new-run", nil
	}
	if n := f.lc.SweepParkedRotations(f.ctx); n != 0 || spawned != 0 {
		t.Fatalf("rotation sweep redispatched %d (spawned %d), want none for a row no longer parked", n, spawned)
	}
}
