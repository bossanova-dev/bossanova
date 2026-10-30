package taskorchestrator

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

func TestCreateSession_DetachHookToken_Sessions_AC128(t *testing.T) {
	for _, tc := range []struct {
		name   string
		detach bool
		token  string
	}{
		{name: "mint for detached plan", detach: true},
		{name: "preserve supplied token", detach: true, token: "caller-token"},
		{name: "interactive stays idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &mockSessionStore{
				createFn: func(_ context.Context, p db.CreateSessionParams) (*models.Session, error) {
					return &models.Session{ID: "sess-detach", RepoID: p.RepoID}, nil
				},
				getFn: func(_ context.Context, id string) (*models.Session, error) { return &models.Session{ID: id}, nil },
			}
			var got session.StartSessionOpts
			starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, opts session.StartSessionOpts) error { got = opts; return nil }}
			creator := NewSessionCreator(store, starter, "claude", zerolog.Nop())
			_, err := creator.CreateSession(context.Background(), CreateSessionOpts{RepoID: "repo-1", Title: "repair", Detach: tc.detach, HookToken: tc.token})
			if err != nil {
				t.Fatal(err)
			}
			if got.Detach != tc.detach {
				t.Errorf("Detach = %v, want %v", got.Detach, tc.detach)
			}
			if tc.detach && tc.token == "" {
				raw, err := hex.DecodeString(got.HookToken)
				if err != nil || len(raw) != 32 {
					t.Errorf("HookToken = %q, want 64 hex characters", got.HookToken)
				}
			} else if got.HookToken != tc.token {
				t.Errorf("HookToken = %q, want %q", got.HookToken, tc.token)
			}
		})
	}
}
