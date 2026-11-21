package settings

import "github.com/recurser/boss/internal/domain/rpc"

// fallbackAgentInfos mirrors the provider plugins' user settings so their
// rows render while the daemon has not loaded the plugin.
func fallbackAgentInfos() []rpc.AgentInfo {
	return []rpc.AgentInfo{
		{
			Name: "claude",
			UserSettings: []rpc.UserSetting{
				{
					Key:          "dangerously_skip_permissions",
					Label:        "Skip permission prompts",
					Description:  "Pass --dangerously-skip-permissions to claude. Use only in trusted worktrees.",
					Type:         rpc.SettingTypeBool,
					DefaultValue: "false",
				},
				{
					Key:          "model",
					Label:        "Model",
					Description:  "Fallback claude --model for runs without their own. Empty uses the claude CLI default.",
					Type:         rpc.SettingTypeString,
					DefaultValue: "",
				},
			},
		},
		{
			Name: "codex",
			UserSettings: []rpc.UserSetting{
				{
					Key:           "sandbox",
					Label:         "Sandbox mode",
					Description:   "Codex --sandbox mode. Empty uses codex default (no --sandbox flag passed).",
					Type:          rpc.SettingTypeEnum,
					AllowedValues: []string{"", "read-only", "workspace-write", "danger-full-access"},
					DefaultValue:  "",
				},
				{
					Key:           "approval",
					Label:         "Approval policy",
					Description:   "Codex --ask-for-approval policy. Empty uses codex default (no flag passed).",
					Type:          rpc.SettingTypeEnum,
					AllowedValues: []string{"", "untrusted", "on-failure", "on-request", "never"},
					DefaultValue:  "",
				},
				{
					Key:          "model",
					Label:        "Model",
					Description:  "Codex --model selection. Empty uses codex default.",
					Type:         rpc.SettingTypeString,
					DefaultValue: "",
				},
				{
					Key:          "dangerously_bypass_approvals_and_sandbox",
					Label:        "Bypass approvals & sandbox (dangerous)",
					Description:  "Pass --dangerously-bypass-approvals-and-sandbox to codex. Overrides sandbox/approval. Use only in trusted worktrees.",
					Type:         rpc.SettingTypeBool,
					DefaultValue: "false",
				},
			},
		},
		{
			Name: "antigravity",
			UserSettings: []rpc.UserSetting{
				{
					Key:          "dangerously_skip_permissions",
					Label:        "Skip permission prompts",
					Description:  "Pass --dangerously-skip-permissions to agy. Use only in trusted worktrees.",
					Type:         rpc.SettingTypeBool,
					DefaultValue: "false",
				},
				{
					Key:           "mode",
					Label:         "Mode",
					Description:   "agy --mode. Empty uses the agy default (no flag passed).",
					Type:          rpc.SettingTypeEnum,
					AllowedValues: []string{"", "accept-edits", "auto", "plan"},
					DefaultValue:  "",
				},
				{
					Key:          "model",
					Label:        "Model",
					Description:  "Fallback agy --model for runs without their own. Empty uses the agy default.",
					Type:         rpc.SettingTypeString,
					DefaultValue: "",
				},
			},
		},
	}
}
