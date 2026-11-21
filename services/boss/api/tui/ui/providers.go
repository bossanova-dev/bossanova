package ui

// Provider describes a coding-agent provider shown on the startup
// provider screen.
type Provider struct {
	Name        string // display label, e.g. "Claude"
	Plugin      string // matching plugin name, e.g. "claude"
	Command     string // executable expected on PATH
	InstallCmd  string // shell command to install the underlying CLI
	InstallURL  string // documentation URL for the provider
	Description string // one-liner shown under the row
}

// Providers is the static list rendered by the onboarding screen.
// Order matches the install order recommended in docs (Claude, Codex, then
// Antigravity).
var Providers = []Provider{
	{
		Name:        "Claude",
		Plugin:      "claude",
		Command:     "claude",
		InstallCmd:  "npm install -g @anthropic-ai/claude-code",
		InstallURL:  "https://docs.claude.com/en/docs/claude-code/overview",
		Description: "Anthropic's Claude Code CLI",
	},
	{
		Name:        "Codex",
		Plugin:      "codex",
		Command:     "codex",
		InstallCmd:  "npm install -g @openai/codex",
		InstallURL:  "https://github.com/openai/codex",
		Description: "OpenAI's Codex CLI",
	},
	{
		Name:        "Antigravity",
		Plugin:      "antigravity",
		Command:     "agy",
		InstallCmd:  "curl -fsSL https://antigravity.google/cli/install.sh | bash",
		InstallURL:  "https://antigravity.google/docs/getting-started?tab=cli",
		Description: "Google's Antigravity CLI",
	},
}

// ProviderFor returns the provider whose plugin is named plugin.
func ProviderFor(plugin string) (Provider, bool) {
	for _, p := range Providers {
		if p.Plugin == plugin {
			return p, true
		}
	}
	return Provider{}, false
}
