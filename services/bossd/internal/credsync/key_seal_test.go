package credsync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
)

func TestHostileServerCannotInjectKnownCredentialKey(t *testing.T) {
	f := newSyncFixture(t)
	f.cloud.caller = "a"
	ctx := context.Background()
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	known := bytes.Repeat([]byte{42}, 32)
	box, err := SealCredentialKey(f.b.config.DeviceKey.PublicKey().Bytes(), known, "u", 1)
	if err != nil {
		t.Fatal(err)
	}
	f.cloud.caller = "b"
	f.cloud.seals["b"] = box
	if err := f.b.EnsureCredentialKey(ctx); err == nil {
		t.Fatal("unsigned server-generated known key accepted")
	}
	_, attacker, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	seal := signedKeySeal{Version: 1, SignerDaemonID: "a", Box: box}
	seal.Signature = ed25519.Sign(attacker, keySealStatement("u", 1, "b", f.b.config.DeviceKey.PublicKey().Bytes(), "a", box))
	encoded, err := json.Marshal(seal)
	if err != nil {
		t.Fatal(err)
	}
	f.cloud.seals["b"] = encoded
	if err := f.b.EnsureCredentialKey(ctx); err == nil {
		t.Fatal("server-forged origin signature accepted")
	}
}
func TestSignedSealContextIsBound(t *testing.T) {
	f := newSyncFixture(t)
	f.cloud.caller = "a"
	ctx := context.Background()
	if err := f.a.EnsureCredentialKey(ctx); err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(f.cloud.seals["b"])
	f.cloud.caller = "b"
	if _, err := f.b.verifiedDevices(f.cloud.devices); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.openSignedKeySeal(1, original, f.cloud.devices); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"signer", "box", "signature"} {
		t.Run(field, func(t *testing.T) {
			var s signedKeySeal
			if err := json.Unmarshal(original, &s); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "signer":
				s.SignerDaemonID = "b"
			case "box":
				s.Box[0] ^= 1
			case "signature":
				s.Signature[0] ^= 1
			}
			encoded, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.b.openSignedKeySeal(1, encoded, f.cloud.devices); err == nil {
				t.Fatal("tampered seal accepted")
			}
		})
	}
	if _, err := f.b.openSignedKeySeal(2, original, f.cloud.devices); err == nil {
		t.Fatal("generation substitution accepted")
	}
	f.b.config.UserID = "other"
	if _, err := f.b.openSignedKeySeal(1, original, f.cloud.devices); err == nil {
		t.Fatal("cross-user seal accepted")
	}
	f.b.config.UserID = "u"
	if _, err := f.a.openSignedKeySeal(1, original, f.cloud.devices); err == nil {
		t.Fatal("cross-recipient seal accepted")
	}
}
func TestSignedGenerationRollbackRefusedAcrossRestart(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	f.cloud.caller = "a"
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	old := bytes.Clone(f.cloud.seals["a"])
	f.cloud.devices[1].Status = "revoked"
	if err := f.a.Roll(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	trust := f.a.trust
	f.a = NewEngine(f.a.config, f.cloud, f.la, trust)
	// A hostile server suppresses the revocation and replays an authentic old seal.
	f.cloud.devices[1].Status = "enrolled"
	f.cloud.generation = 1
	f.cloud.seals["a"] = old
	if err := f.a.EnsureCredentialKey(ctx); !errors.Is(err, ErrRollback) {
		t.Fatalf("signed older generation was accepted: %v", err)
	}
}

func TestInterruptedRotationRetriesDurableProposalAcrossRestart(t *testing.T) {
	for _, responseLost := range []bool{false, true} {
		name := "before-commit"
		if responseLost {
			name = "response-lost"
		}
		t.Run(name, func(t *testing.T) {
			f := newSyncFixture(t)
			ctx := context.Background()
			f.cloud.caller = "a"
			if err := f.a.Publish(ctx, "account", "original"); err != nil {
				t.Fatal(err)
			}
			f.cloud.devices[1].Status = "revoked"
			f.cloud.rollErrorBefore = !responseLost
			f.cloud.rollErrorAfter = responseLost
			if err := f.a.Roll(ctx, "b"); err == nil {
				t.Fatal("transport failure was hidden")
			}
			if p, err := f.a.trust.PendingRotation(); err != nil || p == nil {
				t.Fatalf("retry intent missing: %v", err)
			}
			f.a = NewEngine(f.a.config, f.cloud, f.la, f.a.trust)
			if err := f.a.EnsureCredentialKey(ctx); err != nil {
				t.Fatal(err)
			}
			if f.cloud.generation != 2 || f.cloud.rolls != 1 {
				t.Fatal("retry did not resolve one committed rotation")
			}
			if p, err := f.a.trust.PendingRotation(); err != nil || p != nil {
				t.Fatalf("committed rotation still pending: %v", err)
			}
			if err := f.a.Publish(ctx, "account", "original"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
