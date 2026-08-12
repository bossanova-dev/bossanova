package agent

import (
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestStartAgentRunRequestExposesVersionedHeadlessCapabilityProfile(t *testing.T) {
	field := (&bossanovav1.StartAgentRunRequest{}).ProtoReflect().Descriptor().Fields().ByName("headless_capability_profile")
	if field == nil {
		t.Fatal("StartAgentRunRequest must expose headless_capability_profile")
	}
	if field.Kind() != protoreflect.EnumKind {
		t.Fatalf("headless_capability_profile kind = %s, want enum", field.Kind())
	}
	if got := bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1.Number(); got != 1 {
		t.Fatalf("tracker-plan-attachment profile number = %d, want 1", got)
	}
}

// Boss no longer owns MCP configuration: each harness discovers servers its own
// native way and the repo declares them, so the daemon has no config to inject.
// The two fields that carried one are RETIRED, not merely unused — an older
// bossd can still serialize them to a newer plugin, so their numbers and names
// must stay reserved and can never be reused for something else.
//
// Asserting absence alone would pass on a typo, so this checks both halves: the
// names resolve to no field, AND the numbers are reserved rather than free.
func TestStartAgentRunRequestRetiresManagedMcpConfigFields(t *testing.T) {
	desc := (&bossanovav1.StartAgentRunRequest{}).ProtoReflect().Descriptor()
	for _, name := range []string{"managed_mcp_config_path", "is_strict_managed_mcp_config"} {
		if f := desc.Fields().ByName(protoreflect.Name(name)); f != nil {
			t.Fatalf("StartAgentRunRequest must not expose %s (number %d)", name, f.Number())
		}
	}
	reservedNames := map[string]bool{}
	for i := range desc.ReservedNames().Len() {
		reservedNames[string(desc.ReservedNames().Get(i))] = true
	}
	for _, name := range []string{"managed_mcp_config_path", "is_strict_managed_mcp_config"} {
		if !reservedNames[name] {
			t.Fatalf("%s must be reserved, not merely removed", name)
		}
	}
	reservedNumbers := map[protoreflect.FieldNumber]bool{}
	for i := range desc.ReservedRanges().Len() {
		r := desc.ReservedRanges().Get(i)
		for n := r[0]; n < r[1]; n++ {
			reservedNumbers[n] = true
		}
	}
	for _, n := range []protoreflect.FieldNumber{9, 10} {
		if !reservedNumbers[n] {
			t.Fatalf("field number %d must be reserved so it cannot be reused", n)
		}
	}
}
