package streamseal

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func testKeys(t *testing.T) (*ecdh.PrivateKey, ed25519.PrivateKey, AttachContext, []byte) {
	t.Helper()
	viewer, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewAttachKey()
	if err != nil {
		t.Fatal(err)
	}
	return viewer, identity, AttachContext{"daemon", "viewer", "attach", "chat"}, key
}

func TestAttachKeyAuthentication(t *testing.T) {
	viewer, identity, ac, key := testKeys(t)
	sealed, err := SealAttachKey(viewer.PublicKey().Bytes(), key, ac, identity)
	if err != nil {
		t.Fatal(err)
	}
	pub := identity.Public().(ed25519.PublicKey)
	got, err := OpenAttachKey(viewer, pub, ac, sealed)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("round trip: %x, %v", got, err)
	}
	for i := range 4 {
		changed := ac
		fields := []*string{&changed.DaemonID, &changed.ViewerDeviceID, &changed.AttachID, &changed.ChatID}
		*fields[i] += "changed"
		if _, err := OpenAttachKey(viewer, pub, changed, sealed); err == nil {
			t.Fatalf("context field %d accepted", i)
		}
	}
	other, otherIdentity, _, _ := testKeys(t)
	if _, err := OpenAttachKey(other, pub, ac, sealed); err == nil {
		t.Fatal("wrong viewer accepted")
	}
	if _, err := OpenAttachKey(viewer, otherIdentity.Public().(ed25519.PublicKey), ac, sealed); err == nil {
		t.Fatal("wrong signer accepted")
	}
	for _, signature := range []bool{false, true} {
		changed := SealedAttachKey{bytes.Clone(sealed.Box), bytes.Clone(sealed.Signature)}
		if signature {
			changed.Signature[0] ^= 1
		} else {
			changed.Box[0] ^= 1
		}
		if _, err := OpenAttachKey(viewer, pub, ac, changed); err == nil {
			t.Fatal("tampering accepted")
		}
	}
}
