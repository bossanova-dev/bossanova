package ghtoken

import (
	"fmt"
	"maps"
	"time"
)

// Identity names whose GitHub credential a repository uses.
type Identity string

const (
	// IdentityUser is the operator's own gh login, the fallback for every
	// repository the installation token does not cover.
	IdentityUser Identity = "user"
	// IdentityInstallation is the Boss GitHub App installation token bosso
	// minted for the repository.
	IdentityInstallation Identity = "installation"
)

// Reason says why a repository uses its identity. The values are stable and
// carry no secret, so diagnostics renders them verbatim.
type Reason string

const (
	// ReasonInstallation means the installation token is in use.
	ReasonInstallation Reason = "installation"
	// ReasonNotSignedIn means no upstream, an empty daemon token, or a failed
	// re-register after the daemon token was rejected.
	ReasonNotSignedIn Reason = "not_signed_in"
	// ReasonNotGitHub means the origin is not a GitHub repository.
	ReasonNotGitHub Reason = "not_github"
	// ReasonNoInstallation means bosso refused (PermissionDenied): no
	// installation covers the repository, or this daemon's owner may not use it.
	ReasonNoInstallation Reason = "no_installation"
	// ReasonAppUnavailable means the App is disabled or unconfigured on bosso
	// (FailedPrecondition).
	ReasonAppUnavailable Reason = "app_unavailable"
	// ReasonUpstreamUnsupported means bosso predates the RPC (Unimplemented).
	ReasonUpstreamUnsupported Reason = "upstream_unsupported"
	// ReasonUpstreamError covers every other failure: Unavailable, a deadline,
	// a transport error, or an unusable response.
	ReasonUpstreamError Reason = "upstream_error"
	// ReasonNotRequested is report-only: signed in, but nothing has asked for
	// this repository's credential yet.
	ReasonNotRequested Reason = "not_requested"
)

// Credential is the GitHub credential one repository uses right now. The
// installation token sits in an unexported field, read through Token, so
// fmt verbs and encoding/json never render it.
type Credential struct {
	Identity       Identity
	InstallationID int64
	AccountLogin   string
	// Permissions is what GitHub granted, permission name to access level.
	Permissions map[string]string
	ExpiresAt   time.Time
	Reason      Reason

	token string
}

// Token returns the installation token, or "" when the identity is the user.
func (c Credential) Token() string { return c.token }

// String renders the credential without its token, for %v and %+v.
func (c Credential) String() string {
	return fmt.Sprintf("{Identity:%s InstallationID:%d AccountLogin:%s Reason:%s ExpiresAt:%s}",
		c.Identity, c.InstallationID, c.AccountLogin, c.Reason, c.ExpiresAt.Format(time.RFC3339))
}

// GoString renders the credential without its token, for %#v.
func (c Credential) GoString() string {
	return fmt.Sprintf("ghtoken.Credential{Identity:%q, InstallationID:%d, AccountLogin:%q, Reason:%q, ExpiresAt:%q}",
		c.Identity, c.InstallationID, c.AccountLogin, c.Reason, c.ExpiresAt.Format(time.RFC3339))
}

// userCredential is the fallback answer: the user's gh login, and why.
func userCredential(reason Reason) Credential {
	return Credential{Identity: IdentityUser, Reason: reason}
}

// RepoCredentialState is one local repository's credential as Report sees
// it. It never carries the token.
type RepoCredentialState struct {
	RepoID         string
	Origin         string
	Identity       Identity
	InstallationID int64
	AccountLogin   string
	Reason         Reason
	// ExpiresAt is the cached installation token's expiry; zero without one.
	ExpiresAt time.Time
	// NextAttemptAt is the earliest time the source asks bosso again; zero
	// when nothing is scheduled.
	NextAttemptAt time.Time
}

// entryState is one repository's cached answer. Callers hold entry.mu.
type entryState struct {
	attempted      bool
	token          string
	installationID int64
	accountLogin   string
	permissions    map[string]string
	expiresAt      time.Time
	// reason is the last outcome: ReasonInstallation after a mint, the
	// refusal or failure reason otherwise.
	reason        Reason
	nextAttemptAt time.Time
	failures      int
	// rejected is the last token GitHub refused (see Source.Invalidate); a
	// mint that returns it again is not cached.
	rejected string
}

// usable reports whether the cached token may still be served at now.
func (s *entryState) usable(now time.Time) bool {
	return s.token != "" && s.expiresAt.Sub(now) > staleGuard
}

// fresh reports whether the cached token needs no refresh at now.
func (s *entryState) fresh(now time.Time) bool {
	return s.token != "" && s.expiresAt.Sub(now) > refreshMargin
}

// answer is the credential the cached state yields at now with no RPC: the
// usable token, otherwise the user's login with the last reason. A token that
// aged out before the next attempt is allowed (bosso returned one already
// close to expiry) answers upstream_error.
func (s *entryState) answer(now time.Time) Credential {
	if s.usable(now) {
		return Credential{
			Identity:       IdentityInstallation,
			InstallationID: s.installationID,
			AccountLogin:   s.accountLogin,
			Permissions:    maps.Clone(s.permissions),
			ExpiresAt:      s.expiresAt,
			Reason:         ReasonInstallation,
			token:          s.token,
		}
	}
	if s.reason == ReasonInstallation {
		return userCredential(ReasonUpstreamError)
	}
	return userCredential(s.reason)
}
