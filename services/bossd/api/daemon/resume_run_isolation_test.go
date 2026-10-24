package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossd/internal/status"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/proofenv"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// signallingChats reports the first time a resume lists one session's chats,
// which both resume paths do only once they hold that session's resume gate.
type signallingChats struct {
	db.AgentChatStore
	sessionID string
	listed    chan struct{}
	once      sync.Once
}

func (c *signallingChats) ListBySession(ctx context.Context, sessionID string) ([]*domain.AgentChat, error) {
	if sessionID == c.sessionID {
		c.once.Do(func() { close(c.listed) })
	}
	return c.AgentChatStore.ListBySession(ctx, sessionID)
}

// TestManualResumeWaitingOnAWedgedRepoDoesNotStallOtherRepos pins finding 4 of
// the #3198 verifier: a manual resume waits up to StartPathLockTimeout for its
// repo's start-path lock, and it must not hold the automatic dropped-run resume
// of a session in another repo while it waits.
func TestManualResumeWaitingOnAWedgedRepoDoesNotStallOtherRepos(t *testing.T) {
	ctx := context.Background()
	database := setupServerTestDB(t)
	repos := db.NewRepoStore(database)
	sessions := status.NewRecomputingSessionStore(db.NewSessionStore(database), &dismissRecomputerFake{})
	ledger := db.NewResumeAttemptStore(database)
	baseChats := db.NewAgentChatStore(database)

	newSession := func(name, plan string, params db.UpdateSessionParams) (*models.Repo, *models.Session) {
		t.Helper()
		repo, err := repos.Create(ctx, db.CreateRepoParams{DisplayName: name, LocalPath: t.TempDir(), OriginURL: "https://github.com/test/" + name, DefaultBaseBranch: "main", WorktreeBaseDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		row, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: name, Plan: plan, WorktreePath: t.TempDir(), BranchName: "fix/" + name, AgentName: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		prior := "old-" + name
		priorPtr := &prior
		params.AgentSessionID = &priorPtr
		if _, err = sessions.Update(ctx, row.ID, params); err != nil {
			t.Fatal(err)
		}
		if _, err = baseChats.Create(ctx, db.CreateAgentChatParams{SessionID: row.ID, AgentSessionID: prior, AgentName: "claude", Title: name}); err != nil {
			t.Fatal(err)
		}
		return repo, row
	}
	blocked := int(machine.Blocked)
	wedgedRepo, manual := newSession("wedged", "/boss-build TEST-1", db.UpdateSessionParams{State: &blocked})
	implementing, unattended := int(machine.ImplementingPlan), true
	_, dropped := newSession("other", "/boss-build TEST-2", db.UpdateSessionParams{State: &implementing, IsTmuxUnattended: &unattended})

	chats := &signallingChats{AgentChatStore: baseChats, sessionID: manual.ID, listed: make(chan struct{})}
	var mu sync.Mutex
	launches := map[string]int{}
	runner := &setupStreamAgent{startFn: func(_ context.Context, _, prompt string, _ *string, _ string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		launches[prompt[len(prompt)-len("TEST-1"):]]++
		return "new-run-" + prompt[len(prompt)-len("TEST-1"):], nil
	}}
	launchesOf := func(ticket string) int {
		mu.Lock()
		defer mu.Unlock()
		return launches[ticket]
	}
	lc := session.NewLifecycle(sessions, repos, chats, nil, nil, runner, nil, nil, zerolog.Nop())
	lc.SetProofEnvResolver(proofenv.NewNoop())
	lc.SetRunResumeStore(ledger)
	death := time.Now().UTC()
	now := death
	lc.SetClockForTest(func() time.Time { return now })
	lc.ResumeDroppedRuns(ctx)
	if got := launchesOf("TEST-2"); got != 0 {
		t.Fatalf("launches at the death = %d, want 0", got)
	}
	now = death.Add(time.Minute)

	releaseWedge, _, err := session.AcquireStartPath(ctx, wedgedRepo.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(releaseWedge)

	srv := &Server{sessions: sessions, repos: repos, lifecycle: lc}
	manualDone := make(chan error, 1)
	go func() {
		_, err := srv.ResumeSessionRun(ctx, connect.NewRequest(&pb.ResumeSessionRunRequest{SessionId: manual.ID}))
		manualDone <- err
	}()
	<-chats.listed

	sweepDone := make(chan int, 1)
	go func() { sweepDone <- lc.ResumeDroppedRuns(ctx) }()
	select {
	case resumed := <-sweepDone:
		if resumed != 1 || launchesOf("TEST-2") != 1 {
			t.Errorf("automatic resume in the other repo = %d resumed, %d launched; want 1 and 1", resumed, launchesOf("TEST-2"))
		}
		close(sweepDone)
	case <-time.After(30 * time.Second):
		t.Error("the automatic resume in another repo waited on the manual resume stuck behind the wedged repo")
	}
	select {
	case err := <-manualDone:
		t.Fatalf("manual resume finished while its repo was wedged: %v", err)
	default:
	}

	// A second manual resume waits on the gate the first one holds. A caller
	// that gives up while waiting is answered Canceled, not Unavailable.
	giveUp, cancel := context.WithCancel(ctx)
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := srv.ResumeSessionRun(giveUp, connect.NewRequest(&pb.ResumeSessionRunRequest{SessionId: manual.ID})); connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("resume abandoned while waiting = %v (code %v), want Canceled", err, connect.CodeOf(err))
	}

	releaseWedge()
	if err := <-manualDone; err != nil {
		t.Fatalf("manual resume after the wedge cleared: %v", err)
	}
	if got := launchesOf("TEST-1"); got != 1 {
		t.Fatalf("manual launches = %d, want 1", got)
	}
	<-sweepDone
	stored, err := sessions.Get(ctx, dropped.ID)
	if err != nil || stored.AgentSessionID == nil || *stored.AgentSessionID != "new-run-TEST-2" {
		t.Fatalf("dropped session = %+v (err %v), want it on new-run-TEST-2", stored, err)
	}
}
