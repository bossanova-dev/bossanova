package upstream

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/99designs/keyring"
	"github.com/recurser/bossd/internal/credsync"
)

func TestCredentialTrustPersistsOnlyExplicitPins(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	original := openKeyring
	openKeyring = func() (keyring.Keyring, error) { return ring, nil }
	t.Cleanup(func() { openKeyring = original })
	trust := CredentialTrust{}
	if _, err := trust.PinnedIdentity("unconfirmed"); err == nil {
		t.Fatal("unconfirmed identity trusted")
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := PinCredentialIdentity("root", public); err != nil {
		t.Fatal(err)
	}
	restored, err := (CredentialTrust{}).PinnedIdentity("root")
	if err != nil || !restored.Equal(public) {
		t.Fatal("identity pin not durable")
	}
	d := credsync.Device{DaemonID: "root", Status: "enrolled", ApprovedByDaemonID: "root", IdentityPublicKey: public, DevicePublicKey: []byte("test public encryption key"), ApprovalSignature: []byte("test verified statement")}
	if err := trust.SaveApprovedDevice(d); err != nil {
		t.Fatal(err)
	}
	restoredDevice, err := (CredentialTrust{}).ApprovedDevice("root")
	if err != nil {
		t.Fatal(err)
	}
	if restoredDevice.DaemonID != d.DaemonID || !bytes.Equal(restoredDevice.ApprovalSignature, d.ApprovalSignature) || !bytes.Equal(restoredDevice.DevicePublicKey, d.DevicePublicKey) {
		t.Fatal("verified approval changed after restart")
	}
}
