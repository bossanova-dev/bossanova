package streamseal

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"math"
	"testing"
)

func TestAttachRejectsInvalidInputs(t *testing.T) {
	viewer, identity, ac, key := testKeys(t)
	for _, bad := range []struct {
		pub, key []byte
		identity ed25519.PrivateKey
		context  AttachContext
	}{
		{nil, key, identity, ac},
		{make([]byte, 32), key, identity, ac},
		{viewer.PublicKey().Bytes(), key[:31], identity, ac},
		{viewer.PublicKey().Bytes(), key, nil, ac},
		{viewer.PublicKey().Bytes(), key, identity, AttachContext{}},
	} {
		if _, err := SealAttachKey(bad.pub, bad.key, bad.context, bad.identity); err == nil {
			t.Fatal("invalid seal inputs accepted")
		}
	}
	sealed, err := SealAttachKey(viewer.PublicKey().Bytes(), key, ac, identity)
	if err != nil {
		t.Fatal(err)
	}
	pub := identity.Public().(ed25519.PublicKey)
	if _, err := OpenAttachKey(nil, pub, ac, sealed); err == nil {
		t.Fatal("nil viewer accepted")
	}
	if _, err := OpenAttachKey(viewer, nil, ac, sealed); err == nil {
		t.Fatal("nil identity accepted")
	}
	if _, err := OpenAttachKey(viewer, pub, AttachContext{}, sealed); err == nil {
		t.Fatal("empty context accepted")
	}
	otherCurve, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAttachKey(otherCurve, pub, ac, sealed); err == nil {
		t.Fatal("wrong curve accepted")
	}
	hp, err := hpke.NewDHKEMPublicKey(viewer.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	shortBox, err := hpke.Seal(hp, hpke.HKDFSHA256(), hpke.AES256GCM(), ac.info(), key[:31])
	if err != nil {
		t.Fatal(err)
	}
	for _, box := range [][]byte{shortBox, {1, 2, 3}} {
		signed := SealedAttachKey{box, ed25519.Sign(identity, ac.statement(viewer.PublicKey().Bytes(), box))}
		if _, err := OpenAttachKey(viewer, pub, ac, signed); err == nil {
			t.Fatal("invalid signed payload accepted")
		}
	}
}

func TestFrameInputsAndReceiveExhaustion(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	for _, bad := range []struct {
		key       []byte
		direction Direction
		attachID  string
	}{
		{nil, DaemonToViewer, "attach"},
		{key[:31], DaemonToViewer, "attach"},
		{key, 0, "attach"},
		{key, 3, "attach"},
		{key, DaemonToViewer, ""},
	} {
		if _, err := NewFrameSealer(bad.key, bad.direction, bad.attachID); err == nil {
			t.Fatal("invalid sealer accepted")
		}
		if _, err := NewFrameOpener(bad.key, bad.direction, bad.attachID); err == nil {
			t.Fatal("invalid opener accepted")
		}
	}
	s, _ := NewFrameSealer(key, DaemonToViewer, "attach")
	o, _ := NewFrameOpener(key, DaemonToViewer, "attach")
	s.counter, o.counter = math.MaxUint64-1, math.MaxUint64-1
	counter, ct, err := s.Seal([]byte("last"))
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := o.Open(counter, ct); err != nil || string(plain) != "last" {
		t.Fatalf("last frame: %q %v", plain, err)
	}
	if _, err := o.Open(0, ct); err == nil {
		t.Fatal("receive counter wrapped")
	}
}

func TestContextEncodingAndFingerprints(t *testing.T) {
	a := AttachContext{"ab", "c", "attach", "chat"}
	b := AttachContext{"a", "bc", "attach", "chat"}
	if bytes.Equal(a.info(), b.info()) || bytes.Equal(a.statement([]byte{1}, []byte{2}), b.statement([]byte{1}, []byte{2})) {
		t.Fatal("ambiguous context encoding")
	}
	if bytes.Equal(a.statement([]byte{1, 2}, []byte{3}), a.statement([]byte{1}, []byte{2, 3})) {
		t.Fatal("ambiguous key/box encoding")
	}
	pub := bytes.Repeat([]byte{1}, 32)
	if ViewerFingerprint("", pub) == DaemonFingerprint(pub) {
		t.Fatal("fingerprint domains collide")
	}
	if ViewerFingerprint("one", pub) == ViewerFingerprint("two", pub) {
		t.Fatal("fingerprint ignores viewer id")
	}
	one, err := NewAttachKey()
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewAttachKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 32 || len(two) != 32 || bytes.Equal(one, two) {
		t.Fatal("attach keys are not fresh 32-byte keys")
	}
}
