package client

import (
	"reflect"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAgentInfoFromProto_MapsAllFields(t *testing.T) {
	in := &pb.AgentInfo{
		Name:    "claude",
		Version: "v1.2.3",
		UserSettings: []*pb.UserSetting{
			{
				Key:          "dangerously_skip_permissions",
				Label:        "Skip permissions",
				Description:  "Run claude with --dangerously-skip-permissions",
				Type:         pb.UserSettingType_USER_SETTING_TYPE_BOOL,
				DefaultValue: "false",
			},
			{
				Key:           "model",
				Label:         "Model",
				Type:          pb.UserSettingType_USER_SETTING_TYPE_ENUM,
				DefaultValue:  "sonnet",
				AllowedValues: []string{"sonnet", "opus", "haiku"},
			},
			{
				Key:   "extra_args",
				Label: "Extra CLI args",
				Type:  pb.UserSettingType_USER_SETTING_TYPE_STRING,
			},
		},
	}

	got := agentInfoFromProto(in)

	if got.Name != "claude" {
		t.Errorf("Name = %q, want claude", got.Name)
	}
	if got.Version != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", got.Version)
	}
	if len(got.UserSettings) != 3 {
		t.Fatalf("UserSettings len = %d, want 3", len(got.UserSettings))
	}

	// Bool setting.
	bs := got.UserSettings[0]
	if bs.Type != SettingTypeBool {
		t.Errorf("settings[0].Type = %v, want SettingTypeBool", bs.Type)
	}
	if bs.Key != "dangerously_skip_permissions" {
		t.Errorf("settings[0].Key = %q", bs.Key)
	}

	// Enum setting + allowed values.
	es := got.UserSettings[1]
	if es.Type != SettingTypeEnum {
		t.Errorf("settings[1].Type = %v, want SettingTypeEnum", es.Type)
	}
	if !reflect.DeepEqual(es.AllowedValues, []string{"sonnet", "opus", "haiku"}) {
		t.Errorf("settings[1].AllowedValues = %v", es.AllowedValues)
	}

	// String setting.
	ss := got.UserSettings[2]
	if ss.Type != SettingTypeString {
		t.Errorf("settings[2].Type = %v, want SettingTypeString", ss.Type)
	}
}

func TestAgentInfoFromProto_NilInput(t *testing.T) {
	got := agentInfoFromProto(nil)
	if got.Name != "" || len(got.UserSettings) != 0 {
		t.Errorf("nil input should produce zero AgentInfo, got %+v", got)
	}
}

func TestSettingTypeFromProto_Unspecified(t *testing.T) {
	if got := settingTypeFromProto(pb.UserSettingType_USER_SETTING_TYPE_UNSPECIFIED); got != SettingTypeUnspecified {
		t.Errorf("unspecified = %v, want SettingTypeUnspecified", got)
	}
}
