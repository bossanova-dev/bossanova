package jwk

import (
	"encoding/base64"
	"math/big"
	"strings"
	"testing"
)

func TestParseRSAPublicKey_Modulus(t *testing.T) {
	t.Parallel()
	exponent := encodeBigInt(big.NewInt(65537))
	tests := []struct {
		name    string
		modulus *big.Int
		wantErr string
	}{
		{name: "rejects zero", modulus: new(big.Int), wantErr: "invalid RSA modulus"},
		{name: "rejects 1024 bits", modulus: rsaModulusForBits(t, 1024), wantErr: "invalid RSA modulus"},
		{name: "rejects 2047 bits", modulus: rsaModulusForBits(t, 2047), wantErr: "invalid RSA modulus"},
		{name: "accepts 2048 bits", modulus: rsaModulusForBits(t, 2048)},
		{name: "accepts 4096 bits", modulus: rsaModulusForBits(t, 4096)},
		{name: "rejects even", modulus: evenRSAmodulusForBits(t, 2048), wantErr: "invalid RSA modulus"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := ParseRSAPublicKey(encodeBigInt(tt.modulus), exponent)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseRSAPublicKey() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRSAPublicKey() error = %v", err)
			}
			if got, want := key.N.BitLen(), tt.modulus.BitLen(); got != want {
				t.Fatalf("modulus bit length = %d, want %d", got, want)
			}
			if key.E != 65537 {
				t.Fatalf("exponent = %d, want 65537", key.E)
			}
		})
	}
}

func TestParseRSAPublicKey_Exponent(t *testing.T) {
	t.Parallel()
	modulus := encodeBigInt(rsaModulusForBits(t, 2048))
	tooLarge := new(big.Int).Add(big.NewInt(maxRSAPublicExponent), big.NewInt(1))
	tests := []struct {
		name     string
		exponent *big.Int
		wantErr  string
	}{
		{name: "rejects empty", exponent: nil, wantErr: "invalid RSA public exponent"},
		{name: "rejects zero", exponent: new(big.Int), wantErr: "invalid RSA public exponent"},
		{name: "rejects one", exponent: big.NewInt(1), wantErr: "invalid RSA public exponent"},
		{name: "rejects even", exponent: big.NewInt(65536), wantErr: "invalid RSA public exponent"},
		{name: "rejects above maximum", exponent: tooLarge, wantErr: "invalid RSA public exponent"},
		{name: "accepts 65537", exponent: big.NewInt(65537)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := ParseRSAPublicKey(modulus, encodeBigInt(tt.exponent))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseRSAPublicKey() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRSAPublicKey() error = %v", err)
			}
			if key.E != 65537 {
				t.Fatalf("exponent = %d, want 65537", key.E)
			}
		})
	}
}

func TestParseRSAPublicKey_MalformedBase64(t *testing.T) {
	t.Parallel()
	modulus := encodeBigInt(rsaModulusForBits(t, 2048))
	exponent := encodeBigInt(big.NewInt(65537))

	if _, err := ParseRSAPublicKey("not!base64", exponent); err == nil || !strings.Contains(err.Error(), "decode n") {
		t.Fatalf("invalid modulus error = %v, want decode n error", err)
	}
	if _, err := ParseRSAPublicKey(modulus, "not!base64"); err == nil || !strings.Contains(err.Error(), "decode e") {
		t.Fatalf("invalid exponent error = %v, want decode e error", err)
	}
}

func rsaModulusForBits(t *testing.T, bits int) *big.Int {
	t.Helper()
	modulus := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	modulus.SetBit(modulus, 0, 1)
	if modulus.BitLen() != bits || modulus.Bit(0) != 1 {
		t.Fatalf("synthetic modulus has bit length %d and low bit %d, want %d and 1", modulus.BitLen(), modulus.Bit(0), bits)
	}
	return modulus
}

func evenRSAmodulusForBits(t *testing.T, bits int) *big.Int {
	t.Helper()
	modulus := rsaModulusForBits(t, bits)
	modulus.SetBit(modulus, 0, 0)
	if modulus.BitLen() != bits || modulus.Bit(0) != 0 {
		t.Fatalf("synthetic modulus has bit length %d and low bit %d, want %d and 0", modulus.BitLen(), modulus.Bit(0), bits)
	}
	return modulus
}

func encodeBigInt(value *big.Int) string {
	if value == nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(value.Bytes())
}
