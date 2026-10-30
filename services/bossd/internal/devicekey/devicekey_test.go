package devicekey

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"
)

type memoryDeviceRing struct {
	items             map[string][]byte
	readErr, writeErr error
}

func (r *memoryDeviceRing) Get(id string) ([]byte, error) {
	if r.readErr != nil {
		return nil, r.readErr
	}
	v, ok := r.items[id]
	if !ok {
		return nil, ErrNotFound
	}
	return bytes.Clone(v), nil
}
func (r *memoryDeviceRing) Set(id string, v []byte) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	r.items[id] = bytes.Clone(v)
	return nil
}
func TestLoadOrCreateAndRotation(t *testing.T) {
	ring := &memoryDeviceRing{items: map[string][]byte{}}
	first, err := LoadOrCreate(ring, "daemon-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(ring, "daemon-1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("key changed across loads")
	}
	if len(ring.items["agent-credential-device-key:daemon-1"]) != 32 {
		t.Fatal("key missing from expected item")
	}
	next, err := Rotate(ring, "daemon-1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Bytes(), next.Bytes()) {
		t.Fatal("rotation reused revoked key")
	}
	loaded, err := LoadOrCreate(ring, "daemon-1")
	if err != nil || !bytes.Equal(next.Bytes(), loaded.Bytes()) {
		t.Fatalf("rotation not persisted: %v", err)
	}
}
func TestMalformedKeyNotOverwritten(t *testing.T) {
	ring := &memoryDeviceRing{items: map[string][]byte{"agent-credential-device-key:d": []byte("short")}}
	if _, err := LoadOrCreate(ring, "d"); err == nil {
		t.Fatal("accepted malformed key")
	}
	if _, err := Rotate(ring, "d"); err == nil {
		t.Fatal("overwrote malformed key on rotation")
	}
	if string(ring.items["agent-credential-device-key:d"]) != "short" {
		t.Fatal("malformed item overwritten")
	}
}
func TestKeyringFailures(t *testing.T) {
	failure := errors.New("locked")
	for _, ring := range []*memoryDeviceRing{{readErr: failure}, {items: map[string][]byte{}, writeErr: failure}} {
		if _, err := LoadOrCreate(ring, "d"); !errors.Is(err, failure) {
			t.Fatalf("lost keyring error: %v", err)
		}
	}
	if _, err := LoadOrCreate(&memoryDeviceRing{}, ""); err == nil {
		t.Fatal("accepted empty daemon id")
	}
}
func TestFingerprint(t *testing.T) {
	identity := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	device := make([]byte, 32)
	const want = "HMCJ-C23A-AFAG-YYMF"
	if got := Fingerprint(identity, device); got != want {
		t.Fatalf("fingerprint = %s, want %s", got, want)
	}
	original := Fingerprint(identity, device)
	for i := 0; i < len(identity)*8; i++ {
		changed := bytes.Clone(identity)
		changed[i/8] ^= 1 << uint(i%8)
		if Fingerprint(changed, device) == original {
			t.Fatalf("identity bit %d not bound", i)
		}
	}
	for i := 0; i < len(device)*8; i++ {
		changed := bytes.Clone(device)
		changed[i/8] ^= 1 << uint(i%8)
		if Fingerprint(identity, changed) == original {
			t.Fatalf("device bit %d not bound", i)
		}
	}
}
func TestSignedStatements(t *testing.T) {
	private := ed25519.NewKeyFromSeed(make([]byte, 32))
	public := private.Public().(ed25519.PublicKey)
	request := EnrollmentRequest{DaemonID: "d", DevicePub: bytes.Repeat([]byte{1}, 32)}
	signature, err := SignEnrollmentRequest(private, request)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyEnrollmentRequest(public, request, signature) {
		t.Fatal("request rejected")
	}
	changed := request
	changed.DaemonID = "other"
	if VerifyEnrollmentRequest(public, changed, signature) {
		t.Fatal("daemon not bound")
	}
	changed = request
	changed.DevicePub = bytes.Repeat([]byte{2}, 32)
	if VerifyEnrollmentRequest(public, changed, signature) {
		t.Fatal("key not bound")
	}
	approval := Approval{ApproverDaemonID: "a", DaemonID: "d", DevicePub: request.DevicePub}
	approved, err := SignApproval(private, approval)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyApproval(public, approval, approved) {
		t.Fatal("approval rejected")
	}
	if VerifyApproval(public, approval, signature) {
		t.Fatal("cross-domain signature accepted")
	}
	alteredApproval := approval
	alteredApproval.DaemonID = "other"
	if VerifyApproval(public, alteredApproval, approved) {
		t.Fatal("approval target not bound")
	}
	alteredApproval = approval
	alteredApproval.DevicePub = bytes.Repeat([]byte{2}, 32)
	if VerifyApproval(public, alteredApproval, approved) {
		t.Fatal("approval key not bound")
	}
	approval.ApproverDaemonID = "other"
	if VerifyApproval(public, approval, approved) {
		t.Fatal("approver not bound")
	}
	revocation := Revocation{RevokerDaemonID: "a", DaemonID: "d"}
	revoked, err := SignRevocation(private, revocation)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyRevocation(public, revocation, revoked) {
		t.Fatal("revocation rejected")
	}
	alteredRevocation := revocation
	alteredRevocation.RevokerDaemonID = "other"
	if VerifyRevocation(public, alteredRevocation, revoked) {
		t.Fatal("revoker not bound")
	}
	revocation.DaemonID = "other"
	if VerifyRevocation(public, revocation, revoked) {
		t.Fatal("revocation target not bound")
	}
	if _, err := SignEnrollmentRequest(nil, request); err == nil {
		t.Fatal("invalid private key accepted")
	}
	if VerifyEnrollmentRequest(nil, request, signature) {
		t.Fatal("invalid public key accepted")
	}
}

func TestCanonicalStatementsDoNotAliasFieldBoundaries(t *testing.T) {
	private := ed25519.NewKeyFromSeed(make([]byte, 32))
	public := private.Public().(ed25519.PublicKey)
	signature, err := SignRevocation(private, Revocation{RevokerDaemonID: "ab", DaemonID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if VerifyRevocation(public, Revocation{RevokerDaemonID: "a", DaemonID: "bc"}, signature) {
		t.Fatal("ambiguous field boundaries accepted")
	}
}
