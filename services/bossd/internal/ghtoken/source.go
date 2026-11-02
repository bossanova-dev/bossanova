// Package ghtoken keeps bossd's per-repository GitHub credential: the Boss
// GitHub App installation token bosso mints through
// IssueGitHubInstallationToken when the daemon is signed in, and the user's
// own gh login otherwise (BOS-2264).
//
// The source holds tokens in memory only and never persists, logs or wraps
// one into an error. Every failure answers with the user's login and a
// Reason, because that fallback is the contract: a caller never has to
// handle an error to keep reaching GitHub.
package ghtoken

import (
	"context"
	"errors"
	"maps"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/notesync"
)

const (
	// MaxRetryInterval bounds every wait before the source asks bosso again,
	// jitter included. Refusals wait exactly this long.
	MaxRetryInterval = 10 * time.Minute
	// DefaultCallTimeout bounds one RPC attempt and the re-register.
	DefaultCallTimeout = 10 * time.Second

	// refreshMargin is how long before expires_at a cached token is
	// refreshed. It stays below bosso's 5-minute re-mint margin, so each
	// refresh lands on a fresh bosso mint rather than bosso's cached token.
	refreshMargin = 3 * time.Minute
	// staleGuard is how close to expiry a token may get and still be served.
	staleGuard = 30 * time.Second
	// errorBackoffBase is the first wait after an upstream_error; it doubles
	// per consecutive failure.
	errorBackoffBase = 30 * time.Second
	// minAttemptGap is the floor on the next attempt after bosso returns a
	// token already inside refreshMargin (clock skew), so it cannot hot-loop.
	minAttemptGap = 30 * time.Second
)

// errReRegisterFailed marks an Unauthenticated call whose self-heal failed.
var errReRegisterFailed = errors.New("daemon re-register failed")

// Client sends one IssueGitHubInstallationToken request for repoOriginURL
// with sessionToken as the Authorization bearer, and returns bosso's answer or
// its connect error unchanged. The protobuf adapter lives at the API edge
// (services/bossd/cmd/github_credentials_adapter.go), so this package owns
// only the domain shape.
type Client interface {
	IssueInstallationToken(ctx context.Context, sessionToken, repoOriginURL string) (MintedToken, error)
}

// MintedToken is one installation token bosso minted. Its String and GoString
// omit the token.
type MintedToken struct {
	Token          string
	ExpiresAt      time.Time
	InstallationID int64
	AccountLogin   string
	// Permissions is what GitHub granted, permission name to access level.
	Permissions map[string]string
}

// String renders the minted token's metadata without the token.
func (m MintedToken) String() string {
	return "ghtoken.MintedToken{InstallationID:" + strconv.FormatInt(m.InstallationID, 10) +
		" AccountLogin:" + m.AccountLogin + " ExpiresAt:" + m.ExpiresAt.Format(time.RFC3339) + "}"
}

// GoString renders the minted token's metadata without the token, for %#v.
func (m MintedToken) GoString() string { return m.String() }

// TokenHolder is the shared daemon session token. upstream.SessionTokenHolder
// satisfies it.
type TokenHolder interface {
	Get() string
	Set(tok string)
	CompareAndSwap(old, tok string) bool
}

// Repos lists the daemon's local repositories. db.RepoStore satisfies it.
type Repos interface {
	List(ctx context.Context) ([]*models.Repo, error)
}

// Config wires a Source. A nil Client is the local-only daemon: every answer
// is the user's login. Repos is used only by Report.
type Config struct {
	Client Client
	Tokens TokenHolder
	// ReRegister is the shared daemon re-register function. On an
	// Unauthenticated call the source calls it once, adopts the fresh token
	// and retries; nil disables the self-heal.
	ReRegister func(context.Context) (string, error)
	// NeedsLogin reports that the daemon's sign-in is gone (a logout, or a
	// refresh token the identity provider rejected). While it is true no
	// token is minted or served; nil means never.
	NeedsLogin func() bool
	Repos      Repos
	Logger     zerolog.Logger

	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Jitter returns a random duration in [0, limit); nil uses math/rand.
	Jitter      func(limit time.Duration) time.Duration
	CallTimeout time.Duration
}

// Source answers which GitHub credential a repository uses right now.
type Source struct {
	cfg Config

	// reRegisterSlot is a one-slot semaphore held across one re-register, so
	// concurrent rejections of the same session token issue one new token.
	reRegisterSlot chan struct{}

	mu      sync.Mutex
	entries map[string]*entry
}

// entry is one canonical origin. fetch is a one-slot semaphore held across
// that repository's RPC, so concurrent callers share one fetch and other
// repositories never wait; a caller whose context ends while it waits answers
// from the cache. mu guards state for the short reads Report makes.
type entry struct {
	fetch chan struct{}
	mu    sync.Mutex
	state entryState
	// logged is the last identity, installation and reason logged at Info.
	logged Credential
}

// New builds a Source, filling every unset optional Config field.
func New(cfg Config) *Source {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Jitter == nil {
		cfg.Jitter = func(limit time.Duration) time.Duration {
			if limit <= 0 {
				return 0
			}
			return rand.N(limit) // #nosec G404 -- retry jitter, not a secret
		}
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = DefaultCallTimeout
	}
	return &Source{cfg: cfg, entries: map[string]*entry{}, reRegisterSlot: make(chan struct{}, 1)}
}

// Credential returns the credential repoOriginURL uses now. It never fails:
// every refusal or error is the user's login with a Reason.
func (s *Source) Credential(ctx context.Context, repoOriginURL string) Credential {
	origin := vcs.NormalizeRepoURL(repoOriginURL)
	if origin == "" || !vcs.IsGitHubURL(origin) {
		return userCredential(ReasonNotGitHub)
	}
	if !s.signedIn() {
		// No backoff is set, so a later login takes effect on the next call.
		return userCredential(ReasonNotSignedIn)
	}
	e := s.entry(origin)
	answer := s.fetchOrCached(ctx, origin, e)
	if !s.signedIn() || s.lookup(origin) != e {
		// A logout or Reset landed while this call fetched: nothing minted
		// for the previous sign-in is served.
		return userCredential(ReasonNotSignedIn)
	}
	s.logChange(origin, e, answer)
	return answer
}

// fetchOrCached answers from e's cache, refreshing it first when it is due.
// Only the caller holding e.fetch refreshes; a caller whose context ends while
// it waits for that slot answers from the cache without an RPC.
func (s *Source) fetchOrCached(ctx context.Context, origin string, e *entry) Credential {
	select {
	case e.fetch <- struct{}{}:
	case <-ctx.Done():
		return e.cached(s.cfg.Now())
	}
	defer func() { <-e.fetch }()

	now := s.cfg.Now()
	e.mu.Lock()
	skip := e.state.fresh(now) || now.Before(e.state.nextAttemptAt)
	answer := e.state.answer(now)
	e.mu.Unlock()
	if skip {
		return answer
	}
	return s.refresh(ctx, origin, e)
}

// cached is e's answer at now with no RPC.
func (e *entry) cached(now time.Time) Credential {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.answer(now)
}

// Reset drops every cached token, backoff and logged state, so nothing minted
// for the previous sign-in is served after a logout or an account change. The
// next Credential call for a repository mints afresh when signed in.
func (s *Source) Reset() {
	s.mu.Lock()
	dropped := len(s.entries)
	s.entries = map[string]*entry{}
	s.mu.Unlock()
	s.cfg.Logger.Info().Int("repositories", dropped).
		Msg("github credentials: cleared cached installation tokens after a sign-in change")
}

// Invalidate drops repoOriginURL's cached installation token when it is still
// token, the one GitHub rejected, so the next Credential call mints afresh
// instead of serving it until it expires. bosso may hand the same token back
// from its own cache; store refuses it, so the repository waits out the
// upstream_error backoff on the user's login rather than re-minting on every
// call.
func (s *Source) Invalidate(repoOriginURL, token string) {
	e := s.lookup(vcs.NormalizeRepoURL(repoOriginURL))
	if e == nil || token == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.token != token {
		return
	}
	e.state.rejected = token
	e.state.token = ""
	e.state.expiresAt = time.Time{}
	e.state.nextAttemptAt = time.Time{}
	s.cfg.Logger.Warn().Str("repo_origin", vcs.NormalizeRepoURL(repoOriginURL)).
		Int64("installation_id", e.state.installationID).
		Msg("github credentials: GitHub rejected the installation token; minting a new one")
}

func (s *Source) signedIn() bool {
	if s.cfg.Client == nil || s.sessionToken() == "" {
		return false
	}
	return s.cfg.NeedsLogin == nil || !s.cfg.NeedsLogin()
}

func (s *Source) sessionToken() string {
	if s.cfg.Tokens == nil {
		return ""
	}
	return s.cfg.Tokens.Get()
}

func (s *Source) entry(origin string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[origin]
	if !ok {
		e = &entry{fetch: make(chan struct{}, 1)}
		s.entries[origin] = e
	}
	return e
}

func (s *Source) lookup(origin string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[origin]
}

// refresh asks bosso for origin's token and records the outcome. The caller
// holds e.fetch.
func (s *Source) refresh(ctx context.Context, origin string, e *entry) Credential {
	minted, err := s.call(ctx, origin)
	now := s.cfg.Now()
	if err == nil {
		err = s.store(e, minted, now)
	}
	if err != nil && ctx.Err() != nil {
		// The caller gave up; that says nothing about the repository, so no
		// backoff is recorded.
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.state.answer(now)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state.attempted = true
	if err != nil {
		reason := reasonFor(err)
		s.cfg.Logger.Warn().Str("repo_origin", origin).Str("code", codeFor(err)).Str("reason", string(reason)).
			Msg("github credentials: IssueGitHubInstallationToken failed")
		e.state.reason = reason
		e.state.nextAttemptAt = now.Add(s.retryWait(reason, &e.state))
	}
	return e.state.answer(now)
}

// store caches a minted token. A response without a token, whose token is
// already inside staleGuard, or that repeats the token GitHub rejected, is an
// upstream error and is not cached.
func (s *Source) store(e *entry, m MintedToken, now time.Time) error {
	expiresAt := m.ExpiresAt
	if m.Token == "" || expiresAt.Sub(now) <= staleGuard {
		return errUnusableToken
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if m.Token == e.state.rejected {
		return errRejectedToken
	}
	e.state = entryState{
		rejected:       e.state.rejected,
		attempted:      true,
		token:          m.Token,
		installationID: m.InstallationID,
		accountLogin:   m.AccountLogin,
		permissions:    maps.Clone(m.Permissions),
		expiresAt:      expiresAt,
		reason:         ReasonInstallation,
	}
	if expiresAt.Sub(now) <= refreshMargin+minAttemptGap {
		e.state.nextAttemptAt = now.Add(minAttemptGap)
	}
	return nil
}

var (
	errUnusableToken = errors.New("bosso returned an unusable installation token")
	errRejectedToken = errors.New("bosso returned the installation token GitHub rejected")
)

// call issues the RPC with the daemon session token, each attempt under its
// own CallTimeout. On Unauthenticated it retries once with a rotated token:
// one another caller already installed, or else one it re-registers for.
func (s *Source) call(ctx context.Context, origin string) (MintedToken, error) {
	attempt := func(token string) (MintedToken, error) {
		callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
		defer cancel()
		return s.cfg.Client.IssueInstallationToken(callCtx, token, origin)
	}
	failed := s.sessionToken()
	minted, err := attempt(failed)
	if connect.CodeOf(err) != connect.CodeUnauthenticated || s.cfg.ReRegister == nil {
		return minted, err
	}
	retry, ok := s.rotatedToken(ctx, failed)
	if !ok {
		return MintedToken{}, errReRegisterFailed
	}
	return attempt(retry)
}

// rotatedToken returns the session token to retry with after bosso rejected
// failed. Every re-register issues a new daemon token and bosso keeps one per
// daemon, so a caller re-registering for a token someone already replaced
// would revoke that replacement. Re-registers are therefore serialized, and a
// caller that finds the holder already moved past failed reuses it.
func (s *Source) rotatedToken(ctx context.Context, failed string) (string, bool) {
	select {
	case s.reRegisterSlot <- struct{}{}:
	case <-ctx.Done():
		return "", false
	}
	defer func() { <-s.reRegisterSlot }()
	if current := s.sessionToken(); current != "" && current != failed {
		return current, true
	}
	fresh, ok := s.runReRegister(ctx)
	if !ok {
		return "", false
	}
	retry, _ := notesync.AdoptReRegisteredToken(s.cfg.Tokens, failed, fresh)
	return retry, true
}

// runReRegister runs the shared self-heal under its own CallTimeout.
func (s *Source) runReRegister(ctx context.Context) (string, bool) {
	regCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()
	fresh, err := s.cfg.ReRegister(regCtx)
	if err != nil || fresh == "" {
		s.cfg.Logger.Warn().Bool("empty_token", err == nil).
			Msg("github credentials: re-register after auth rejection failed")
		return "", false
	}
	return fresh, true
}

// reasonFor maps a failed fetch to its Reason.
func reasonFor(err error) Reason {
	if errors.Is(err, errReRegisterFailed) {
		return ReasonNotSignedIn
	}
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		return ReasonNotSignedIn
	case connect.CodePermissionDenied:
		return ReasonNoInstallation
	case connect.CodeFailedPrecondition:
		return ReasonAppUnavailable
	case connect.CodeUnimplemented:
		return ReasonUpstreamUnsupported
	default:
		return ReasonUpstreamError
	}
}

// codeFor names a failure for the log without its message or body.
func codeFor(err error) string {
	switch {
	case errors.Is(err, errReRegisterFailed):
		return "reregister_failed"
	case errors.Is(err, errUnusableToken):
		return "unusable_token"
	case errors.Is(err, errRejectedToken):
		return "rejected_token"
	default:
		return connect.CodeOf(err).String()
	}
}

// retryWait is how long a failed repository waits before the next attempt.
// Refusals wait MaxRetryInterval; upstream errors back off from
// errorBackoffBase, doubling per consecutive failure, plus jitter. Every wait
// is clamped to MaxRetryInterval.
func (s *Source) retryWait(reason Reason, st *entryState) time.Duration {
	if reason != ReasonUpstreamError {
		st.failures = 0
		return MaxRetryInterval
	}
	st.failures++
	wait := MaxRetryInterval
	if shift := st.failures - 1; shift < 16 {
		if d := errorBackoffBase << shift; d < MaxRetryInterval {
			wait = d
		}
	}
	return min(wait+s.cfg.Jitter(wait/2), MaxRetryInterval)
}

// logChange logs one Info line when origin's identity, installation or reason
// changes. It never logs the token.
func (s *Source) logChange(origin string, e *entry, c Credential) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.logged.Identity == c.Identity && e.logged.InstallationID == c.InstallationID && e.logged.Reason == c.Reason {
		return
	}
	e.logged = Credential{Identity: c.Identity, InstallationID: c.InstallationID, Reason: c.Reason}
	s.cfg.Logger.Info().Str("repo_origin", origin).Str("identity", string(c.Identity)).
		Int64("installation_id", c.InstallationID).Str("reason", string(c.Reason)).
		Msg("github credentials: repository credential changed")
}
