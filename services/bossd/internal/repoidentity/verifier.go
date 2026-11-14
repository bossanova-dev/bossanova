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
	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/detach"
	"github.com/recurser/bossd/internal/domain"
)

const (
	identityTTL  = 10 * time.Minute
	fetchTimeout = 5 * time.Second
	// identityRefreshAhead is how long before expiry a lookup that finds a still
	// valid identity renews it in the background. Without it, the first lookup
	// after expiry waited on GitHub (0.5-0.9 s measured, up to fetchTimeout),
	// and the terminal writer asks on every output chunk, so every active
	// terminal on the machine stalled once per repository every ten minutes.
	// The renewal keeps a repository that is in use from ever expiring; one
	// left idle past its expiry still waits for a fresh read, as before.
	identityRefreshAhead = 2 * time.Minute
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
	// renewals holds the background renewal running for each origin, so a burst
	// of lookups inside the renewal window starts one renewal, not one each.
	renewals map[string]<-chan struct{}
}

// New constructs a verifier for a locally credentialed provider.
func New(reader Reader, logger zerolog.Logger) *Verifier {
	return &Verifier{reader: reader, logger: logger, now: time.Now, cache: make(map[string]entry), renewals: make(map[string]<-chan struct{})}
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
// A valid one is returned at once; inside its last identityRefreshAhead it is
// also renewed in the background, so a repository in use never expires.
func (v *Verifier) Identity(ctx context.Context, repo domain.Repo) config.RepoIdentity {
	id, origin := localIdentity(repo)
	if id.Host != "github.com" || v.reader == nil || ctx.Err() != nil {
		return id
	}
	if cached, ok := v.valid(origin); ok {
		if !v.now().Before(cached.expiresAt.Add(-identityRefreshAhead)) {
			v.renew(origin)
		}
		return withVerified(id, cached)
	}
	verified, err := detach.Flight(ctx, &v.group, v.logger, origin, fetchTimeout, v.fetcher(origin))
	if err != nil || ctx.Err() != nil {
		return id
	}
	if verified.repoID > 0 && v.now().Before(verified.expiresAt) {
		return withVerified(id, verified)
	}
	return id
}

func withVerified(id config.RepoIdentity, verified entry) config.RepoIdentity {
	id.ProviderRepoID = verified.repoID
	id.IsIDVerified = true
	id.Visibility = verified.visibility
	return id
}

// valid returns the cached identity for origin when it has not expired.
func (v *Verifier) valid(origin string) (entry, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	cached, ok := v.cache[origin]
	return cached, ok && cached.repoID > 0 && v.now().Before(cached.expiresAt)
}

// renew starts one background renewal of origin's identity unless one is
// already running. A failed renewal leaves the cached identity to expire.
func (v *Verifier) renew(origin string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if running, ok := v.renewals[origin]; ok {
		select {
		case <-running:
		default:
			return
		}
	}
	v.renewals[origin] = safego.Go(v.logger, func() {
		if _, err := detach.Flight(context.Background(), &v.group, v.logger, origin, fetchTimeout, v.fetcher(origin)); err != nil {
			v.logger.Debug().Err(err).Str("origin", origin).Msg("repository identity renewal failed")
		}
	})
}

// fetcher reads origin's identity from the provider unless the cache already
// holds one outside its renewal window, the answer a flight joined late gets.
func (v *Verifier) fetcher(origin string) func(context.Context) (entry, error) {
	return func(fetchCtx context.Context) (entry, error) {
		v.mu.Lock()
		cached, ok := v.cache[origin]
		if ok && v.now().Before(cached.expiresAt.Add(-identityRefreshAhead)) {
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
	}
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
