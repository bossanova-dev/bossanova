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
	visibility string
	repoID     int64
	expiresAt  time.Time
}

// Verifier caches successful reads from the local GitHub adapter.
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

func localIdentity(repo domain.Repo) (config.RepoIdentity, string) {
	id := config.RepoIdentity{LocalPath: repo.LocalPath, Visibility: "unknown"}
	origin := vcs.CanonicalRepoURL(repo.OriginURL)
	parsed, err := url.Parse(origin)
	if err != nil || origin == "" {
		return id, ""
	}
	id.Host = parsed.Host
	id.Name = strings.TrimPrefix(parsed.Path, "/")
	return id, origin
}

// LastVerifiedIdentity returns local metadata and the last successful provider ID.
// It is only for matching local policy changes on idle terminals. Active content
// and control require Identity, which refreshes expired verification.
// This method never starts or waits for a provider lookup.
func (v *Verifier) LastVerifiedIdentity(repo domain.Repo) config.RepoIdentity {
	id, origin := localIdentity(repo)
	v.mu.Lock()
	defer v.mu.Unlock()
	cached := v.cache[origin]
	if cached.repoID > 0 {
		id.ProviderRepoID = cached.repoID
		id.IsIDVerified = true
	}
	return id
}

// Identity combines the registered repo row with a freshly verified GitHub identity.
// An expired identity is never returned while its replacement is being fetched.
func (v *Verifier) Identity(ctx context.Context, repo domain.Repo) config.RepoIdentity {
	id, origin := localIdentity(repo)
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
		v.mu.Unlock()
		started := v.now()
		repoID, visibility, readErr := v.reader.GetRepositoryIdentity(fetchCtx, origin)
		if readErr != nil {
			return entry{}, readErr
		}
		if fetchCtx.Err() != nil {
			return entry{}, fetchCtx.Err()
		}
		fresh := entry{repoID: repoID, visibility: normalizeVisibility(visibility), expiresAt: started.Add(identityTTL)}
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
		id.Visibility = verified.visibility
	}
	return id
}

func normalizeVisibility(raw string) string {
	switch raw {
	case "public", "internal":
		return "public"
	case "private":
		return "private"
	default:
		return "unknown"
	}
}
