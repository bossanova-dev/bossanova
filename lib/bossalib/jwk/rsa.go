// Package jwk parses JSON Web Keys shared across Bossanova services.
package jwk

import (
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
)

const (
	// MinRSAModulusBits is the JWA (RFC 7518 §3.3) minimum for RS256,
	// RS384, and RS512 signing keys.
	MinRSAModulusBits    = 2048
	maxRSAPublicExponent = 1<<31 - 1
)

// ParseRSAPublicKey parses and validates a base64url-encoded RSA JWK modulus
// and exponent.
func ParseRSAPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}

	modulus := new(big.Int).SetBytes(nBytes)
	if modulus.Sign() <= 0 || modulus.BitLen() < MinRSAModulusBits || modulus.Bit(0) == 0 {
		return nil, errors.New("invalid RSA modulus")
	}
	exponent := new(big.Int).SetBytes(eBytes)
	if exponent.Sign() <= 0 || exponent.Cmp(big.NewInt(maxRSAPublicExponent)) > 0 {
		return nil, errors.New("invalid RSA public exponent")
	}
	publicExponent := int(exponent.Int64())
	if publicExponent <= 1 || publicExponent%2 == 0 {
		return nil, errors.New("invalid RSA public exponent")
	}
	return &rsa.PublicKey{N: modulus, E: publicExponent}, nil
}
