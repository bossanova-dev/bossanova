// Package repoidentity verifies registered repositories using local provider credentials.
package repoidentity

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/detach"
	"github.com/recurser/bossd/internal/domain"
)

const (
	identityTTL  = 10 * time.Minute
	fetchTimeout = 5 * time.Second
)

// Reader is an optional provider capability, separate from vcs.Provider.
type Reader interface {
	GetRepositoryIdentity(context.Context, string) (int64, string, error)
}

type entry struct {
	repoID    int64
	expiresAt time.Time
}

// Verifier caches only successful, unexpired reads from the local GitHub adapter.
// It does not accept provider identities from cloud payloads.
type Verifier struct {
	reader Reader
	now    func() time.Time
	mu     sync.Mutex
	cache  map[string]entry
	group  detach.Group
	logger zerolog.Logger
}

// New constructs a verifier for a locally credentialed provider.
func New(reader Reader, logger zerolog.Logger) *Verifier {
	return &Verifier{reader: reader, logger: logger, now: time.Now, cache: make(map[string]entry)}
}

// Identity combines the registered repo row with a freshly verified GitHub identity.
// An expired identity is never returned while its replacement is being fetched.
func (v *Verifier) Identity(ctx context.Context, repo domain.Repo) config.RepoIdentity {
	id := config.RepoIdentity{LocalPath: repo.LocalPath}
	origin := vcs.CanonicalRepoURL(repo.OriginURL)
	parsed, err := url.Parse(origin)
	if err != nil || origin == "" {
		return id
	}
	id.Host = parsed.Host
	id.Name = strings.TrimPrefix(parsed.Path, "/")
	if id.Host != "github.com" || v.reader == nil || ctx.Err() != nil {
		return id
	}
	verified, err := detach.Flight(ctx, &v.group, v.logger, origin, fetchTimeout, func(fetchCtx context.Context) (entry, error) {
		v.mu.Lock()
		cached, ok := v.cache[origin]
		if ok && v.now().Before(cached.expiresAt) {
			v.mu.Unlock()
			return cached, nil
		}
		delete(v.cache, origin)
		v.mu.Unlock()
		started := v.now()
		repoID, _, readErr := v.reader.GetRepositoryIdentity(fetchCtx, origin)
		if readErr != nil {
			return entry{}, readErr
		}
		if fetchCtx.Err() != nil {
			return entry{}, fetchCtx.Err()
		}
		fresh := entry{repoID: repoID, expiresAt: started.Add(identityTTL)}
		if repoID <= 0 || !v.now().Before(fresh.expiresAt) {
			return entry{}, nil
		}
		v.mu.Lock()
		v.cache[origin] = fresh
		v.mu.Unlock()
		return fresh, nil
	})
	if err != nil || ctx.Err() != nil {
		return id
	}
	if verified.repoID > 0 && v.now().Before(verified.expiresAt) {
		id.ProviderRepoID = verified.repoID
		id.IsIDVerified = true
	}
	return id
}
