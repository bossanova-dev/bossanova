// Package repoorigin keeps a registered repository's stored origin in step
// with its checkout's origin remote.
package repoorigin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/domain"
)

// ErrNoRemote means the checkout has no usable origin remote, so the stored
// origin was left as it was.
var ErrNoRemote = errors.New("no origin remote configured")

// ErrUnhostedRemote means the origin remote names no hosted repository (a
// local path or bare mirror, say) while the stored origin does. A rename or
// transfer never does that, and following it would break every provider call,
// so the stored origin was left as it was.
var ErrUnhostedRemote = errors.New("origin remote names no hosted repository")

// Detector reads a checkout's origin remote URL; "" means there is none.
type Detector interface {
	DetectOriginURL(ctx context.Context, repoPath string) (string, error)
}

// Store persists a repository's origin.
type Store interface {
	Update(ctx context.Context, id string, params domain.UpdateRepoParams) (*domain.Repo, error)
}

// Repos lists registered repositories and persists their origins.
type Repos interface {
	Store
	List(ctx context.Context) ([]*domain.Repo, error)
}

// Refresh persists the checkout's origin remote, credentials stripped, when it
// differs from repo.OriginURL, and then replaces *repo with the stored row. It
// returns the origin stored before the call. A missing or empty remote changes
// nothing and returns ErrNoRemote; an unhosted remote replacing a hosted origin
// changes nothing and returns ErrUnhostedRemote.
func Refresh(ctx context.Context, detector Detector, store Store, repo *domain.Repo, logger zerolog.Logger) (string, error) {
	previous := repo.OriginURL
	remote, err := detector.DetectOriginURL(ctx, repo.LocalPath)
	if err != nil {
		return previous, fmt.Errorf("detect origin URL: %w", err)
	}
	remote = domain.CloneURLWithoutCredentials(strings.TrimSpace(remote))
	if remote == "" {
		return previous, ErrNoRemote
	}
	if remote == previous {
		return previous, nil
	}
	if vcs.NormalizeRepoURL(remote) == "" && vcs.NormalizeRepoURL(previous) != "" {
		return previous, ErrUnhostedRemote
	}
	updated, err := store.Update(ctx, repo.ID, domain.UpdateRepoParams{OriginURL: &remote})
	if err != nil {
		return previous, fmt.Errorf("persist origin URL: %w", err)
	}
	*repo = *updated
	logger.Info().
		Str("repo", repo.ID).
		Str("previous_origin_url", domain.RedactURLCredentials(previous)).
		Str("origin_url", domain.RedactURLCredentials(remote)).
		Msg("persisted repo origin from its origin remote")
	return previous, nil
}

// refreshTimeout bounds one repository's refresh, so a checkout whose git
// call hangs (a slow mount or a blocked protected folder) cannot use up the
// whole pass.
const refreshTimeout = 2 * time.Second

// RefreshAll runs Refresh for every registered repository. A repository whose
// remote is missing, or whose refresh fails, keeps its stored origin; failures
// are logged and do not stop the others.
func RefreshAll(ctx context.Context, detector Detector, repos Repos, logger zerolog.Logger) {
	all, err := repos.List(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("repo origin refresh: list repos")
		return
	}
	for i, repo := range all {
		if ctx.Err() != nil {
			logger.Warn().Err(ctx.Err()).Int("unchecked", len(all)-i).Msg("repo origin refresh stopped; remaining repos keep their stored origin")
			return
		}
		repoCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
		_, err := Refresh(repoCtx, detector, repos, repo, logger)
		timedOut := repoCtx.Err() != nil
		cancel()
		switch {
		case err == nil:
		case timedOut:
			logger.Warn().Err(err).Str("repo", repo.ID).Msg("repo origin refresh timed out; stored origin kept")
		case errors.Is(err, ErrNoRemote), errors.Is(err, ErrUnhostedRemote):
			logger.Debug().Str("repo", repo.ID).Err(err).Msg("repo origin refresh: stored origin kept")
		default:
			logger.Warn().Err(err).Str("repo", repo.ID).Msg("repo origin refresh failed; stored origin kept")
		}
	}
}
