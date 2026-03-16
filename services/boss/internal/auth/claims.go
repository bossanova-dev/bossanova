package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// idTokenClaims holds the claims we care about from the ID token.
type idTokenClaims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// parseIDTokenClaims extracts claims from a JWT without validation.
// This is safe because the ID token was already validated by the issuer
// during the token exchange. We only use it for display (email, name).
func parseIDTokenClaims(idToken string) (*idTokenClaims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	var claims idTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("unmarshal claims: %w", err)
	}

	return &claims, nil
}
