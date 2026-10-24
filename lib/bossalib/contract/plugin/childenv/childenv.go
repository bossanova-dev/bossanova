// Package childenv builds child process environments from an approved baseline.
package childenv

import "strings"

// PassthroughEnvVar carries the operator policy from bossd to its agent plugins.
const PassthroughEnvVar = "BOSS_CHILD_ENV_PASSTHROUGH"

// Policy lists exact environment names and name prefixes a child may inherit.
// Prefixes are literal prefixes, without a wildcard suffix.
type Policy struct {
	Names    []string
	Prefixes []string
}

// Build keeps approved NAME=value entries in their original order, preserving
// values verbatim. Malformed entries are discarded. It does not read the process
// environment or add defaults; use ForPlugin to select the baseline policy.
func Build(environ []string, policy Policy) []string {
	result := make([]string, 0, len(environ))
	names := make(map[string]struct{}, len(policy.Names))
	for _, name := range policy.Names {
		names[name] = struct{}{}
	}
	for _, entry := range environ {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			continue
		}
		if _, allowed := names[name]; allowed {
			result = append(result, entry)
			continue
		}
		for _, prefix := range policy.Prefixes {
			if prefix != "" && strings.HasPrefix(name, prefix) {
				result = append(result, entry)
				break
			}
		}
	}
	return result
}

var baseline = Policy{
	Names: []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TERM", "TZ", "LANG", "SSH_AUTH_SOCK",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GH_TOKEN", "GITHUB_TOKEN", "GH_HOST",
		// Shared go-plugin handshake and parent watcher use these outside BOSS_*.
		"BOSSANOVA_PLUGIN", "BOSSANOVA_PLUGIN_PARENT_PID",
	},
	Prefixes: []string{"LC_", "XDG_", "BOSS_", "BOSSD_"},
}

var pluginPolicies = map[string]Policy{
	"claude": {
		Names:    []string{"CLOUD_ML_REGION", "GOOGLE_APPLICATION_CREDENTIALS", "CLAUDE_CONFIG_DIR"},
		Prefixes: []string{"ANTHROPIC_", "CLAUDE_CODE_", "AWS_", "VERTEX_REGION_"},
	},
	"codex": {Prefixes: []string{"OPENAI_", "CODEX_", "AZURE_OPENAI_"}},
	// OpenCode's provider authentication names are documented at
	// https://opencode.ai/docs/providers/; use exact names so one provider does
	// not implicitly grant unrelated credentials with the same vendor prefix.
	"opencode": {
		Names: []string{
			"ANTHROPIC_API_KEY", "OPENAI_API_KEY",
			"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_REGION",
			"AWS_BEARER_TOKEN_BEDROCK", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
			"AZURE_RESOURCE_NAME", "AZURE_COGNITIVE_SERVICES_RESOURCE_NAME",
			"CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_API_KEY", "CLOUDFLARE_API_TOKEN", "CLOUDFLARE_GATEWAY_ID",
			"DIGITALOCEAN_ACCESS_TOKEN", "GITLAB_AI_GATEWAY_URL", "GITLAB_INSTANCE_URL", "GITLAB_OAUTH_CLIENT_ID", "GITLAB_TOKEN",
			"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "VERTEX_LOCATION", "NVIDIA_API_KEY",
			"AICORE_DEPLOYMENT_ID", "AICORE_RESOURCE_GROUP", "AICORE_SERVICE_KEY", "AI_API_URL",
			"SNOWFLAKE_ACCOUNT", "SNOWFLAKE_CORTEX_PAT", "SNOWFLAKE_CORTEX_TOKEN", "XAI_API_KEY",
		},
		Prefixes: []string{"OPENCODE_"},
	},
	// The endpoint override is read by the Linear plugin's E2E transport.
	"linear": {Names: []string{"LINEAR_API_ENDPOINT"}},
}

// ForPlugin returns the baseline plus that plugin's provider variables and user
// passthrough names. A passthrough ending in _* grants that literal prefix; all
// other entries are exact names. A bare wildcard never grants the entire host
// environment. Each returned policy owns its slices.
func ForPlugin(pluginName string, passthrough []string) Policy {
	policy := Policy{
		Names:    append([]string{}, baseline.Names...),
		Prefixes: append([]string{}, baseline.Prefixes...),
	}
	extra := pluginPolicies[pluginName]
	policy.Names = append(policy.Names, extra.Names...)
	policy.Prefixes = append(policy.Prefixes, extra.Prefixes...)
	for _, entry := range passthrough {
		entry = strings.TrimSpace(entry)
		if entry == "" || entry == "*" {
			continue
		}
		if strings.HasSuffix(entry, "_*") {
			policy.Prefixes = append(policy.Prefixes, strings.TrimSuffix(entry, "*"))
		} else {
			policy.Names = append(policy.Names, entry)
		}
	}
	return policy
}
