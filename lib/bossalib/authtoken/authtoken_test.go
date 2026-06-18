package authtoken

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// makeJWT builds an unsigned-but-well-formed JWT (header.payload.sig) whose
// payload carries the given claims. The signature segment is irrelevant here —
// AccessTokenExpiry never verifies it (bosso does that server-side); it only
// reads the exp claim.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	return makeJWTWithPayloadEncoding(t, claims, base64.RawURLEncoding)
}

func makeJWTWithPayloadEncoding(t *testing.T, claims map[string]any, payloadEncoding *base64.Encoding) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	header := enc(map[string]string{"alg": "RS256", "typ": "JWT"})
	return header + "." + payloadEncoding.EncodeToString(payload) + ".sig"
}

func TestAccessTokenExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	t.Run("prefers JWT exp claim over expires_in", func(t *testing.T) {
		exp := now.Add(5 * time.Minute).Unix()
		// expires_in is deliberately wrong (0) — the JWT must win.
		tok := makeJWT(t, map[string]any{"exp": exp})
		got := AccessTokenExpiry(tok, 0, now)
		if want := time.Unix(exp, 0); !got.Equal(want) {
			t.Fatalf("got %v, want %v (JWT exp)", got, want)
		}
	})

	t.Run("JWT exp wins even when expires_in is set", func(t *testing.T) {
		exp := now.Add(5 * time.Minute).Unix()
		tok := makeJWT(t, map[string]any{"exp": exp})
		got := AccessTokenExpiry(tok, 9999, now) // conflicting expires_in
		if want := time.Unix(exp, 0); !got.Equal(want) {
			t.Fatalf("got %v, want %v (JWT exp must take precedence)", got, want)
		}
	})

	t.Run("falls back to expires_in when token is not a JWT", func(t *testing.T) {
		got := AccessTokenExpiry("opaque-not-a-jwt", 120, now)
		if want := now.Add(120 * time.Second); !got.Equal(want) {
			t.Fatalf("got %v, want %v (expires_in fallback)", got, want)
		}
	})

	t.Run("falls back to expires_in when JWT has no exp claim", func(t *testing.T) {
		tok := makeJWT(t, map[string]any{"sub": "user_123"})
		got := AccessTokenExpiry(tok, 200, now)
		if want := now.Add(200 * time.Second); !got.Equal(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("uses default TTL when neither JWT exp nor positive expires_in available", func(t *testing.T) {
		// This is the regression case: WorkOS returned expires_in:0 and the
		// token couldn't be decoded. We must NOT return ~now (which caused the
		// perpetual 60s refresh loop) — return a sane future expiry instead.
		got := AccessTokenExpiry("opaque", 0, now)
		if want := now.Add(DefaultTTL); !got.Equal(want) {
			t.Fatalf("got %v, want %v (default TTL)", got, want)
		}
		if !got.After(now) {
			t.Fatalf("expiry %v must be in the future relative to %v", got, now)
		}
	})

	t.Run("malformed JWT payload falls back, never returns ~now", func(t *testing.T) {
		got := AccessTokenExpiry("a.!!!notbase64!!!.c", 0, now)
		if want := now.Add(DefaultTTL); !got.Equal(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("numeric zero exp is an expired JWT claim, not a missing claim", func(t *testing.T) {
		tok := makeJWT(t, map[string]any{"exp": 0})
		got := AccessTokenExpiry(tok, 120, now)
		if want := time.Unix(0, 0); !got.Equal(want) {
			t.Fatalf("got %v, want %v (numeric exp=0)", got, want)
		}
	})

	t.Run("past exp claim is authoritative", func(t *testing.T) {
		exp := now.Add(-time.Hour).Unix()
		tok := makeJWT(t, map[string]any{"exp": exp})
		got := AccessTokenExpiry(tok, 120, now)
		if want := time.Unix(exp, 0); !got.Equal(want) {
			t.Fatalf("got %v, want %v (past JWT exp)", got, want)
		}
	})

	t.Run("wrong-type exp falls back to expires_in", func(t *testing.T) {
		tok := makeJWT(t, map[string]any{"exp": "1700000300"})
		got := AccessTokenExpiry(tok, 120, now)
		if want := now.Add(120 * time.Second); !got.Equal(want) {
			t.Fatalf("got %v, want %v (wrong-type exp fallback)", got, want)
		}
	})

	t.Run("non-integer exp falls back to expires_in", func(t *testing.T) {
		tok := makeJWT(t, map[string]any{"exp": 1700000300.5})
		got := AccessTokenExpiry(tok, 120, now)
		if want := now.Add(120 * time.Second); !got.Equal(want) {
			t.Fatalf("got %v, want %v (non-integer exp fallback)", got, want)
		}
	})

	t.Run("padded JWT payload is accepted", func(t *testing.T) {
		exp := now.Add(5 * time.Minute).Unix()
		tok := makeJWTWithPayloadEncoding(t, map[string]any{"exp": exp}, base64.URLEncoding)
		got := AccessTokenExpiry(tok, 0, now)
		if want := time.Unix(exp, 0); !got.Equal(want) {
			t.Fatalf("got %v, want %v (padded JWT exp)", got, want)
		}
	})

	t.Run("invalid JWT segment counts fall back", func(t *testing.T) {
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1700000300}`))
		for _, tok := range []string{"header." + payload, "header." + payload + ".sig.extra"} {
			got := AccessTokenExpiry(tok, 120, now)
			if want := now.Add(120 * time.Second); !got.Equal(want) {
				t.Fatalf("token %q got %v, want %v", tok, got, want)
			}
		}
	})

	t.Run("JWT payload that actually requires base64 padding decodes via URLEncoding fallback", func(t *testing.T) {
		// jwtExp tries RawURLEncoding (unpadded) first and only falls back to
		// the padded URLEncoding decoder when the first attempt errors. The
		// existing "padded JWT payload is accepted" case marshals an 18-byte
		// payload whose length is divisible by 3, so URLEncoding emits no "="
		// and RawURLEncoding succeeds outright — the fallback branch never runs.
		// Pick an exp whose marshaled payload is NOT a multiple of 3 bytes so
		// URLEncoding genuinely adds padding, forcing the fallback path and
		// pinning both the "RawURLEncoding failed" and "URLEncoding succeeded"
		// conditions in jwtExp.
		exp := int64(17_000_000_000) // {"exp":17000000000} is 19 bytes (19 % 3 == 1)
		payload, err := json.Marshal(map[string]any{"exp": exp})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if len(payload)%3 == 0 {
			t.Fatalf("payload length %d is a multiple of 3; choose an exp that forces base64 padding", len(payload))
		}
		encoded := base64.URLEncoding.EncodeToString(payload)
		if !strings.Contains(encoded, "=") {
			t.Fatalf("encoded payload %q has no padding; cannot exercise the URLEncoding fallback", encoded)
		}
		// The unpadded decoder must reject this segment, so the fallback is the
		// only path that can recover the exp claim.
		if _, err := base64.RawURLEncoding.DecodeString(encoded); err == nil {
			t.Fatalf("RawURLEncoding unexpectedly accepted padded segment %q", encoded)
		}

		tok := "header." + encoded + ".sig"
		got := AccessTokenExpiry(tok, 0, now)
		if want := time.Unix(exp, 0); !got.Equal(want) {
			t.Fatalf("got %v, want %v (padded JWT exp via URLEncoding fallback)", got, want)
		}
	})
}
