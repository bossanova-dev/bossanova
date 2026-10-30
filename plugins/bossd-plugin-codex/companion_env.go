package main

// Keep in sync with domain.SentinelAPIKey. Plugins cannot import host packages.
// This fixed marker authorizes nothing upstream and is safe to carry in argv.
const companionSentinelAPIKey = "sk-ant-boss-managed-rotation-sentinel-00000000000000"

func appendCompanionShellEnv(args []string, enabled bool) []string {
	if !enabled {
		return args
	}
	// Codex applies set after its KEY/SECRET/TOKEN environment exclusions.
	return appendConfigOverride(args, `shell_environment_policy.set.ANTHROPIC_API_KEY="`+companionSentinelAPIKey+`"`)
}
