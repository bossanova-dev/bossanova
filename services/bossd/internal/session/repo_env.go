package session

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// RepoForSessionEnv looks up repoID to source a session's repo-config secrets
// (LINEAR_API_KEY / SENTRY_*) for dotenv.OverlayWithRepo. It is the single home
// for the non-fatal "load the repo, or warn and carry on with nil" policy that
// every session-spawn site shares.
//
// A nil store or a failed lookup is non-fatal by design: it logs one warning
// tagged with op and returns nil, on which OverlayWithRepo's fail-clean
// guarantee still fires (LINEAR_API_KEY is emitted empty), so the daemon's
// ambient LINEAR_API_KEY can never leak into the session. It never returns an
// error — every caller proceeds with the (possibly nil) repo.
//
// The store is nil-checked because it is wired late on some hosts (the server /
// plugin repo stores are set via SetSessionDeps and may be unset early); the
// check is a harmless no-op where the store is always present.
//
// op is a short human label for the spawn path (e.g. "wake chat") used only in
// the warning. Repo secrets themselves are never logged.
func RepoForSessionEnv(ctx context.Context, repos db.RepoStore, repoID, sessionID, op string, logger zerolog.Logger) *models.Repo {
	if repos == nil {
		return nil
	}
	repo, err := repos.Get(ctx, repoID)
	if err != nil {
		logger.Warn().Err(err).Str("session_id", sessionID).Str("repo_id", repoID).
			Msg(op + ": repo lookup failed; session env omits repo-config secrets")
		return nil
	}
	return repo
}
