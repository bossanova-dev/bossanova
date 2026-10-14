package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
)

type dismissRecomputerFake struct{ sessionIDs []string }

func (f *dismissRecomputerFake) Recompute(_ context.Context, sessionID string) error {
	f.sessionIDs = append(f.sessionIDs, sessionID)
	return nil
}

type dismissHarness struct {
	srv        *Server
	sessions   *db.SQLiteSessionStore
	recomputer *dismissRecomputerFake
	published  []*pb.Session
	sessionID  string
}

func newDismissHarness(t *testing.T) *dismissHarness {
	t.Helper()
	database := dbtest.New(t)
	repos := db.NewRepoStore(database)
	sessions := db.NewSessionStore(database)
	ctx := context.Background()
	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/test-repo",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "fix row",
		WorktreePath: "/tmp/wt/fix-row",
		BranchName:   "feat/fix-row",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	h := &dismissHarness{sessions: sessions, recomputer: &dismissRecomputerFake{}, sessionID: sess.ID}
	h.srv = New(Config{
		Repos:            repos,
		Sessions:         sessions,
		Logger:           zerolog.Nop(),
		StatusRecomputer: h.recomputer,
		OnSessionUpdated: func(_ context.Context, p *pb.Session) { h.published = append(h.published, p) },
	})
	return h
}

func TestDismissSessionAlert_PersistsRecomputesAndRepublishes(t *testing.T) {
	h := newDismissHarness(t)
	ctx := context.Background()

	resp, err := h.srv.DismissSessionAlert(ctx, connect.NewRequest(&pb.DismissSessionAlertRequest{
		SessionId: h.sessionID,
		AlertKey:  "resume:auth_expired",
	}))
	if err != nil {
		t.Fatalf("DismissSessionAlert: %v", err)
	}
	if got := resp.Msg.GetSession().GetId(); got != h.sessionID {
		t.Fatalf("response session id = %q, want %q", got, h.sessionID)
	}
	if got := resp.Msg.GetSession().GetRepoDisplayName(); got != "test-repo" {
		t.Errorf("response repo display name = %q, want test-repo", got)
	}

	stored, err := h.sessions.Get(ctx, h.sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.DismissedAlertKey != "resume:auth_expired" || stored.DismissedAlertAt == nil {
		t.Fatalf("stored dismissal = %q/%v, want resume:auth_expired with a time", stored.DismissedAlertKey, stored.DismissedAlertAt)
	}

	if len(h.recomputer.sessionIDs) != 1 || h.recomputer.sessionIDs[0] != h.sessionID {
		t.Errorf("recompute calls = %v, want [%s]", h.recomputer.sessionIDs, h.sessionID)
	}
	if len(h.published) != 1 || h.published[0].GetId() != h.sessionID {
		t.Fatalf("published = %v, want the dismissed session once", h.published)
	}
}

func TestDismissSessionAlert_RejectsMissingFields(t *testing.T) {
	h := newDismissHarness(t)
	cases := map[string]*pb.DismissSessionAlertRequest{
		"missing session id": {AlertKey: "resume:crash"},
		"blank session id":   {SessionId: "  ", AlertKey: "resume:crash"},
		"missing alert key":  {SessionId: h.sessionID},
		"blank alert key":    {SessionId: h.sessionID, AlertKey: " "},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := h.srv.DismissSessionAlert(context.Background(), connect.NewRequest(req))
			var cerr *connect.Error
			if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
				t.Fatalf("err = %v, want InvalidArgument", err)
			}
		})
	}
	if len(h.recomputer.sessionIDs) != 0 || len(h.published) != 0 {
		t.Fatalf("a rejected request recomputed %v / published %d", h.recomputer.sessionIDs, len(h.published))
	}
	stored, err := h.sessions.Get(context.Background(), h.sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.DismissedAlertKey != "" {
		t.Fatalf("a rejected request stored key %q", stored.DismissedAlertKey)
	}
}

func TestDismissSessionAlert_UnknownSessionIsNotFound(t *testing.T) {
	h := newDismissHarness(t)
	_, err := h.srv.DismissSessionAlert(context.Background(), connect.NewRequest(&pb.DismissSessionAlertRequest{
		SessionId: "no-such-session",
		AlertKey:  "resume:crash",
	}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeNotFound {
		t.Fatalf("err = %v, want NotFound", err)
	}
	if len(h.published) != 0 {
		t.Fatalf("an unknown session was published %d times", len(h.published))
	}
}
