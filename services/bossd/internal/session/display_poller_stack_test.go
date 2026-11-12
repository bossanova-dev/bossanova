package session

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

type recordingStackRefresher struct{ calls []string }

func (r *recordingStackRefresher) Refresh(_ context.Context, repo *models.Repo, sessions []*models.Session) {
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	slices.Sort(ids)
	r.calls = append(r.calls, repo.ID+": "+strings.Join(ids, " "))
}

// Each poll cycle hands every repository's listed sessions, with or without
// a PR, to the stack refresher once.
func TestDisplayPoller_RefreshesStacksOncePerRepo(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"}
	pr1, pr2 := 1, 2
	repos.repos["repo-a"] = &models.Repo{ID: "repo-a", OriginURL: "owner/a"}
	repos.repos["repo-b"] = &models.Repo{ID: "repo-b", OriginURL: "owner/b"}
	sessions.sessions["a1"] = &models.Session{ID: "a1", RepoID: "repo-a", PRNumber: &pr1}
	sessions.sessions["a2"] = &models.Session{ID: "a2", RepoID: "repo-a"}
	sessions.sessions["b1"] = &models.Session{ID: "b1", RepoID: "repo-b", PRNumber: &pr2}
	poller := NewDisplayPoller(sessions, repos, vp, status.NewDisplayTracker(), time.Minute, zerolog.Nop())
	got := &recordingStackRefresher{}
	poller.SetStackRefresher(got)

	poller.poll(t.Context())

	slices.Sort(got.calls)
	if want := []string{"repo-a: a1 a2", "repo-b: b1"}; !slices.Equal(got.calls, want) {
		t.Fatalf("stack refreshes = %q; want %q", got.calls, want)
	}
}
