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
	ReadWhen string // one-line "read this reference when…" routing hint for the skill index
	Commands []CommandDoc
}

// GroupSpec defines a group's id and human title and fixes its render order.
type GroupSpec struct {
	ID       string
	Title    string
	ReadWhen string // one-line "read this reference when…" routing hint for the skill index
}

// GroupOrder fixes the order and titles of generated "## " sections. Every
// command MUST be assigned (via cobra GroupID) to one of these ids; the
// extractor hard-errors on any command with an unknown or empty group.
var GroupOrder = []GroupSpec{
	{ID: "session", Title: "Session Management", ReadWhen: "Creating, listing, attaching to, merging or archiving a session"},
	{ID: "chat", Title: "Chat Control", ReadWhen: "Starting a chat, sending it a message, or reading a transcript"},
	{ID: "repo", Title: "Repository Management", ReadWhen: "Registering, cloning, updating or removing a repository"},
	{ID: "cron", Title: "Cron Jobs", ReadWhen: "Creating, editing, listing or firing a scheduled job"},
	{ID: "callback", Title: "GitHub Callbacks", ReadWhen: "Arming or inspecting a one-shot GitHub PR callback"},
	{ID: "broadcast", Title: "Broadcasts", ReadWhen: "Sending a broadcast or registering an outcome subscription"},
	{ID: "notes", Title: "Notes", ReadWhen: "Recording or harvesting durable repo-scoped notes"},
	{ID: "account", Title: "Account Management", ReadWhen: "Adding, testing, rotating or switching a provider account"},
	{ID: "trash", Title: "Trash Management", ReadWhen: "Resurrecting an archived session or emptying the trash"},
	{ID: "daemon", Title: "Daemon Management", ReadWhen: "Starting, stopping or inspecting bossd"},
	{ID: "mcp", Title: "MCP Server", ReadWhen: "Running or configuring the MCP server"},
	{ID: "skills", Title: "Skills", ReadWhen: "Installing or syncing the boss skill payload"},
	{ID: "settings", Title: "Settings & Auth", ReadWhen: "Changing global settings or authenticating"},
	{ID: "diagnostics", Title: "Diagnostics", ReadWhen: "Running the repair doctor, checks or other diagnostics"},
	{ID: "plugins", Title: "Plugins", ReadWhen: "Listing or inspecting loaded bossd plugins"},
	{ID: "other", Title: "Other", ReadWhen: "Anything unclassified (e.g. `boss version`)"},
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
