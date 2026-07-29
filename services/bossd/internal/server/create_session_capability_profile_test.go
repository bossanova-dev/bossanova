package server

import (
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestCreateSessionDoesNotExposeHeadlessCapabilityProfile keeps capability
// selection an internal launch policy rather than a public client API option.
func TestCreateSessionDoesNotExposeHeadlessCapabilityProfile(t *testing.T) {
	fields := (&bossanovav1.CreateSessionRequest{}).ProtoReflect().Descriptor().Fields()
	if field := fields.ByName("headless_capability_profile"); field != nil {
		t.Fatalf("CreateSessionRequest must not expose headless_capability_profile: %s", field.FullName())
	}
}
