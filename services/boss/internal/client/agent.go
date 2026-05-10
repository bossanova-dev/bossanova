package client

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AgentInfo is the package-local representation of a registered agent
// runner. Mirrors pb.AgentInfo but lets views avoid importing proto types.
type AgentInfo struct {
	Name         string
	Version      string
	UserSettings []UserSetting
}

// UserSetting describes a single configurable knob exposed by an agent
// runner plugin. Mirrors pb.UserSetting.
type UserSetting struct {
	Key           string
	Label         string
	Description   string
	DefaultValue  string
	Type          SettingType
	AllowedValues []string
}

// SettingType enumerates the renderable kinds of UserSetting.
type SettingType int

const (
	// SettingTypeUnspecified is the zero value; treat as opaque/text.
	SettingTypeUnspecified SettingType = iota
	// SettingTypeBool renders as a checkbox.
	SettingTypeBool
	// SettingTypeString renders as a text input.
	SettingTypeString
	// SettingTypeEnum renders as a cycle picker over AllowedValues.
	SettingTypeEnum
)

// agentInfoFromProto converts the proto AgentInfo into the package-local type.
func agentInfoFromProto(p *pb.AgentInfo) AgentInfo {
	if p == nil {
		return AgentInfo{}
	}
	out := AgentInfo{
		Name:    p.GetName(),
		Version: p.GetVersion(),
	}
	for _, s := range p.GetUserSettings() {
		out.UserSettings = append(out.UserSettings, userSettingFromProto(s))
	}
	return out
}

// userSettingFromProto maps a proto UserSetting into the local one.
func userSettingFromProto(p *pb.UserSetting) UserSetting {
	if p == nil {
		return UserSetting{}
	}
	return UserSetting{
		Key:           p.GetKey(),
		Label:         p.GetLabel(),
		Description:   p.GetDescription(),
		DefaultValue:  p.GetDefaultValue(),
		Type:          settingTypeFromProto(p.GetType()),
		AllowedValues: append([]string(nil), p.GetAllowedValues()...),
	}
}

// settingTypeFromProto maps a proto enum to the local enum.
func settingTypeFromProto(t pb.UserSettingType) SettingType {
	switch t {
	case pb.UserSettingType_USER_SETTING_TYPE_BOOL:
		return SettingTypeBool
	case pb.UserSettingType_USER_SETTING_TYPE_STRING:
		return SettingTypeString
	case pb.UserSettingType_USER_SETTING_TYPE_ENUM:
		return SettingTypeEnum
	default:
		return SettingTypeUnspecified
	}
}
