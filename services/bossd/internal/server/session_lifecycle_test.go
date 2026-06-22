package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/db"
)

// lifecycleSessionStoreFake is a minimal db.SessionStore that records the last
// Update params and lets each test choose whether Update/Get succeed. Only the
// two methods the lifecycle RPCs touch are overridden; the embedded interface
// makes every other method a nil-panic, so an accidental call is loud.
type lifecycleSessionStoreFake struct {
	db.SessionStore

	updateErr   error
	getErr      error
	session     *models.Session
	lastUpdate  db.UpdateSessionParams
	updateCalls int
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
