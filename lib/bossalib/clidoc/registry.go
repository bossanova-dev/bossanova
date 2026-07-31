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
var Registry = newRegistry()

// newRegistry constructs the command prose registry. Keeping the literal in a
// builder lets tests exercise and verify the documentation data after package
// initialization, when Go's coverage instrumentation is active.
func newRegistry() map[string]Prose {
	return map[string]Prose{
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
				"scripting. Use --agent to override the default agent plugin.\n\n" +
				"Launching a session to run work unattended: supplying a prompt " +
				"launches the agent (headless, via the implicit --detach) so the work " +
				"actually runs — the CLI and the MCP `create_session` tool now share " +
				"this default. `create_session` applies the same rule: a " +
				"prompt-carrying call defaults to headless and reports " +
				"agent_launched=true, while attended:true creates the session idle " +
				"awaiting a human `boss attach` (agent_launched=false). Prefer the " +
				"default for programmatic/unattended launches.",
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

		// --- GitHub Callbacks ---
		"boss callback": {
			Long: "A GitHub callback is a durable, one-shot notification: it fires a " +
				"prompt into a chat once a pull request reaches a chosen state, then " +
				"retires. Use it to answer natural-language asks like \"tell me when PR " +
				"#123 is merged\", \"ping this chat when PR #123 goes green\", \"let me " +
				"know if PR #123's checks fail\", \"notify me when PR #123 is closed\", " +
				"\"tell me when PR #123 comes out of draft\", or \"ping me when PR #123 " +
				"is green and ready to merge\". Triggers map to those phrasings: " +
				"`merged`, `checks_passed` (green), `checks_failed` (red), `closed`, " +
				"`ready_for_review` (the draft→ready flip), and `checks_passed_ready` " +
				"(green and not a draft — the merge-eligibility moment). Triggers are " +
				"evaluated on PR state, not on transitions: a callback armed on a PR " +
				"that ALREADY satisfies its trigger fires on the next evaluation rather " +
				"than waiting for a fresh event. Delivery only " +
				"signals that the event fired — always verify the PR's actual state " +
				"before acting on it. Callbacks expire after 24h by default and may not " +
				"outlive 30 days.",
		},
		"boss callback add": {
			Long: "Register a one-shot callback. `<pr>` is a bare PR number (resolved " +
				"against the current repository) or a full " +
				"`https://github.com/owner/repo/pull/N` URL. `<trigger>` is one of " +
				"`merged`, `closed`, `checks_passed`, `checks_failed`, " +
				"`ready_for_review` (the draft→ready flip), or `checks_passed_ready` " +
				"(green and not a draft — merge-eligible). Triggers match on PR state, " +
				"not on transitions, so arming one against a PR that already satisfies " +
				"it fires on the next evaluation. The `--message` " +
				"prompt is delivered verbatim to the target chat when the callback fires " +
				"and is treated as a secret — it is never echoed back on any surface. " +
				"Expiry defaults to 24h and may not exceed 30 days.",
			Examples: []Example{
				{
					Command:     `boss callback add 123 merged --message "PR #123 merged — pull main and redeploy"`,
					Explanation: `"tell me when PR #123 is merged"`,
				},
				{
					Command:     `boss callback add 123 checks_passed --message "PR #123 is green — start the release"`,
					Explanation: `"ping this chat when PR #123 goes green"`,
				},
				{
					Command:     `boss callback add 123 checks_failed --message "PR #123 is red — investigate the failing checks"`,
					Explanation: `"let me know if PR #123's checks fail"`,
				},
				{
					Command: `boss callback add https://github.com/acme/widget/pull/123 closed ` +
						`--message "PR #123 was closed" --expires-in 7d`,
					Explanation: `"notify me when PR #123 is closed" (full URL, longer expiry)`,
				},
				{
					Command:     `boss callback add 123 ready_for_review --message "PR #123 left draft — review it"`,
					Explanation: `"tell me when PR #123 comes out of draft"`,
				},
				{
					Command:     `boss callback add 123 checks_passed_ready --message "PR #123 is green and ready to merge"`,
					Explanation: `"ping me when PR #123 is green and ready to merge"`,
				},
			},
		},
		"boss callback list": {
			Examples: []Example{
				{Command: "boss callback list"},
				{Command: "boss callback list --repo acme/widget --trigger merged"},
				{Command: "boss callback list --json"},
			},
		},
		"boss callback remove": {
			Examples: []Example{{Command: "boss callback remove cb_abc123"}},
		},

		// --- Notes ---
		"boss notes": {
			Long: "A note is durable free-text recorded against a REPOSITORY so a later " +
				"sweep can harvest what a run learned — a gotcha, a decision, a piece " +
				"of tech debt worth filing. Notes are repo-scoped and session and chat " +
				"are provenance ONLY: they record who wrote the note, and archiving or " +
				"removing that session never removes its notes. A note outlives the run " +
				"that wrote it. Inside a registered repo or a session worktree the repo " +
				"and session default from the working directory, so an agent can leave a " +
				"note with one command and no ids to look up. A body is REQUIRED (a blank " +
				"or whitespace-only one is rejected), may be up to 64 KiB, and is stored " +
				"verbatim. Tags are normalised — trimmed, lowercased and " +
				"de-duplicated — so `Tech-Debt` and `tech-debt` are one tag; a note may " +
				"carry up to 32 tags of 64 bytes each. Notes are listed OLDEST first. " +
				"`add`, `ls`, `show` and `edit` all take `--json` for machine parsing.",
		},
		"boss notes add": {
			Long: "Record a note. `--repo`, `--session` and `--chat` are resolved in this " +
				"order: the explicit flag, then the ambient `BOSS_REPO_ID` / " +
				"`BOSS_SESSION_ID` / `BOSS_AGENT_SESSION_ID`, then — for the repo and " +
				"session only — the daemon's resolution of the working directory. An " +
				"agent running inside its own session worktree therefore needs no ids at " +
				"all. There is no working-directory fallback for the chat: a session's " +
				"primary chat is not necessarily the one calling, so guessing would " +
				"attribute the note to the wrong agent — export " +
				"`BOSS_AGENT_SESSION_ID` or pass `--chat` if the chat matters. When the " +
				"repository cannot be resolved the command FAILS naming `--repo` rather " +
				"than writing the note against the wrong repo. `--tag` is repeatable " +
				"(`--tag a --tag b`), not comma-separated; tags are trimmed, lowercased " +
				"and de-duplicated before they are stored.",
			Examples: []Example{
				{
					Command:     `boss notes add "the flaky test is a socket-token race" --tag tech-debt`,
					Explanation: "From inside a session worktree: repo and session are inferred, no ids needed",
				},
				{
					Command:     `boss notes add "auth middleware assumes a trailing slash" --tag gotcha --tag auth`,
					Explanation: "Repeat --tag for several tags",
				},
				{
					Command:     `boss notes add "release checklist step 3 is stale" --repo my-repo --json`,
					Explanation: "Record against an explicit repo from anywhere, and parse the result",
				},
			},
		},
		"boss notes ls": {
			Long: "List notes in the order they were recorded, OLDEST first, so " +
				"`--limit N` returns the N oldest. `--repo` resolves like `add`'s: the " +
				"explicit flag, then the ambient `BOSS_REPO_ID`, then the working " +
				"directory's repo — so inside a repo the listing is scoped to it. To " +
				"list across EVERY repo pass `--repo \"\"` explicitly; simply leaving " +
				"the repo directory is NOT enough, because a boss-managed agent pane " +
				"always exports `BOSS_REPO_ID`. `--tag` " +
				"matches ANY of the tags given (a note carrying just one of them " +
				"matches), not all of them; unlike on `add`/`edit`, `--tag \"\"` here is " +
				"not a wildcard — the daemon fails closed on a tag that normalises away, " +
				"so it matches nothing. `--search` matches a substring of the body, " +
				"case-insensitively for ASCII only (the daemon folds case with SQLite's " +
				"`LOWER()`, which does not fold non-ASCII); SQL wildcards are matched " +
				"literally. `--session` filters by the session that recorded " +
				"the note and does NOT default from the working directory — a " +
				"session-scoped default would silently hide the repo's other notes. " +
				"Bodies are flattened to one line and truncated in the table: use " +
				"`boss notes show` for the full text.",
			Examples: []Example{
				{Command: "boss notes ls"},
				{
					Command:     "boss notes ls --tag tech-debt --tag gotcha",
					Explanation: "Notes carrying EITHER tag (any-of, not all-of)",
				},
				{
					Command:     `boss notes ls --search "socket token" --limit 5`,
					Explanation: "The 5 oldest notes whose body contains the term",
				},
				{Command: "boss notes ls --repo my-repo --json"},
				{
					Command:     `boss notes ls --repo ""`,
					Explanation: "Every repo, even from inside a session pane that exports BOSS_REPO_ID",
				},
			},
		},
		"boss notes show": {
			Long: "Print one note in full: its ids, provenance, tags, timestamps, and " +
				"then the body verbatim and untruncated (`boss notes ls` only shows a " +
				"one-line preview). `--repo` is a routing hint for a remote daemon and " +
				"is ignored locally — the note is resolved by id.",
			Examples: []Example{
				{Command: "boss notes show abc123"},
				{Command: "boss notes show abc123 --json"},
			},
		},
		"boss notes edit": {
			Long: "Change a note's body and/or tags; pass at least one of `--body` and " +
				"`--tag` or the command fails with nothing to do. An omitted `--body` " +
				"leaves the body alone and an omitted `--tag` leaves the tags alone. " +
				"Passing `--tag` REPLACES the whole tag set with exactly what you pass — " +
				"it does not merge, so re-list every tag the note should keep. " +
				"`--tag \"\"` therefore clears every tag.",
			Examples: []Example{
				{
					Command:     `boss notes edit abc123 --body "the flaky test is a socket-token race; fixed in #1712"`,
					Explanation: "Rewrite the body, leaving the tags untouched",
				},
				{
					Command:     "boss notes edit abc123 --tag tech-debt --tag resolved",
					Explanation: "REPLACES the tag set with exactly these two tags",
				},
				{
					Command:     `boss notes edit abc123 --tag ""`,
					Explanation: "Clear every tag",
				},
			},
		},
		"boss notes rm": {
			Long: "Remove a note by id. Removal is idempotent: removing a note that is " +
				"already gone succeeds rather than erroring, so a cleanup script can be " +
				"re-run safely. Removing a note is permanent — there is no trash for " +
				"notes.",
			Examples: []Example{{Command: "boss notes rm abc123"}},
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
				"if the daemon is already stopped or not installed. Normal stops " +
				"leave plugin processes alone — bossd reaps its own children. Use " +
				"`--all-standalone` only for explicit cleanup of every user-owned " +
				"standalone bossd process and its `bossd-plugin-*` children across " +
				"profiles.",
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
			Long:     "Reports the MCP service state (installed/running) plus the instances: inventory of every boss-mcp process owned by the current user (service, stray HTTP, session-owned, and orphaned counts).",
			Examples: []Example{{Command: "boss mcp status"}},
		},
		"boss mcp start": {
			Examples: []Example{{Command: "boss mcp start"}},
		},
		"boss mcp stop": {
			Long:     "Stops the managed MCP service (leaving its service file in place) and also terminates stray `--http` daemons and orphaned session MCP servers owned by the current user, while leaving live session-owned servers running since each exits with its own chat. Idempotent.",
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
		"boss repair start": {
			Long: "(Re-)arms the auto-repair workflow. Calls the daemon's " +
				"`StartRepairWorkflow` RPC, which declares the repair plugin's " +
				"desired-started state from current settings and ensures the " +
				"workflow is running. A RUNNING workflow is left untouched (never " +
				"restarted); a PAUSED one is left for the operator to resume. Use " +
				"after the repair plugin was stopped or restarted and auto-repair " +
				"is sitting disarmed — no bossd restart needed.",
			Examples: []Example{{Command: "boss repair start"}},
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
}
