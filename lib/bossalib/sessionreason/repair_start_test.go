package sessionreason

import (
	"testing"
	"time"
)

func TestRepairCouldNotStart(t *testing.T) {
	for _, reason := range []string{
		RepairCouldNotStart(5, 31*time.Minute, "ABCDEF0123456789", "fedcba9876543210", "chat busy\nprivate detail"),
		"automatic repair could not start 5 times in a row over 31m0s on ABCDEF012345; last refusal: chat busy",
	} {
		block, ok := ParseRepairCouldNotStart(&reason)
		if !ok || block.HeadMoved("abcdef0123456789") || block.HeadMoved("") || !block.HeadMoved("new-head") {
			t.Fatalf("head key: %+v %v", block, ok)
		}
		if block.BaseMoved("") || (block.BaseSHA != "" && (block.BaseMoved("FEDCBA9876543210") || !block.BaseMoved("new-base"))) {
			t.Fatalf("base key: %+v", block)
		}
	}
	other := "automatic repair failed 5 times"
	if _, ok := ParseRepairCouldNotStart(&other); ok {
		t.Fatal("matched terminal reason")
	}
	if _, ok := ParseRepairCouldNotStart(nil); ok {
		t.Fatal("matched nil")
	}
}
