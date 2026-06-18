// Package authtoken contains small, dependency-free helpers for reasoning
// about WorkOS authentication tokens shared between the boss CLI and the bossd
// daemon.
package authtoken

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// DefaultTTL is the fallback access-token lifetime used only when expiry
// cannot be derived from the token itself or from a positive expires_in.
// WorkOS access tokens are short-lived (~5 minutes); the daemon's refresher
// will renew well before this elapses.
const DefaultTTL = 5 * time.Minute

// AccessTokenExpiry returns the best-known absolute expiry for a WorkOS access
// token.
//
// WorkOS access tokens are JWTs whose `exp` claim is authoritative. The OAuth
// `expires_in` field returned alongside them is unreliable — in practice it
// arrives as 0 on token refresh, which previously made the daemon compute an
// expiry of ~now and spin in a perpetual refresh loop, starving the upstream
// reverse stream (and therefore webhook delivery). We therefore prefer the JWT
// `exp` claim, fall back to a positive expires_in, and only then to DefaultTTL.
// We never return ~now, so a degenerate response can't reintroduce the loop.
func AccessTokenExpiry(accessToken string, expiresIn int, now time.Time) time.Time {
	if exp, ok := jwtExp(accessToken); ok {
		return exp
	}
	if expiresIn > 0 {
		return now.Add(time.Duration(expiresIn) * time.Second)
	}
	return now.Add(DefaultTTL)
}

// jwtExp extracts the `exp` claim (as an absolute time) from a JWT access
// token without verifying its signature — verification is bosso's job. It
// returns ok=false when the token is not a JWT or carries no numeric exp.
func jwtExp(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Tolerate tokens that included padding.
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return time.Time{}, false
		}
	}
	var claims struct {
		Exp *int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, false
	}
	if claims.Exp == nil {
		return time.Time{}, false
	}
	return time.Unix(*claims.Exp, 0), true
}
