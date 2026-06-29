package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// lifecycleSessionStoreFake is a minimal db.SessionStore that records the last
// Update params and lets each test choose whether Update/Get succeed. Only the
// two methods the lifecycle RPCs touch are overridden; the embedded interface
// makes every other method a nil-panic, so an accidental call is loud.
type lifecycleSessionStoreFake struct {
	db.SessionStore

	updateErr     error
	getErr        error
	session       *models.Session
	lastUpdate    db.UpdateSessionParams
	updateCalls   int
	archiveErr    error
	archiveCalled bool
}

// Archive records that the lifecycle archive reached the DB write. The embedded
// db.SessionStore would otherwise nil-panic, so the success path of
// session.Lifecycle.ArchiveSession needs this override.
func (f *lifecycleSessionStoreFake) Archive(_ context.Context, _ string) error {
	f.archiveCalled = true
	return f.archiveErr
}

// archiveRepoStoreFake is a minimal db.RepoStore whose Get returns a fixed repo,
// so the lifecycle archive's repo lookup (and sessionProtoWithRepo) succeed
// without a real store. Any other method nil-panics loudly.
type archiveRepoStoreFake struct {
	db.RepoStore
	repo *models.Repo
}

func (r *archiveRepoStoreFake) Get(_ context.Context, _ string) (*models.Repo, error) {
	return r.repo, nil
}

func (f *lifecycleSessionStoreFake) Update(_ context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	f.updateCalls++
	f.lastUpdate = params
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.sessionOr(id), nil
}

func (f *lifecycleSessionStoreFake) Get(_ context.Context, id string) (*models.Session, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.sessionOr(id), nil
}

func (f *lifecycleSessionStoreFake) sessionOr(id string) *models.Session {
	if f.session != nil {
		return f.session
	}
	return &models.Session{ID: id}
}

// completionNotifierSpy records HandleSessionCompleted calls so a test can
// assert the orchestrator is (or is not) notified.
type completionNotifierSpy struct {
	calls []completionNotifierCall
}

type completionNotifierCall struct {
	sessionID string
	outcome   models.TaskMappingStatus
}

func (s *completionNotifierSpy) HandleSessionCompleted(_ context.Context, sessionID string, outcome models.TaskMappingStatus) {
	s.calls = append(s.calls, completionNotifierCall{sessionID: sessionID, outcome: outcome})
}

func TestPauseSession(t *testing.T) {
	t.Run("empty id is rejected", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{}}
		_, err := srv.PauseSession(context.Background(), connect.NewRequest(&pb.PauseSessionRequest{}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("update failure becomes internal error", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{updateErr: errors.New("boom")}}
		_, err := srv.PauseSession(context.Background(), connect.NewRequest(&pb.PauseSessionRequest{Id: "s1"}))
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("get failure after update becomes internal error", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{getErr: errors.New("gone")}}
		_, err := srv.PauseSession(context.Background(), connect.NewRequest(&pb.PauseSessionRequest{Id: "s1"}))
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("success disables automation and returns the session", func(t *testing.T) {
		store := &lifecycleSessionStoreFake{session: &models.Session{ID: "s1"}}
		srv := &Server{sessions: store}
		resp, err := srv.PauseSession(context.Background(), connect.NewRequest(&pb.PauseSessionRequest{Id: "s1"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Msg.GetSession().GetId() != "s1" {
			t.Errorf("session id = %q, want s1", resp.Msg.GetSession().GetId())
		}
		if store.lastUpdate.AutomationEnabled == nil || *store.lastUpdate.AutomationEnabled {
			t.Errorf("AutomationEnabled = %v, want pointer to false", store.lastUpdate.AutomationEnabled)
		}
	})
}

func TestResumeSession(t *testing.T) {
	t.Run("empty id is rejected", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{}}
		_, err := srv.ResumeSession(context.Background(), connect.NewRequest(&pb.ResumeSessionRequest{}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("update failure becomes internal error", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{updateErr: errors.New("boom")}}
		_, err := srv.ResumeSession(context.Background(), connect.NewRequest(&pb.ResumeSessionRequest{Id: "s1"}))
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("get failure after update becomes internal error", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{getErr: errors.New("gone")}}
		_, err := srv.ResumeSession(context.Background(), connect.NewRequest(&pb.ResumeSessionRequest{Id: "s1"}))
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("success enables automation and returns the session", func(t *testing.T) {
		store := &lifecycleSessionStoreFake{session: &models.Session{ID: "s1"}}
		srv := &Server{sessions: store}
		resp, err := srv.ResumeSession(context.Background(), connect.NewRequest(&pb.ResumeSessionRequest{Id: "s1"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Msg.GetSession().GetId() != "s1" {
			t.Errorf("session id = %q, want s1", resp.Msg.GetSession().GetId())
		}
		if store.lastUpdate.AutomationEnabled == nil || !*store.lastUpdate.AutomationEnabled {
			t.Errorf("AutomationEnabled = %v, want pointer to true", store.lastUpdate.AutomationEnabled)
		}
	})
}

func TestRetrySession(t *testing.T) {
	t.Run("empty id is rejected", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{}}
		_, err := srv.RetrySession(context.Background(), connect.NewRequest(&pb.RetrySessionRequest{}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("update failure becomes internal error", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{updateErr: errors.New("boom")}}
		_, err := srv.RetrySession(context.Background(), connect.NewRequest(&pb.RetrySessionRequest{Id: "s1"}))
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("get failure after update becomes internal error", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{getErr: errors.New("gone")}}
		_, err := srv.RetrySession(context.Background(), connect.NewRequest(&pb.RetrySessionRequest{Id: "s1"}))
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("success clears blocked reason and re-enables automation", func(t *testing.T) {
		store := &lifecycleSessionStoreFake{session: &models.Session{ID: "s1"}}
		srv := &Server{sessions: store}
		resp, err := srv.RetrySession(context.Background(), connect.NewRequest(&pb.RetrySessionRequest{Id: "s1"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Msg.GetSession().GetId() != "s1" {
			t.Errorf("session id = %q, want s1", resp.Msg.GetSession().GetId())
		}
		if store.lastUpdate.AutomationEnabled == nil || !*store.lastUpdate.AutomationEnabled {
			t.Errorf("AutomationEnabled = %v, want pointer to true", store.lastUpdate.AutomationEnabled)
		}
		// BlockedReason is a double pointer set to clear the column (*nil).
		if store.lastUpdate.BlockedReason == nil {
			t.Fatal("BlockedReason = nil, want non-nil pointer that clears the column")
		}
		if *store.lastUpdate.BlockedReason != nil {
			t.Errorf("*BlockedReason = %v, want nil (column cleared)", *store.lastUpdate.BlockedReason)
		}
	})
}

func TestCloseSession(t *testing.T) {
	t.Run("empty id is rejected", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{}}
		_, err := srv.CloseSession(context.Background(), connect.NewRequest(&pb.CloseSessionRequest{}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("update failure becomes internal error", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{updateErr: errors.New("boom")}}
		_, err := srv.CloseSession(context.Background(), connect.NewRequest(&pb.CloseSessionRequest{Id: "s1"}))
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("get failure after update becomes internal error", func(t *testing.T) {
		srv := &Server{sessions: &lifecycleSessionStoreFake{getErr: errors.New("gone")}}
		_, err := srv.CloseSession(context.Background(), connect.NewRequest(&pb.CloseSessionRequest{Id: "s1"}))
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
		}
	})

	t.Run("success sets closed state and notifies completion", func(t *testing.T) {
		store := &lifecycleSessionStoreFake{session: &models.Session{ID: "s1"}}
		notifier := &completionNotifierSpy{}
		srv := &Server{sessions: store, completionNotifier: notifier}
		resp, err := srv.CloseSession(context.Background(), connect.NewRequest(&pb.CloseSessionRequest{Id: "s1"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Msg.GetSession().GetId() != "s1" {
			t.Errorf("session id = %q, want s1", resp.Msg.GetSession().GetId())
		}
		if store.lastUpdate.State == nil || *store.lastUpdate.State != int(machine.Closed) {
			t.Errorf("State = %v, want pointer to %d (Closed)", store.lastUpdate.State, int(machine.Closed))
		}
		if len(notifier.calls) != 1 {
			t.Fatalf("completion notifier calls = %d, want 1", len(notifier.calls))
		}
		if notifier.calls[0].sessionID != "s1" {
			t.Errorf("notified session id = %q, want s1", notifier.calls[0].sessionID)
		}
		if notifier.calls[0].outcome != models.TaskMappingStatusFailed {
			t.Errorf("notified outcome = %v, want Failed", notifier.calls[0].outcome)
		}
	})
}

// TestStopSessionEmptyID pins the id guard on StopSession (server.go:1434). The
// nil lifecycle means a mutant that skips the guard would panic dereferencing
// s.lifecycle rather than returning a clean InvalidArgument.
func TestStopSessionEmptyID(t *testing.T) {
	srv := &Server{}
	_, err := srv.StopSession(context.Background(), connect.NewRequest(&pb.StopSessionRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}
}

// TestMergeSessionEmptyID pins the id guard on MergeSession (server.go:1547).
// With nil stores a mutant that skips the guard would panic on the first store
// access instead of returning InvalidArgument.
func TestMergeSessionEmptyID(t *testing.T) {
	srv := &Server{}
	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}
}

// TestArchiveSessionAndNotify verifies the archiveSessionAndNotify helper
// (BOS-101): a genuinely missing session is idempotent (fires onSessionDeleted,
// nil error), an already-archived session that still exists fires
// onSessionUpdated (and is NOT dropped from the read model), and a real
// lifecycle error propagates.
func TestArchiveSessionAndNotify(t *testing.T) {
	const sessionID = "s1"

	t.Run("ErrNoRows fires onSessionDeleted and returns nil", func(t *testing.T) {
		store := &lifecycleSessionStoreFake{getErr: sql.ErrNoRows}
		lc := session.NewLifecycle(store, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop())
		var deletedID string
		srv := &Server{
			sessions:         store,
			lifecycle:        lc,
			onSessionDeleted: func(_ context.Context, id string) { deletedID = id },
		}
		if err := srv.ArchiveSessionAndNotify(context.Background(), sessionID); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if deletedID != sessionID {
			t.Errorf("onSessionDeleted called with %q, want %q", deletedID, sessionID)
		}
	})

	t.Run("non-ErrNoRows lifecycle error propagates", func(t *testing.T) {
		store := &lifecycleSessionStoreFake{getErr: errors.New("boom")}
		lc := session.NewLifecycle(store, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop())
		srv := &Server{
			sessions:  store,
			lifecycle: lc,
		}
		if err := srv.ArchiveSessionAndNotify(context.Background(), sessionID); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	// BOS-101 headline behavior: a successful archive must emit onSessionUpdated
	// so the TUI/bosso read-model drop the session immediately (no poll wait).
	t.Run("success archives and fires onSessionUpdated", func(t *testing.T) {
		// WorktreePath == repo.LocalPath skips the worktree archive; nil tmux and
		// nil AgentSessionID skip the process/tmux teardown, so the success path
		// runs with minimal deps.
		store := &lifecycleSessionStoreFake{
			session: &models.Session{ID: sessionID, RepoID: "r1", WorktreePath: "/x"},
		}
		repos := &archiveRepoStoreFake{repo: &models.Repo{ID: "r1", LocalPath: "/x"}}
		lc := session.NewLifecycle(store, repos, nil, nil, nil, nil, nil, nil, zerolog.Nop())

		var updatedID string
		srv := &Server{
			sessions:         store,
			repos:            repos,
			lifecycle:        lc,
			onSessionUpdated: func(_ context.Context, p *pb.Session) { updatedID = p.GetId() },
		}

		if err := srv.ArchiveSessionAndNotify(context.Background(), sessionID); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !store.archiveCalled {
			t.Error("expected the lifecycle archive to reach sessions.Archive")
		}
		if updatedID != sessionID {
			t.Errorf("onSessionUpdated fired with %q, want %q", updatedID, sessionID)
		}
	})

	// BOS-101 regression: SQLiteSessionStore.Archive returns sql.ErrNoRows both
	// for a missing row AND for one that is already archived (its
	// archived_at IS NULL update matches nothing). An already-archived session
	// whose row still exists must stay archived/in trash — fire onSessionUpdated,
	// NOT onSessionDeleted, so bosso does not drop it from the read model.
	t.Run("already-archived session fires onSessionUpdated, not onSessionDeleted", func(t *testing.T) {
		store := &lifecycleSessionStoreFake{
			session:    &models.Session{ID: sessionID, RepoID: "r1", WorktreePath: "/x"},
			archiveErr: sql.ErrNoRows,
		}
		repos := &archiveRepoStoreFake{repo: &models.Repo{ID: "r1", LocalPath: "/x"}}
		lc := session.NewLifecycle(store, repos, nil, nil, nil, nil, nil, nil, zerolog.Nop())

		var updatedID string
		var deletedCalled bool
		srv := &Server{
			sessions:         store,
			repos:            repos,
			lifecycle:        lc,
			onSessionUpdated: func(_ context.Context, p *pb.Session) { updatedID = p.GetId() },
			onSessionDeleted: func(_ context.Context, _ string) { deletedCalled = true },
		}

		if err := srv.ArchiveSessionAndNotify(context.Background(), sessionID); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if deletedCalled {
			t.Error("onSessionDeleted must not fire for an already-archived session that still exists")
		}
		if updatedID != sessionID {
			t.Errorf("onSessionUpdated fired with %q, want %q", updatedID, sessionID)
		}
	})
}

// TestArchiveSession_MissingSession tests BOS-76: archiving an orphaned session
// (one whose DB row is gone) must be an idempotent success that fires the
// KIND_DELETED callback so bosso drops the stale read-model row.
func TestArchiveSession_MissingSession(t *testing.T) {
	t.Run("sql.ErrNoRows is idempotent success and fires onSessionDeleted", func(t *testing.T) {
		store := &lifecycleSessionStoreFake{getErr: sql.ErrNoRows}
		lc := session.NewLifecycle(store, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop())

		var deletedID string
		srv := &Server{
			sessions:         store,
			lifecycle:        lc,
			onSessionDeleted: func(_ context.Context, id string) { deletedID = id },
		}

		resp, err := srv.ArchiveSession(context.Background(), connect.NewRequest(&pb.ArchiveSessionRequest{Id: "s1"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Msg.GetSession().GetId() != "s1" {
			t.Errorf("response session id = %q, want s1", resp.Msg.GetSession().GetId())
		}
		if deletedID != "s1" {
			t.Errorf("onSessionDeleted called with %q, want s1", deletedID)
		}
	})

	t.Run("non-ErrNoRows error returns CodeInternal without firing onSessionDeleted", func(t *testing.T) {
		store := &lifecycleSessionStoreFake{getErr: errors.New("boom")}
		lc := session.NewLifecycle(store, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop())

		var deletedCalled bool
		srv := &Server{
			sessions:         store,
			lifecycle:        lc,
			onSessionDeleted: func(_ context.Context, _ string) { deletedCalled = true },
		}

		_, err := srv.ArchiveSession(context.Background(), connect.NewRequest(&pb.ArchiveSessionRequest{Id: "s1"}))
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("code = %v, want CodeInternal (err=%v)", connect.CodeOf(err), err)
		}
		if deletedCalled {
			t.Error("onSessionDeleted must not be called on non-ErrNoRows errors")
		}
	})
}
