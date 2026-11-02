package ghtoken

import (
	"context"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
)

// Report returns one entry per local repository: which credential it uses and
// why. It reads only cached state, so it never calls bosso and never mints a
// token; a signed-in repository nothing has asked about reports
// not_requested.
func (s *Source) Report(ctx context.Context) ([]RepoCredentialState, error) {
	repos, err := s.cfg.Repos.List(ctx)
	if err != nil {
		return nil, err
	}
	signedIn := s.signedIn()
	out := make([]RepoCredentialState, 0, len(repos))
	for _, r := range repos {
		origin := vcs.NormalizeRepoURL(r.OriginURL)
		st := RepoCredentialState{RepoID: r.ID, Origin: origin, Identity: IdentityUser}
		switch {
		case origin == "" || !vcs.IsGitHubURL(origin):
			st.Reason = ReasonNotGitHub
		case !signedIn:
			st.Reason = ReasonNotSignedIn
		default:
			s.fillReport(&st, origin)
		}
		out = append(out, st)
	}
	return out, nil
}

// fillReport copies origin's cached state into st without the token.
func (s *Source) fillReport(st *RepoCredentialState, origin string) {
	st.Reason = ReasonNotRequested
	e := s.lookup(origin)
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.state.attempted {
		return
	}
	now := s.cfg.Now()
	c := e.state.answer(now)
	st.NextAttemptAt = e.state.nextAttemptAt
	if c.Identity == IdentityInstallation {
		st.Identity = c.Identity
		st.InstallationID = c.InstallationID
		st.AccountLogin = c.AccountLogin
		st.ExpiresAt = c.ExpiresAt
		st.Reason = c.Reason
		if st.NextAttemptAt.IsZero() {
			st.NextAttemptAt = c.ExpiresAt.Add(-refreshMargin)
		}
		return
	}
	if e.state.reason == ReasonInstallation && !now.Before(e.state.nextAttemptAt) {
		// The token expired and nothing has asked since; the next request
		// refreshes it.
		st.NextAttemptAt = time.Time{}
		return
	}
	st.Reason = c.Reason
}
