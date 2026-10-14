package daemonproof

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

func TestMessageLayout(t *testing.T) {
	got := Message("daemon-1", "tok", 1700000000)
	// sha256("tok") in hex.
	want := []byte("bossanova.daemon-register.v1\x00daemon-1\x00" +
		"1a7674eb4ee78df7e1ac439a93c3fa8e3c945784d4dec9fd8e3011738b2f1d62" +
		"\x001700000000")
	if !bytes.Equal(got, want) {
		t.Fatalf("Message =\n%q\nwant\n%q", got, want)
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv := newKey(t)
	now := time.Unix(1700000000, 0)
	sig := Sign(priv, "daemon-1", "bearer-a", now.Unix())
	if err := Verify(pub, "daemon-1", "bearer-a", now.Unix(), sig, now); err != nil {
		t.Fatalf("Verify round trip: %v", err)
	}
	// Within the window on either side.
	if err := Verify(pub, "daemon-1", "bearer-a", now.Unix(), sig, now.Add(MaxSkew)); err != nil {
		t.Fatalf("Verify at +MaxSkew: %v", err)
	}
	if err := Verify(pub, "daemon-1", "bearer-a", now.Unix(), sig, now.Add(-MaxSkew)); err != nil {
		t.Fatalf("Verify at -MaxSkew: %v", err)
	}
}

func TestVerifyRejectsChangedInputs(t *testing.T) {
	pub, priv := newKey(t)
	otherPub, _ := newKey(t)
	now := time.Unix(1700000000, 0)
	signedAt := now.Unix()
	sig := Sign(priv, "daemon-1", "bearer-a", signedAt)

	cases := []struct {
		name     string
		pub      []byte
		daemonID string
		bearer   string
		signedAt int64
		sig      []byte
		want     error
	}{
		{"daemon id", pub, "daemon-2", "bearer-a", signedAt, sig, ErrBadSignature},
		{"bearer", pub, "daemon-1", "bearer-b", signedAt, sig, ErrBadSignature},
		{"timestamp", pub, "daemon-1", "bearer-a", signedAt + 1, sig, ErrBadSignature},
		{"other key", otherPub, "daemon-1", "bearer-a", signedAt, sig, ErrBadSignature},
		{"truncated signature", pub, "daemon-1", "bearer-a", signedAt, sig[:10], ErrBadSignature},
		{"empty signature", pub, "daemon-1", "bearer-a", signedAt, nil, ErrBadSignature},
		{"short public key", pub[:16], "daemon-1", "bearer-a", signedAt, sig, ErrInvalidPublicKey},
		{"empty public key", nil, "daemon-1", "bearer-a", signedAt, sig, ErrInvalidPublicKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.pub, tc.daemonID, tc.bearer, tc.signedAt, tc.sig, now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyRejectsStaleTimestamp(t *testing.T) {
	pub, priv := newKey(t)
	now := time.Unix(1700000000, 0)

	past := now.Add(-MaxSkew - time.Second).Unix()
	if err := Verify(pub, "daemon-1", "b", past, Sign(priv, "daemon-1", "b", past), now); !errors.Is(err, ErrStale) {
		t.Fatalf("Verify of a signature older than MaxSkew = %v, want ErrStale", err)
	}
	future := now.Add(MaxSkew + time.Second).Unix()
	if err := Verify(pub, "daemon-1", "b", future, Sign(priv, "daemon-1", "b", future), now); !errors.Is(err, ErrStale) {
		t.Fatalf("Verify of a signature dated past MaxSkew = %v, want ErrStale", err)
	}
}
