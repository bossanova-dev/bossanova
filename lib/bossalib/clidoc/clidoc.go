// Package clidoc holds an audience-neutral documentation model for the boss
// CLI, plus a prose registry keyed by command path. It has NO dependency on
// cobra or on any services/* package, so it can be imported by both the boss
// CLI skill generator (services/boss) and the MCP server (lib/bossalib/bossmcp,
// BOS-37) as a single source of truth for command/flag prose.
package clidoc

// Example is one runnable command line plus optional explanation.
type Example struct {
	Command     string
	Explanation string // optional; "" renders as a bare fenced command
}

// FlagDoc is the neutral description of a single command flag.
type FlagDoc struct {
	Name           string // long name, without leading "--"
	Shorthand      string // single-char shorthand, without leading "-"; "" if none
	Usage          string
	Default        string // rendered default; "" to omit
	Required       bool
	Deprecated     bool
	DeprecationMsg string
}

// CommandDoc is the neutral description of a single command.
type CommandDoc struct {
	Path           string // "boss ls"
	Synopsis       string // one line (cobra Short)
	UsageLine      string // "boss ls [flags]"
	Long           string // extended prose (from Registry); "" if none
	Aliases        []string
	Examples       []Example
	Flags          []FlagDoc
	IsGroup        bool // group command (no Run/RunE, only subcommands)
	GroupID        string
	DeprecationMsg string // "" unless deprecated
}

// GroupDoc is a rendered section: a titled group of commands.
type GroupDoc struct {
	ID       string
	Title    string
	Commands []CommandDoc
}

// GroupSpec defines a group's id and human title and fixes its render order.
type GroupSpec struct {
	ID    string
	Title string
}

// GroupOrder fixes the order and titles of generated "## " sections. Every
// command MUST be assigned (via cobra GroupID) to one of these ids; the
// extractor hard-errors on any command with an unknown or empty group.
var GroupOrder = []GroupSpec{
	{ID: "session", Title: "Session Management"},
	{ID: "chat", Title: "Chat Control"},
	{ID: "repo", Title: "Repository Management"},
	{ID: "cron", Title: "Cron Jobs"},
	{ID: "account", Title: "Account Management"},
	{ID: "trash", Title: "Trash Management"},
	{ID: "daemon", Title: "Daemon Management"},
	{ID: "mcp", Title: "MCP Server"},
	{ID: "skills", Title: "Skills"},
	{ID: "settings", Title: "Settings & Auth"},
	{ID: "diagnostics", Title: "Diagnostics"},
	{ID: "plugins", Title: "Plugins"},
	{ID: "other", Title: "Other"},
}

// GroupTitle returns the human title for a group id.
func GroupTitle(id string) (string, bool) {
	for _, g := range GroupOrder {
		if g.ID == id {
			return g.Title, true
		}
	}
	return "", false
}
