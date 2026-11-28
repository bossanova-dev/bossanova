package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
)

// originRows is an in-memory repo store holding one row per id.
type originRows struct {
	db.RepoStore
	rows      map[string]*models.Repo
	updateErr error
	updates   int
}

func (s *originRows) Get(_ context.Context, id string) (*models.Repo, error) {
	r, ok := s.rows[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	c := *r
	return &c, nil
}

func (s *originRows) Update(_ context.Context, id string, p db.UpdateRepoParams) (*models.Repo, error) {
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	s.updates++
	if p.OriginURL != nil {
		s.rows[id].OriginURL = *p.OriginURL
	}
	c := *s.rows[id]
	return &c, nil
}

func TestRefreshRepoOrigin(t *testing.T) {
	const (
		stale   = "https://github.com/old-owner/repo.git"
		renamed = "https://github.com/new-owner/repo.git"
	)
	tests := []struct {
		name         string
		id           string
		remote       string
		updateErr    error
		wantCode     connect.Code
		wantPrevious string
		wantStored   string
		wantUpdates  int
	}{
		{name: "renamed remote is persisted", id: "r1", remote: "https://bob:tok3n-value@github.com/new-owner/repo.git", wantPrevious: stale, wantStored: renamed, wantUpdates: 1},
		{name: "matching remote writes nothing", id: "r1", remote: stale, wantPrevious: stale, wantStored: stale},
		{name: "missing remote keeps the stored origin", id: "r1", wantCode: connect.CodeFailedPrecondition, wantStored: stale},
		{name: "local-path remote keeps a hosted origin", id: "r1", remote: "/src/mirror/origin.git", wantCode: connect.CodeFailedPrecondition, wantStored: stale},
		{name: "origin owned by another repo is refused", id: "r1", remote: renamed, updateErr: domain.ErrAmbiguousOrigin, wantCode: connect.CodeFailedPrecondition, wantStored: stale},
		{name: "repo removed before the write", id: "r1", remote: renamed, updateErr: domain.ErrNotFound, wantCode: connect.CodeNotFound, wantStored: stale},
		{name: "unknown repo", id: "nope", remote: renamed, wantCode: connect.CodeNotFound, wantStored: stale},
		{name: "empty id", remote: renamed, wantCode: connect.CodeInvalidArgument, wantStored: stale},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repos := &originRows{rows: map[string]*models.Repo{"r1": {ID: "r1", LocalPath: "/src/repo", OriginURL: stale}}, updateErr: tc.updateErr}
			s := New(Config{Repos: repos, Worktrees: &repoMgmtWorktree{originURL: tc.remote}})

			resp, err := s.RefreshRepoOrigin(context.Background(), connect.NewRequest(&pb.RefreshRepoOriginRequest{Id: tc.id}))
			if tc.wantCode != 0 {
				if connect.CodeOf(err) != tc.wantCode {
					t.Fatalf("err = %v, want code %v", err, tc.wantCode)
				}
			} else {
				if err != nil {
					t.Fatalf("RefreshRepoOrigin: %v", err)
				}
				if resp.Msg.GetPreviousOriginUrl() != tc.wantPrevious || resp.Msg.GetRepo().GetOriginUrl() != tc.wantStored {
					t.Fatalf("response previous=%q origin=%q, want %q and %q", resp.Msg.GetPreviousOriginUrl(), resp.Msg.GetRepo().GetOriginUrl(), tc.wantPrevious, tc.wantStored)
				}
			}
			if got := repos.rows["r1"].OriginURL; got != tc.wantStored {
				t.Fatalf("stored origin = %q, want %q", got, tc.wantStored)
			}
			if repos.updates != tc.wantUpdates {
				t.Fatalf("writes = %d, want %d", repos.updates, tc.wantUpdates)
			}
		})
	}
}
