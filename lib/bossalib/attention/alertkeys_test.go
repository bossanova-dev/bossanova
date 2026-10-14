package attention

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestRotationAlertKey(t *testing.T) {
	if got := RotationAlertKey(pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED.String()); got != "rotation:auth_invalidated" {
		t.Fatalf("RotationAlertKey = %q, want rotation:auth_invalidated", got)
	}
	keys := map[string]bool{
		RotationAlertKey(pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED.String()): true,
		RotationAlertKey(pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED.String()):    true,
		RepairFailedAlertKey:  true,
		RepairRefusedAlertKey: true,
		BlockedAlertKey:       true,
	}
	if len(keys) != 5 {
		t.Fatalf("alert keys collide: %v", keys)
	}
}
