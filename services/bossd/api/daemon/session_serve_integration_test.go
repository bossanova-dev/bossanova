package server

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/serve"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

type serveRPCSessionStore struct {
	sessionStore
	row *models.Session
}

func (f serveRPCSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	if id != f.row.ID {
		return nil, domain.ErrNotFound
	}
	return f.row, nil
}
func (f serveRPCSessionStore) List(context.Context, string) ([]*models.Session, error) {
	return []*models.Session{f.row}, nil
}

type serveRPCRepoStore struct {
	repoStore
	row *models.Repo
}

func (f serveRPCRepoStore) Get(context.Context, string) (*models.Repo, error) { return f.row, nil }

// Execute the configured command in place of a tmux pane, retaining fake pane liveness.
type serveRPCPane struct{ live bool }

func (f *serveRPCPane) NewSession(ctx context.Context, o tmux.NewSessionOpts) error {
	cmd := exec.CommandContext(ctx, o.Command[0], o.Command[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Dir = o.WorkDir
	cmd.Env = os.Environ()
	for k, v := range o.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := cmd.Run(); err != nil {
		return err
	}
	f.live = true
	return nil
}
func (f *serveRPCPane) HasSessionStatus(context.Context, string) (bool, error)   { return f.live, nil }
func (f *serveRPCPane) KillSession(context.Context, string) error                { f.live = false; return nil }
func (f *serveRPCPane) KillPaneProcessGroup(context.Context, string) error       { return nil }
func (f *serveRPCPane) ListSessions(context.Context) ([]tmux.LiveSession, error) { return nil, nil }
func (f *serveRPCPane) ShowEnv(context.Context, string, string) (string, tmux.ShowEnvStatus) {
	return "", tmux.ShowEnvUnset
}

func TestServeRPCPublishesSessionLinks_Sessions_AC250803(t *testing.T) {
	script := `{"type":"command","argv":["sh","-c","printf '%s' '{\"api\":\"https://example.com/api\"}' > \"$BOSS_SERVICES_FILE\""]}`
	sess := &models.Session{ID: "bbbbbbbb11111111", RepoID: "aaaaaaaa11111111", WorktreePath: t.TempDir()}
	sessions := serveRPCSessionStore{row: sess}
	repos := serveRPCRepoStore{row: &models.Repo{ID: sess.RepoID, LocalPath: sess.WorktreePath, ServeScript: &script}}
	tracker := status.NewDisplayTracker()
	runner := serve.New(sessions, repos, &serveRPCPane{}, tracker, serve.Config{DataDir: t.TempDir()})
	s := New(Config{Sessions: sessions, Repos: repos, DisplayTracker: tracker, SessionServeRunner: runner})
	// The real runner launches the configured process through the fake pane above.
	if _, err := s.StartSessionServe(t.Context(), connect.NewRequest(&pb.StartSessionServeRequest{SessionId: sess.ID})); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		published := make(chan struct{}, 1)
		tracker.SetOnAttentionChange(func(string) {
			select {
			case published <- struct{}{}:
			default:
			}
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := safego.Go(zerolog.Nop(), func() { runner.Run(ctx) })
		<-published
		cancel()
		<-done
		tracker.SetOnAttentionChange(nil)
	})
	response, err := s.GetSession(t.Context(), connect.NewRequest(&pb.GetSessionRequest{Id: sess.ID}))
	if err != nil {
		t.Fatal(err)
	}
	links := response.Msg.Session.ServiceLinks
	if len(links) != 1 || links[0].Name != "api" || links[0].Url != "https://example.com/api" {
		t.Fatalf("links=%v", links)
	}
	if _, err = runner.Get(t.Context(), "unknown"); err != domain.ErrNotFound {
		t.Fatalf("unknown=%v", err)
	}
	if _, err = s.StopSessionServe(t.Context(), connect.NewRequest(&pb.StopSessionServeRequest{SessionId: sess.ID})); err != nil {
		t.Fatal(err)
	}
	response, err = s.GetSession(t.Context(), connect.NewRequest(&pb.GetSessionRequest{Id: sess.ID}))
	if err != nil || len(response.Msg.Session.ServiceLinks) != 0 {
		t.Fatalf("stop links=%v err=%v", response, err)
	}
}
