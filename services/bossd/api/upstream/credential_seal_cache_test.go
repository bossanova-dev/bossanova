package upstream

import (
	"bytes"
	"errors"
	"testing"

	"github.com/99designs/keyring"
	"github.com/recurser/bossd/internal/credsync"
)

func TestCredentialSealCachePersistsRollbackFloorAndExactBox(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	original := openKeyring
	openKeyring = func() (keyring.Keyring, error) { return ring, nil }
	t.Cleanup(func() { openKeyring = original })
	trust := CredentialTrust{Namespace: "issuer/user/daemon"}
	if highest, err := trust.HighestKeyGeneration(); err != nil || highest != 0 {
		t.Fatalf("initial highest=%d err=%v", highest, err)
	}
	if _, err := trust.AcceptedKeySeal(1); !errors.Is(err, credsync.ErrNotFound) {
		t.Fatal("missing box was accepted")
	}
	box := []byte("signed sealed credential key")
	if err := trust.SaveAcceptedKeySeal(3, box); err != nil {
		t.Fatal(err)
	}
	restarted := CredentialTrust{Namespace: trust.Namespace}
	highest, err := restarted.HighestKeyGeneration()
	if err != nil || highest != 3 {
		t.Fatalf("restored highest=%d err=%v", highest, err)
	}
	loaded, err := restarted.AcceptedKeySeal(3)
	if err != nil || !bytes.Equal(loaded, box) {
		t.Fatal("signed box not restored exactly")
	}
	loaded[0] ^= 1
	again, err := restarted.AcceptedKeySeal(3)
	if err != nil || !bytes.Equal(again, box) {
		t.Fatal("read mutated accepted box")
	}
	if err := restarted.SaveAcceptedKeySeal(2, box); !errors.Is(err, credsync.ErrRollback) {
		t.Fatal("older generation accepted")
	}
	replacement := []byte("replacement authenticated by the consumer")
	if err := restarted.SaveAcceptedKeySeal(3, replacement); err != nil {
		t.Fatal(err)
	}
	loadedReplacement, err := restarted.AcceptedKeySeal(3)
	if err != nil || !bytes.Equal(loadedReplacement, replacement) {
		t.Fatal("authenticated same generation replacement not retained")
	}
	if err := restarted.SaveAcceptedKeySeal(3, replacement); err != nil {
		t.Fatal("identical box was not idempotent")
	}
	if other, err := (CredentialTrust{Namespace: "issuer/other-user/daemon"}).HighestKeyGeneration(); err != nil || other != 0 {
		t.Fatal("rollback floor leaked across identities")
	}
	if err := restarted.SaveAcceptedKeySeal(4, []byte("next signed box")); err != nil {
		t.Fatal(err)
	}
	if highest, err := restarted.HighestKeyGeneration(); err != nil || highest != 4 {
		t.Fatal("next generation was not recorded")
	}
}

type sealCacheFailingKeyring struct {
	keyring.Keyring
	shouldFail bool
}

func (r *sealCacheFailingKeyring) Set(item keyring.Item) error {
	if r.shouldFail {
		return errors.New("keyring write unavailable")
	}
	return r.Keyring.Set(item)
}
func TestCredentialSealCacheFailedWriteKeepsPreviousFloor(t *testing.T) {
	ring := &sealCacheFailingKeyring{Keyring: keyring.NewArrayKeyring(nil)}
	original := openKeyring
	openKeyring = func() (keyring.Keyring, error) { return ring, nil }
	t.Cleanup(func() { openKeyring = original })
	trust := CredentialTrust{Namespace: "user/daemon"}
	originalBox := []byte("original signed seal")
	if err := trust.SaveAcceptedKeySeal(1, originalBox); err != nil {
		t.Fatal(err)
	}
	ring.shouldFail = true
	if err := trust.SaveAcceptedKeySeal(2, []byte("new signed seal")); err == nil {
		t.Fatal("write failure hidden")
	}
	if highest, err := trust.HighestKeyGeneration(); err != nil || highest != 1 {
		t.Fatal("failed write advanced rollback floor")
	}
	seal, err := trust.AcceptedKeySeal(1)
	if err != nil || !bytes.Equal(seal, originalBox) {
		t.Fatal("failed write changed accepted seal")
	}
	if _, err := trust.AcceptedKeySeal(2); !errors.Is(err, credsync.ErrNotFound) {
		t.Fatal("failed write accepted new generation")
	}
}

func TestPendingCredentialRotationSurvivesRestartAndClearsWithoutRollback(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	original := openKeyring
	openKeyring = func() (keyring.Keyring, error) { return ring, nil }
	t.Cleanup(func() { openKeyring = original })
	trust := CredentialTrust{Namespace: "pending/user/daemon"}
	if pending, err := trust.PendingRotation(); err != nil || pending != nil {
		t.Fatal("unexpected initial rotation")
	}
	rotation := credsync.KeyRotation{ExpectedGeneration: 1, KeyGeneration: 2, OwnSeal: []byte("own signed seal"), Seals: []credsync.KeySeal{{DaemonID: "survivor", SealedCredentialKey: []byte("survivor signed seal")}}, Blobs: []credsync.Blob{{SyncedAgentAccountID: "shared", CredentialVersion: 7, KeyGeneration: 2, Ciphertext: []byte("encrypted credential")}}}
	if err := trust.SavePendingRotation(rotation); err != nil {
		t.Fatal(err)
	}
	// Mutating the original and returned request cannot change the retry proposal.
	rotation.OwnSeal[0] ^= 1
	rotation.Seals[0].SealedCredentialKey[0] ^= 1
	rotation.Blobs[0].Ciphertext[0] ^= 1
	restarted := CredentialTrust{Namespace: trust.Namespace}
	pending, err := restarted.PendingRotation()
	if err != nil || pending == nil {
		t.Fatal("rotation lost after restart")
	}
	if pending.ExpectedGeneration != 1 || pending.KeyGeneration != 2 || pending.Blobs[0].CredentialVersion != 7 || string(pending.OwnSeal) != "own signed seal" || string(pending.Seals[0].SealedCredentialKey) != "survivor signed seal" || string(pending.Blobs[0].Ciphertext) != "encrypted credential" {
		t.Fatal("pending request changed")
	}
	pending.OwnSeal[0] ^= 1
	pending.Blobs[0].Ciphertext[0] ^= 1
	again, err := restarted.PendingRotation()
	if err != nil || string(again.OwnSeal) != "own signed seal" || string(again.Blobs[0].Ciphertext) != "encrypted credential" {
		t.Fatal("retry request was not independently copied")
	}
	highest, err := restarted.HighestKeyGeneration()
	if err != nil || highest != 2 {
		t.Fatal("pending request did not advance rollback floor")
	}
	seal, err := restarted.AcceptedKeySeal(2)
	if err != nil || string(seal) != "own signed seal" {
		t.Fatal("pending request did not pin own signed box")
	}
	if err := restarted.SavePendingRotation(credsync.KeyRotation{ExpectedGeneration: 0, KeyGeneration: 1, OwnSeal: []byte("old box")}); !errors.Is(err, credsync.ErrRollback) {
		t.Fatal("old pending generation accepted")
	}
	if err := restarted.ClearPendingRotation(); err != nil {
		t.Fatal(err)
	}
	if pending, err := restarted.PendingRotation(); err != nil || pending != nil {
		t.Fatal("completed proposal remains pending")
	}
	if highest, err := restarted.HighestKeyGeneration(); err != nil || highest != 2 {
		t.Fatal("acknowledgment lowered rollback floor")
	}
	if seal, err := restarted.AcceptedKeySeal(2); err != nil || string(seal) != "own signed seal" {
		t.Fatal("acknowledgment removed accepted box")
	}
}

func TestPendingCredentialRotationFailedWriteIsAtomic(t *testing.T) {
	ring := &sealCacheFailingKeyring{Keyring: keyring.NewArrayKeyring(nil)}
	original := openKeyring
	openKeyring = func() (keyring.Keyring, error) { return ring, nil }
	t.Cleanup(func() { openKeyring = original })
	trust := CredentialTrust{Namespace: "failed/pending/user/daemon"}
	if err := trust.SaveAcceptedKeySeal(1, []byte("original box")); err != nil {
		t.Fatal(err)
	}
	ring.shouldFail = true
	if err := trust.SavePendingRotation(credsync.KeyRotation{ExpectedGeneration: 1, KeyGeneration: 2, OwnSeal: []byte("new box")}); err == nil {
		t.Fatal("write error was hidden")
	}
	if highest, err := trust.HighestKeyGeneration(); err != nil || highest != 1 {
		t.Fatal("failed proposal advanced rollback floor")
	}
	if pending, err := trust.PendingRotation(); err != nil || pending != nil {
		t.Fatal("failed proposal remained pending")
	}
	if _, err := trust.AcceptedKeySeal(2); !errors.Is(err, credsync.ErrNotFound) {
		t.Fatal("failed proposal installed own seal")
	}
}

func TestPendingCredentialRotationFailedAcknowledgmentRemainsRetryable(t *testing.T) {
	ring := &sealCacheFailingKeyring{Keyring: keyring.NewArrayKeyring(nil)}
	original := openKeyring
	openKeyring = func() (keyring.Keyring, error) { return ring, nil }
	t.Cleanup(func() { openKeyring = original })
	trust := CredentialTrust{Namespace: "failed/ack/user/daemon"}
	if err := trust.SavePendingRotation(credsync.KeyRotation{ExpectedGeneration: 1, KeyGeneration: 2, OwnSeal: []byte("saved box")}); err != nil {
		t.Fatal(err)
	}
	ring.shouldFail = true
	if err := trust.ClearPendingRotation(); err == nil {
		t.Fatal("acknowledgment write failure hidden")
	}
	pending, err := trust.PendingRotation()
	if err != nil || pending == nil || pending.KeyGeneration != 2 || string(pending.OwnSeal) != "saved box" {
		t.Fatal("failed acknowledgment lost retry proposal")
	}
	if highest, err := trust.HighestKeyGeneration(); err != nil || highest != 2 {
		t.Fatal("failed acknowledgment lowered rollback floor")
	}
}
