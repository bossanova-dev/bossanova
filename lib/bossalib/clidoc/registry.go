package clidoc

// Prose is the audience-neutral human/agent-facing text for a command. It is
// keyed by command path in Registry. Keep entries terse and behavioural — this
// text is reused verbatim by the /boss skill and (BOS-37) by the MCP server.
type Prose struct {
	Long     string
	Examples []Example
}

// Registry maps a command path ("boss ls") to its extended prose. Commands
// with no entry render with their cobra Short synopsis only. The prose here is
// ported verbatim from the previously hand-written /boss SKILL.md; the renderer
// (services/boss/cmd/skillgen) merges it with the structural facts extracted
// from the cobra command tree.
var Registry = map[string]Prose{
	// --- Session Management ---
	"boss ls": {
		Long: "An extra `AGENT` column appears only when at least one listed " +
			"session uses an agent that differs from the user's " +
			"`Settings.DefaultAgent`. In the common single-agent case the column " +
			"is hidden so the table stays compact.",
		Examples: []Example{
			{Command: "boss ls"},
			{Command: "boss ls --repo my-repo --state running,paused"},
			{Command: "boss ls --archived"},
		},
	},
	"boss show": {
		Examples: []Example{{Command: "boss show abc123"}},
	},
	"boss new": {
		Long: "Launches the interactive session creation flow. " +
			"When both --repo and --prompt are provided the command runs " +
			"non-interactively: it creates the session, streams any setup output " +
			"to stderr, and prints the session-id and chat-id to stdout, then " +
			"exits. Combine with --detach (implicit when both flags are set) for " +
			"scripting. Use --agent to override the default agent plugin.",
		Examples: []Example{
			{Command: "boss new"},
			{Command: "boss new --agent opencode"},
			{
				Command:     "boss new --repo my-repo --prompt \"refactor the auth module\" --detach",
				Explanation: "Create a session non-interactively and print its ids",
			},
			{
				Command: "boss new --agent codex --repo my-repo " +
					"--prompt \"review this PR for security issues\" --detach",
				Explanation: "Ask Codex for a second opinion; capture ids for boss chat wait",
			},
		},
	},
	"boss attach": {
		Long:     "Attaches to a running session's terminal.",
		Examples: []Example{{Command: "boss attach abc123"}},
	},
	"boss chats": {
		Examples: []Example{{Command: "boss chats abc123"}},
	},
	"boss archive": {
		Examples: []Example{{Command: "boss archive abc123"}},
	},

	// --- Chat Control ---
	"boss chat send": {
		Long: "Delivers a follow-up message to a running chat identified by a " +
			"session id or agent_session_id (the chat-id printed by `boss new --detach`). " +
			"When given a session id, boss targets that session's primary chat. " +
			"The daemon wakes a sleeping chat before pasting the message.",
		Examples: []Example{
			{Command: "boss chat send <session-id|chat-id> \"please also add tests\""},
		},
	},
	"boss chat show": {
		Long: "Prints the full conversation transcript for a chat or session's primary chat. " +
			"Use --result-only to print just the final assistant response text " +
			"(suitable for scripting). Use --limit to cap the number of messages.",
		Examples: []Example{
			{Command: "boss chat show <session-id|chat-id>"},
			{Command: "boss chat show <session-id|chat-id> --result-only"},
			{Command: "boss chat show <session-id|chat-id> --limit 10"},
		},
	},
	"boss chat wait": {
		Long: "Blocks until the chat identified by a session id or agent_session_id becomes " +
			"idle or is waiting for input, then prints the final assistant result. " +
			"Polls chat status every few seconds. Use --timeout to limit wait time. " +
			"Typical recipe: `boss new --agent codex --repo R --prompt P --detach` " +
			"then `boss chat wait <session-id|chat-id>` to collect the result.",
		Examples: []Example{
			{Command: "boss chat wait <session-id|chat-id>"},
			{Command: "boss chat wait <session-id|chat-id> --timeout 10m"},
			{
				Command: "CHAT=$(boss new --agent codex --repo my-repo " +
					"--prompt \"second opinion on PR #42\" --detach | " +
					"awk '/^chat-id/{print $2}') && boss chat wait $CHAT",
				Explanation: "Full cross-agent second-opinion recipe",
			},
		},
	},

	// --- Repository Management ---
	"boss repo add": {
		Examples: []Example{{Command: "boss repo add"}},
	},
	"boss repo ls": {
		Examples: []Example{{Command: "boss repo ls"}},
	},
	"boss repo remove": {
		Examples: []Example{{Command: "boss repo remove my-repo"}},
	},
	"boss repo update": {
		Examples: []Example{
			{Command: `boss repo update my-repo --name "My Repo" --merge-strategy squash`},
			{Command: "boss repo update my-repo --auto-merge-dependabot"},
		},
	},

	// --- Trash Management ---
	"boss trash ls": {
		Examples: []Example{{Command: "boss trash ls"}},
	},
	"boss trash restore": {
		Long:     "Restores an archived session, recreating its worktree.",
		Examples: []Example{{Command: "boss trash restore abc123"}},
	},
	"boss trash delete": {
		Examples: []Example{
			{Command: "boss trash delete abc123"},
			{Command: "boss trash delete abc123 --yes"},
		},
	},
	"boss trash empty": {
		Examples: []Example{
			{Command: "boss trash empty"},
			{Command: "boss trash empty --older-than 30d"},
		},
	},

	// --- Daemon Management ---
	"boss daemon install": {
		Examples: []Example{
			{Command: "boss daemon install"},
			{Command: "boss daemon install --force"},
		},
	},
	"boss daemon uninstall": {
		Examples: []Example{{Command: "boss daemon uninstall"}},
	},
	"boss daemon status": {
		Examples: []Example{{Command: "boss daemon status"}},
	},
	"boss daemon start": {
		Long: "No-op if it's already running. Falls back to spawning bossd " +
			"directly if it isn't installed as a LaunchAgent.",
		Examples: []Example{{Command: "boss daemon start"}},
	},
	"boss daemon stop": {
		Long: "Stops the bossd daemon for the current profile via the platform " +
			"service manager or profile metadata. Idempotent — quietly succeeds " +
			"if the daemon is already stopped or not installed. Use " +
			"`--all-standalone` only for explicit cleanup of every user-owned " +
			"standalone bossd process across profiles.",
		Examples: []Example{
			{Command: "boss daemon stop"},
			{Command: "boss daemon stop --all-standalone"},
		},
	},
	"boss daemon restart": {
		Long: "Restarts the bossd daemon via the platform service manager. " +
			"Errors out if the daemon isn't installed.",
		Examples: []Example{{Command: "boss daemon restart"}},
	},

	// --- MCP Server ---
	"boss mcp": {
		Long: "Manages the local MCP server, which exposes the boss operations " +
			"as MCP tools over Streamable HTTP for MCP-aware hosts. It runs as an " +
			"auto-starting service (launchd on macOS, systemd on Linux) and " +
			"proxies through the local bossd daemon's Unix socket.",
	},
	"boss mcp install": {
		Long: "Installs the MCP server as an auto-starting service and starts " +
			"it. Use `--force` to overwrite an existing service file, and " +
			"`--port` to change the loopback port (default 8765). The server " +
			"listens on `http://127.0.0.1:<port>/mcp`.",
		Examples: []Example{
			{Command: "boss mcp install"},
			{Command: "boss mcp install --force"},
			{Command: "boss mcp install --port 8888"},
		},
	},
	"boss mcp uninstall": {
		Examples: []Example{{Command: "boss mcp uninstall"}},
	},
	"boss mcp status": {
		Examples: []Example{{Command: "boss mcp status"}},
	},
	"boss mcp start": {
		Examples: []Example{{Command: "boss mcp start"}},
	},
	"boss mcp stop": {
		Long:     "Stops the running MCP server, leaving its service file in place. Idempotent.",
		Examples: []Example{{Command: "boss mcp stop"}},
	},

	// --- Settings & Auth ---
	"boss settings": {
		Examples: []Example{
			{Command: "boss settings"},
			{Command: "boss settings --worktree-dir ~/work/bossanova/worktrees"},
			{Command: "boss settings --skip-permissions"},
		},
	},
	"boss config init": {
		Examples: []Example{
			{Command: "boss config init"},
			{Command: "boss config init --plugin-dir ./plugins"},
		},
	},
	"boss login": {
		Examples: []Example{{Command: "boss login"}},
	},
	"boss logout": {
		Examples: []Example{{Command: "boss logout"}},
	},
	"boss auth-status": {
		Examples: []Example{{Command: "boss auth-status"}},
	},

	// --- Diagnostics ---
	"boss repair doctor": {
		Long: "Health-checks the auto-repair pipeline. Calls the daemon's " +
			"`RepairDoctor` RPC and renders a checklist (plugin loaded, `claude` " +
			"on PATH, recent log files, etc.) plus a recent-logs table — answers " +
			"\"is auto-repair healthy?\" without having to grep daemon stderr.",
		Examples: []Example{{Command: "boss repair doctor"}},
	},
	"boss session checks": {
		Long: "Shows bossd's persisted view of a session's CI check snapshots, " +
			"alongside the `DisplayStatus` the daemon computed for each one. " +
			"Useful when reconciling \"why did the TUI think this PR was passing " +
			"when GitHub says failing?\".",
		Examples: []Example{
			{Command: "boss session checks abc123"},
			{Command: "boss session checks abc123 --limit 10"},
		},
	},
	"boss session link-pr": {
		Long: "Attach an existing GitHub PR to a session. Use this to repair " +
			"cron sessions where the agent already committed, pushed, and opened " +
			"a PR before bossd finalized the run.",
		Examples: []Example{
			{Command: "boss session link-pr abc123 477"},
			{Command: "boss session link-pr abc123 https://github.com/owner/repo/pull/477"},
		},
	},

	// --- Plugins ---
	"boss plugin list": {
		Examples: []Example{
			{Command: "boss plugin list"},
			{Command: "boss plugin ls"},
		},
	},

	// --- Other ---
	"boss version": {
		Examples: []Example{{Command: "boss version"}},
	},
	"boss upgrade": {
		Examples: []Example{
			{Command: "boss upgrade --check"},
			{Command: "boss upgrade --yes"},
			{Command: "boss upgrade --version v1.2.4 --yes"},
			{Command: "boss upgrade --yes --no-restart"},
		},
	},
}
