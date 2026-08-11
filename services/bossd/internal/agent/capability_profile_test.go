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

func TestStartAgentRunRequestExposesManagedMcpConfig(t *testing.T) {
	fields := (&bossanovav1.StartAgentRunRequest{}).ProtoReflect().Descriptor().Fields()
	for _, want := range []struct {
		name string
		kind protoreflect.Kind
	}{
		{name: "managed_mcp_config_path", kind: protoreflect.StringKind},
		{name: "is_strict_managed_mcp_config", kind: protoreflect.BoolKind},
	} {
		field := fields.ByName(protoreflect.Name(want.name))
		if field == nil {
			t.Fatalf("StartAgentRunRequest must expose %s", want.name)
		}
		if field.Kind() != want.kind {
			t.Fatalf("%s kind = %s, want %s", want.name, field.Kind(), want.kind)
		}
	}
}
