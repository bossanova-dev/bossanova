package upstream

import (
	"bytes"
	"errors"
	"testing"

	"github.com/99designs/keyring"
	"github.com/recurser/bossd/internal/devicekey"
)

func TestDeviceKeySharedKeyring(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	original := openKeyring
	openKeyring = func() (keyring.Keyring, error) { return ring, nil }
	t.Cleanup(func() { openKeyring = original })
	first, err := LoadDeviceKey("d")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadDeviceKey("d")
	if err != nil || !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("load did not reuse key: %v", err)
	}
	rotated, err := RotateDeviceKey("d")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Bytes(), rotated.Bytes()) {
		t.Fatal("revoked key reused")
	}
	if _, err := ring.Get("agent-credential-device-key:d"); err != nil {
		t.Fatal(err)
	}
	if _, err := (DeviceKeyring{Ring: ring}).Get("missing"); !errors.Is(err, devicekey.ErrNotFound) {
		t.Fatalf("missing item = %v", err)
	}
}
