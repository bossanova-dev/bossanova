// Package auth provides authentication middleware for the orchestrator.
//
// Two authentication schemes are supported:
//
//   - OIDC JWT (Bearer token): Used by the CLI after `boss login`. Validates
//     the JWT against the Auth0 JWKS endpoint and extracts the user identity.
//     Required for RegisterDaemon and other user-facing endpoints.
//
//   - Daemon session token (Bearer token): Returned by RegisterDaemon and used
//     by daemons for subsequent requests (Heartbeat, ListDaemons, etc.). Looked
//     up directly in the database via DaemonStore.GetByToken.
//
// The middleware tries OIDC first; if the token isn't a valid JWT, it falls
// back to session token lookup. On success, authenticated info is attached
// to the context via connectrpc.com/authn.SetInfo and can be retrieved with
// InfoFromContext.
package auth

import (
	"context"

	"connectrpc.com/authn"
)

// Info holds the authenticated caller identity attached to the request context.
// Exactly one of UserID or DaemonID will be set.
type Info struct {
	// UserID is set when the request was authenticated via OIDC JWT.
	UserID string
	// Sub is the OIDC subject claim (set with UserID).
	Sub string
	// Email is the OIDC email claim (set with UserID, may be empty).
	Email string
	// DaemonID is set when the request was authenticated via session token.
	DaemonID string
	// DaemonUserID is the owning user's ID (set with DaemonID).
	DaemonUserID string
}

// IsUser returns true if the caller authenticated as a user (OIDC JWT).
func (i *Info) IsUser() bool { return i.UserID != "" }

// IsDaemon returns true if the caller authenticated as a daemon (session token).
func (i *Info) IsDaemon() bool { return i.DaemonID != "" }

// InfoFromContext extracts the authenticated Info from the request context.
// Returns nil if the request is not authenticated.
func InfoFromContext(ctx context.Context) *Info {
	v, _ := authn.GetInfo(ctx).(*Info)
	return v
}
