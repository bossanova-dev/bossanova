package credsync

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func TestCredentialEncryption(t *testing.T) {
	key, err := NewCredentialKey()
	if err != nil {
		t.Fatal(err)
	}
	aad := CredentialAAD{UserID: "user", SyncedAgentAccountID: "account", CredentialVersion: 1, KeyGeneration: 1}
	blob, err := EncryptCredential(key, []byte("secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := DecryptCredential(key, blob, aad)
	if err != nil || string(plain) != "secret" {
		t.Fatalf("roundtrip: %q %v", plain, err)
	}
	variants := []CredentialAAD{aad, aad, aad, aad}
	variants[0].UserID = "other"
	variants[1].SyncedAgentAccountID = "other"
	variants[2].CredentialVersion++
	variants[3].KeyGeneration++
	for _, changed := range variants {
		if _, err := DecryptCredential(key, blob, changed); err == nil {
			t.Fatal("modified AAD accepted")
		}
	}
	if _, err := DecryptCredential(make([]byte, 32), blob, aad); err == nil {
		t.Fatal("server data without credential key decrypted")
	}
	seen := map[string]bool{}
	for range 10000 {
		b, e := EncryptCredential(key, []byte("s"), aad)
		if e != nil {
			t.Fatal(e)
		}
		nonce := string(b[:12])
		if seen[nonce] {
			t.Fatal("nonce reused")
		}
		seen[nonce] = true
	}
}
func TestCredentialKeySealing(t *testing.T) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	sealed, err := SealCredentialKey(priv.PublicKey().Bytes(), key, "u", 1)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenCredentialKey(priv, sealed, "u", 1)
	if err != nil || !bytes.Equal(key, opened) {
		t.Fatalf("roundtrip: %v", err)
	}
	if _, err := OpenCredentialKey(other, sealed, "u", 1); err == nil {
		t.Fatal("wrong device opened seal")
	}
	if _, err := OpenCredentialKey(priv, sealed, "u", 2); err == nil {
		t.Fatal("wrong generation opened seal")
	}
	if _, err := OpenCredentialKey(priv, sealed, "other", 1); err == nil {
		t.Fatal("wrong user opened seal")
	}
}

func TestCredentialKnownAnswer(t *testing.T) {
	// AES-256-GCM fixture generated independently using Python cryptography.
	blob, err := hex.DecodeString("000000000000000000000000a5c92f4a2340081c622aa0bdce9afc7499c918b66e323d5e1e09a6615d2365a9")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := DecryptCredential(make([]byte, 32), blob, CredentialAAD{UserID: "u", SyncedAgentAccountID: "a", CredentialVersion: 1, KeyGeneration: 1})
	if err != nil || string(plain) != "known credential" {
		t.Fatalf("known answer: %q %v", plain, err)
	}
}

func TestCredentialKeyKnownAnswer(t *testing.T) {
	// Fixed RFC 9180 suite/encoding fixture; no production or enrolled key material.
	private, err := ecdh.X25519().NewPrivateKey(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := hex.DecodeString("51da911fd1ca7357b6270e16b5631bd158d41bc74e43b75650d04f1af53bed31e335171a1e8abfe53e687d2388bca3fede8c25675e79f015b31db6bf3243620ee12f8035642c72688205d4817b4e3c38")
	if err != nil {
		t.Fatal(err)
	}
	key, err := OpenCredentialKey(private, sealed, "u", 1)
	if err != nil || !bytes.Equal(key, bytes.Repeat([]byte{7}, 32)) {
		t.Fatalf("known answer: %v", err)
	}
}
