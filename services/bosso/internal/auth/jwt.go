package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"crypto/rsa"
	"encoding/base64"

	"github.com/golang-jwt/jwt/v5"
)

// JWTValidator validates OIDC JWTs against a JWKS endpoint.
type JWTValidator struct {
	issuer   string // e.g. "https://bossanova.auth0.com/"
	audience string // e.g. "https://api.bossanova.dev"
	jwksURL  string // e.g. "https://bossanova.auth0.com/.well-known/jwks.json"

	mu   sync.RWMutex
	keys map[string]*rsa.PublicKey
	exp  time.Time

	client *http.Client
}

// JWTConfig holds configuration for the JWT validator.
type JWTConfig struct {
	Issuer   string // OIDC issuer URL (e.g. "https://bossanova.auth0.com/")
	Audience string // Expected audience claim
	JWKSURL  string // JWKS endpoint (defaults to issuer + ".well-known/jwks.json")
}

// NewJWTValidator creates a new OIDC JWT validator.
func NewJWTValidator(cfg JWTConfig) *JWTValidator {
	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		jwksURL = cfg.Issuer + ".well-known/jwks.json"
	}
	return &JWTValidator{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		jwksURL:  jwksURL,
		keys:     make(map[string]*rsa.PublicKey),
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Claims holds the validated JWT claims.
type Claims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

// Validate parses and validates an OIDC JWT, returning the claims.
func (v *JWTValidator) Validate(ctx context.Context, tokenStr string) (*Claims, error) {
	token, err := jwt.Parse(tokenStr, v.keyFunc(ctx),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("parse jwt: %w", err)
	}

	mapClaims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("unexpected claims type")
	}

	sub, _ := mapClaims.GetSubject()
	if sub == "" {
		return nil, errors.New("missing sub claim")
	}

	email, _ := mapClaims["email"].(string)

	return &Claims{Sub: sub, Email: email}, nil
}

// keyFunc returns a jwt.Keyfunc that resolves signing keys from JWKS.
func (v *JWTValidator) keyFunc(ctx context.Context) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		kid, ok := token.Header["kid"].(string)
		if !ok {
			return nil, errors.New("missing kid header")
		}

		key, err := v.getKey(ctx, kid)
		if err != nil {
			return nil, err
		}
		return key, nil
	}
}

// getKey returns the RSA public key for the given key ID, fetching JWKS if needed.
func (v *JWTValidator) getKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key, ok := v.keys[kid]
	expired := time.Now().After(v.exp)
	v.mu.RUnlock()

	if ok && !expired {
		return key, nil
	}

	// Fetch JWKS.
	if err := v.fetchJWKS(ctx); err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}

	v.mu.RLock()
	key, ok = v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	return key, nil
}

// jwksResponse represents the JSON Web Key Set response.
type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// fetchJWKS fetches and caches the JWKS from the configured endpoint.
func (v *JWTValidator) fetchJWKS(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}

	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks endpoint returned %d", resp.StatusCode)
	}

	var jwks jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" || k.Use != "sig" {
			continue
		}
		pubKey, err := parseRSAPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pubKey
	}

	v.mu.Lock()
	v.keys = keys
	v.exp = time.Now().Add(1 * time.Hour)
	v.mu.Unlock()

	return nil
}

// parseRSAPublicKey parses base64url-encoded RSA modulus and exponent.
func parseRSAPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)

	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}
