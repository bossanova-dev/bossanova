package auth

import (
	"context"
	"net/http"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/recurser/bosso/internal/db"
)

// TokenLookup looks up a daemon by session token.
type TokenLookup interface {
	GetByToken(ctx context.Context, token string) (*db.Daemon, error)
}

// UserLookup finds a user by OIDC subject.
type UserLookup interface {
	GetBySub(ctx context.Context, sub string) (*db.User, error)
}

// NewMiddleware creates an authn.Middleware that authenticates requests.
//
// Authentication flow:
//  1. Extract bearer token from Authorization header.
//  2. Try OIDC JWT validation. If the token is a valid JWT, look up the user
//     by OIDC subject. If the user exists, attach user Info to context.
//  3. If JWT validation fails, try session token lookup. If a daemon matches,
//     attach daemon Info to context.
//  4. If both fail, return Unauthenticated.
func NewMiddleware(jwtValidator *JWTValidator, users UserLookup, daemons TokenLookup, opts ...connect.HandlerOption) *authn.Middleware {
	authenticate := func(ctx context.Context, req *http.Request) (any, error) {
		token, ok := authn.BearerToken(req)
		if !ok {
			return nil, authn.Errorf("missing or invalid Authorization header")
		}

		// Try OIDC JWT first.
		claims, err := jwtValidator.Validate(ctx, token)
		if err == nil {
			// Valid JWT — look up user by OIDC subject.
			user, err := users.GetBySub(ctx, claims.Sub)
			if err != nil {
				return nil, authn.Errorf("unknown user")
			}
			return &Info{
				UserID: user.ID,
				Sub:    claims.Sub,
				Email:  claims.Email,
			}, nil
		}

		// JWT validation failed — try session token lookup.
		daemon, err := daemons.GetByToken(ctx, token)
		if err != nil {
			return nil, authn.Errorf("invalid credentials")
		}
		return &Info{
			DaemonID:     daemon.ID,
			DaemonUserID: daemon.UserID,
		}, nil
	}

	return authn.NewMiddleware(authenticate, opts...)
}
