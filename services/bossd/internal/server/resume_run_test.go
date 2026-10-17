package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/proofenv"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog"
)

func TestResumeSessionRunRejectsMissingID(t *testing.T) {
	for _, id := range []string{"", "  "} {
		_, err := (&Server{}).ResumeSessionRun(context.Background(), connect.NewRequest(&pb.ResumeSessionRunRequest{SessionId: id}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("id %q: err = %v, want InvalidArgument", id, err)
		}
	}
}

func TestResumeSessionRunLifecycleErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		code  connect.Code
	}{
		{"missing session", sql.ErrNoRows, connect.CodeNotFound},
		{"store failed", errors.New("store unavailable"), connect.CodeInternal},
		{"not resumable", session.ErrRunNotResumable, connect.CodeFailedPrecondition},
		{"start-path lock busy", fmt.Errorf("%w: timed out", session.ErrStartPathBusy), connect.CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &lifecycleSessionStoreFake{getErr: tc.cause}
			lc := session.NewLifecycle(store, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop())
			srv := &Server{sessions: store, lifecycle: lc}
			_, err := srv.ResumeSessionRun(context.Background(), connect.NewRequest(&pb.ResumeSessionRunRequest{SessionId: "s1"}))
			if connect.CodeOf(err) != tc.code {
				t.Fatalf("err = %v, want %v", err, tc.code)
			}
			if !errors.Is(err, tc.cause) {
				t.Fatalf("err = %v, lost cause %v", err, tc.cause)
			}
		})
	}
}

// TestResumeSessionRunRefusesARevivalOntoATakenBranch drives the resume RPC
// through the production store: a Blocked run whose branch another live
// session now holds is refused as FailedPrecondition, naming that session,
// with nothing launched and the row left Blocked.
func TestResumeSessionRunRefusesARevivalOntoATakenBranch(t *testing.T) {
	ctx := context.Background()
	database := setupServerTestDB(t)
	repos := db.NewRepoStore(database)
	sessions := db.NewRecomputingSessionStore(db.NewSessionStore(database), &dismissRecomputerFake{})
	chats := db.NewAgentChatStore(database)
	repo, err := repos.Create(ctx, db.CreateRepoParams{DisplayName: "resume", LocalPath: t.TempDir(), OriginURL: "https://github.com/test/resume", DefaultBaseBranch: "main", WorktreeBaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	row, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "Build", Plan: "/boss-build TEST-1", WorktreePath: repo.LocalPath, BranchName: "fix/shared", AgentName: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	prior := "old-run"
	priorPtr := &prior
	blocked := int(machine.Blocked)
	if _, err = sessions.Update(ctx, row.ID, db.UpdateSessionParams{State: &blocked, AgentSessionID: &priorPtr}); err != nil {
		t.Fatal(err)
	}
	if _, err = chats.Create(ctx, db.CreateAgentChatParams{SessionID: row.ID, AgentSessionID: prior, AgentName: "claude", Title: "Build"}); err != nil {
		t.Fatal(err)
	}
	rival, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "Rival", WorktreePath: t.TempDir(), BranchName: "fix/shared"})
	if err != nil {
		t.Fatal(err)
	}
	awaiting := int(machine.AwaitingChecks)
	if _, err = sessions.Update(ctx, rival.ID, db.UpdateSessionParams{State: &awaiting}); err != nil {
		t.Fatal(err)
	}
	launched := 0
	runner := &setupStreamAgent{startFn: func(context.Context, string, string, *string, string) (string, error) {
		launched++
		return "new-run", nil
	}}
	lc := session.NewLifecycle(sessions, repos, chats, nil, nil, runner, nil, nil, zerolog.Nop())
	lc.SetProofEnvResolver(proofenv.NewNoop())
	srv := &Server{sessions: sessions, repos: repos, lifecycle: lc}

	_, err = srv.ResumeSessionRun(ctx, connect.NewRequest(&pb.ResumeSessionRunRequest{SessionId: row.ID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), rival.ID) {
		t.Fatalf("err = %v, want it to name the rival %s", err, rival.ID)
	}
	if launched != 0 {
		t.Fatalf("launched = %d, want 0", launched)
	}
	stored, err := sessions.Get(ctx, row.ID)
	if err != nil || stored.State != machine.Blocked {
		t.Fatalf("session = %v err = %v, want Blocked kept", stored, err)
	}
}

func TestResumeSessionRunClearsLedgerAndRelaunches(t *testing.T) {
	testResumeThroughProductionStore(t, false, false)
}

func TestResumeSessionRunAutomaticThroughProductionStore(t *testing.T) {
	testResumeThroughProductionStore(t, true, false)
}

// A replacement chat created within the original's millisecond, with an ID
// that sorts first, must not move the run key: the ledger stays on the
// original run and the backoff continues instead of restarting.
func TestResumeSessionRunAutomaticSurvivesSameMillisecondReplacement(t *testing.T) {
	testResumeThroughProductionStore(t, true, true)
}

func testResumeThroughProductionStore(t *testing.T, automatic, sameMillisecondReplacement bool) {
	t.Helper()
	ctx := context.Background()
	database := setupServerTestDB(t)
	repos := db.NewRepoStore(database)
	sessions := db.NewRecomputingSessionStore(db.NewSessionStore(database), &dismissRecomputerFake{})
	chats := db.NewAgentChatStore(database)
	ledger := db.NewResumeAttemptStore(database)
	repo, err := repos.Create(ctx, db.CreateRepoParams{DisplayName: "resume", LocalPath: t.TempDir(), OriginURL: "https://github.com/test/resume", DefaultBaseBranch: "main", WorktreeBaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	row, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "Build", Plan: "/boss-build TEST-1", WorktreePath: repo.LocalPath, AgentName: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	prior := "old-run"
	priorPtr := &prior
	state := int(machine.Blocked)
	if automatic {
		state = int(machine.ImplementingPlan)
	}
	params := db.UpdateSessionParams{State: &state, AgentSessionID: &priorPtr, IsTmuxUnattended: &automatic}
	if !automatic {
		reason := "previous run reported BLOCKED"
		reasonPtr := &reason
		params.RunBlockedReason = &reasonPtr
	}
	if _, err = sessions.Update(ctx, row.ID, params); err != nil {
		t.Fatal(err)
	}
	if _, err = chats.Create(ctx, db.CreateAgentChatParams{SessionID: row.ID, AgentSessionID: prior, AgentName: "claude", Title: "Build"}); err != nil {
		t.Fatal(err)
	}
	if !automatic {
		_, err = ledger.Record(ctx, db.RecordResumeAttemptParams{SessionID: row.ID, RunKey: prior, AttemptedAt: time.Now(), FailureClass: "auth_expired"})
	}
	if err != nil {
		t.Fatal(err)
	}
	launched := 0
	runner := &setupStreamAgent{startFn: func(_ context.Context, _, prompt string, resume *string, _ string) (string, error) {
		launched++
		if !strings.Contains(prompt, "/boss-build TEST-1") || resume != nil {
			t.Fatalf("prompt=%q resume=%v", prompt, resume)
		}
		return fmt.Sprintf("new-run-%d", launched), nil
	}}
	lc := session.NewLifecycle(sessions, repos, chats, nil, nil, runner, nil, nil, zerolog.Nop())
	lc.SetProofEnvResolver(proofenv.NewNoop())
	lc.SetRunResumeStore(ledger)
	if automatic {
		death := time.Now().UTC()
		now := death
		lc.SetClockForTest(func() time.Time { return now })
		for i, minutes := range []int{0, 1, 3, 11, 12} {
			now = death.Add(time.Duration(minutes) * time.Minute)
			lc.ResumeDroppedRuns(ctx)
			wantLaunches := []int{0, 1, 2, 3, 3}[i]
			if launched != wantLaunches {
				t.Fatalf("+%dm launches=%d, want %d", minutes, launched, wantLaunches)
			}
			if sameMillisecondReplacement && launched == 1 {
				if _, err := database.ExecContext(ctx, `UPDATE agent_chats SET created_at = (SELECT created_at FROM agent_chats WHERE agent_session_id = ?), id = '0' WHERE agent_session_id = 'new-run-1'`, prior); err != nil {
					t.Fatal(err)
				}
			}
		}
		stored, err := sessions.Get(ctx, row.ID)
		if err != nil || stored.State != machine.Blocked {
			t.Fatalf("exhausted session=%v err=%v", stored, err)
		}
		attempts, err := ledger.ListForRun(ctx, row.ID, prior)
		if err != nil || len(attempts) != 3 {
			t.Fatalf("attempts=%v err=%v", attempts, err)
		}
		return
	}
	srv := &Server{sessions: sessions, repos: repos, lifecycle: lc}
	resp, err := srv.ResumeSessionRun(ctx, connect.NewRequest(&pb.ResumeSessionRunRequest{SessionId: row.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if launched != 1 || resp.Msg.GetSession().GetAgentSessionId() != "new-run-1" {
		t.Fatalf("launched=%d response=%v", launched, resp.Msg)
	}
	attempts, err := ledger.ListForRun(ctx, row.ID, prior)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("attempts=%v err=%v", attempts, err)
	}
	stored, err := sessions.Get(ctx, row.ID)
	if err != nil || stored.State != machine.ImplementingPlan {
		t.Fatalf("session=%v err=%v", stored, err)
	}
	if stored.RunBlockedReason != nil {
		t.Fatalf("replacement inherited prior run's BLOCKED report: %q", *stored.RunBlockedReason)
	}
}
