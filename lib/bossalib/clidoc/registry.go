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
				"is hidden so the table stays compact.\n\n" +
				"`--json` emits `{\"sessions\": [...]}` — never `null`, and never the " +
				"human \"No sessions found.\" line, so a driver decodes one shape " +
				"whether or not anything matched. Each row carries `id`, `title`, " +
				"`state`, `repo_id`, `agent`, `pr_number`, `pr_url`, `branch`, " +
				"`created_at` and `updated_at`. `state` is the enum NAME with its " +
				"`SESSION_STATE_` prefix trimmed (`RUNNING`, `READY_FOR_REVIEW`, …), " +
				"never the numeric value, so a caller is not coupled to proto field " +
				"ordering; it is the same vocabulary `--state` accepts. `pr_number` " +
				"is `null` rather than 0 for a session with no PR. Timestamps are " +
				"RFC3339 in UTC. `--repo`, `--state` and `--archived` filter " +
				"identically with and without `--json` — the flag changes rendering " +
				"only, never the query.",
			Examples: []Example{
				{Command: "boss ls"},
				{Command: "boss ls --repo my-repo --state running,paused"},
				{Command: "boss ls --archived"},
				// Deliberately no Explanation: the renderer emits one as a
				// `# ...` line inside the bash fence, and skillgen's
				// reference preamble guard rejects any `\n# ` as an H1. The
				// point it would have made (state is the enum name, not a
				// number) is already spelled out in the prose above.
				{Command: "boss ls --json | jq -r '.sessions[] | select(.state==\"READY_FOR_REVIEW\") | .pr_url'"},
			},
		},
		"boss show": {
			Long: "Shows one session's details, then its chats in the same table `boss chats` " +
				"prints -- ID, TITLE, CREATED, STATUS and LAST OUTPUT, with the same reading of " +
				"UNSPECIFIED and of last_output_at. If the status read fails the rows still " +
				"print, STATUS and LAST OUTPUT read `?`, one stderr line explains why, and the " +
				"command exits 0.\n\n" +
				"`--json` emits `{\"session\": {...}}`: every field of a `boss ls " +
				"--json` row, plus the detail the text rendering prints — " +
				"`repo_display_name`, `base_branch`, `worktree_path`, `account_id`, " +
				"`account_label`, `display_status`, `last_check_state`, " +
				"`archived_at`, `setup_error` and `last_repair` (an object, or " +
				"`null` when the repair plugin has never run for this session). " +
				"`display_status` uses the same vocabulary as the TUI and `boss " +
				"session checks` (`idle`, `checking`, `passing`, `failing`, …). The " +
				"chats table the text rendering prints below the header is " +
				"deliberately NOT in this envelope — `boss chats` owns that shape, " +
				"because it has to join in per-chat status, so drive a machine gate " +
				"off `boss chats --json`. An unknown session id " +
				"exits 1 with the shared JSON error envelope on stdout.",
			Examples: []Example{
				{Command: "boss show abc123"},
				{Command: "boss show abc123 --json"},
			},
		},
		"boss new": {
			Long: "Launches the interactive session creation flow. " +
				"When both --repo and --prompt are provided the command runs " +
				"non-interactively: it creates the session, streams any setup output " +
				"to stderr, and prints the session-id and chat-id to stdout, then " +
				"exits. Combine with --detach (implicit when both flags are set) for " +
				"scripting. Use --agent to override the default agent plugin.\n\n" +
				"Launching a session to run work unattended: supplying a prompt " +
				"launches the agent headlessly so the work actually runs — that is " +
				"the path's own behaviour, not something --detach causes. " +
				"The CLI and the MCP `create_session` tool now share " +
				"this default. `create_session` applies the same rule: a " +
				"prompt-carrying call defaults to headless and reports " +
				"agent_launched=true, while attended:true creates the session idle " +
				"awaiting a human `boss attach` (agent_launched=false). Prefer the " +
				"default for programmatic/unattended launches.\n\n" +
				"--detach vs --tmux-unattended: they are NOT alternatives. The " +
				"non-interactive --repo + --prompt path always detaches, so --detach " +
				"is a no-op there and only affects flag parsing; it governs whether " +
				"this command attaches a chat pane before it exits, never how or " +
				"where the session is hosted. --tmux-unattended is the distinct " +
				"durable-pane option: it hosts the session in a tmux pane that " +
				"survives a daemon restart and is attach-safe, which is what a child " +
				"session that must outlive the daemon needs. Both can be set at once, " +
				"and the daemon carries them as independent fields.\n\n" +
				"Tracker binding: --tracker-id, --tracker-source (linear or sentry) " +
				"and --tracker-url bind the session to an external issue, which is " +
				"what the daemon's tracker-id dedup keys on — more robust than the " +
				"`[<TICKET>] <title>` title convention, which silently duplicates a " +
				"session when the title drifts. Each flag is independent and an " +
				"omitted one leaves its field unset; an unrecognised --tracker-source " +
				"is rejected before any session is created.\n\n" +
				"Use --json on the non-interactive path for a machine-readable " +
				"envelope instead of the two-line output: success prints " +
				"{session:{id, title, chat_id}} on stdout, failure prints " +
				"{error:{code, connect_code, message}} with a stable `code` such as " +
				"INVALID_ARGUMENT or NOT_FOUND, and every failure still exits 1. " +
				"Setup output goes to stderr either way, so stdout carries exactly " +
				"one JSON object. Without --json the two-line `session-id:` / " +
				"`chat-id:` output is unchanged.",
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
				{
					Command: "boss new --repo my-repo --prompt \"/boss-build PROJ-42\" " +
						"--tmux-unattended --tracker-id PROJ-42 --tracker-source linear --json",
					Explanation: "Launch a durable tracker-bound child and parse {session:{id, chat_id}}",
				},
				{
					Command: "CHAT=$(boss new --repo my-repo --prompt \"/boss-build PROJ-42\" " +
						"--json | jq -r .session.chat_id)",
					Explanation: "Capture the chat id without parsing the human two-line output",
				},
			},
		},
		"boss attach": {
			Long:     "Attaches to a running session's terminal.",
			Examples: []Example{{Command: "boss attach abc123"}},
		},
		"boss chats": {
			Long: "Lists a session's chats with ID, TITLE, CREATED, STATUS and LAST OUTPUT " +
				"(`boss show` prints the same table). " +
				"STATUS is the bare chat-status name (IDLE, WORKING, QUESTION, STOPPED, " +
				"LIMITED, WAITING, UNSPECIFIED); a WAITING chat also shows its reason. A chat " +
				"with no cached status reads UNSPECIFIED, which means unknown, not settled; so " +
				"does a status a newer daemon reports that this build has no name for, so the " +
				"value is never a bare number. " +
				"Use --json for one object per chat carrying agent_session_id, title, " +
				"created_at, status, last_output_at and waiting_reason, with timestamps in " +
				"RFC3339. No settled/not-settled boolean is emitted -- the threshold belongs " +
				"to the caller. Mind what last_output_at means: while a chat is WORKING it is " +
				"the fetch time, so every working chat in one read shares it, and it only " +
				"freezes at the true last output once the chat is IDLE. Gate on IDLE together " +
				"with a stale last_output_at; staleness alone proves nothing. If the status read " +
				"fails -- a daemon too old to serve the call -- table mode prints rows with " +
				"`?` plus one stderr line and exits 0, while " +
				"--json exits 1 with {error:{code: CHAT_STATUS_UNAVAILABLE, ...}} and no chats " +
				"array, so an unavailable status can never be misread as a settled one. " +
				"(Both --remote and --host return real statuses: --remote proxies the read to " +
				"the session's owning daemon, --host tunnels to a local one.)",
			Examples: []Example{
				{Command: "boss chats abc123"},
				{
					Command:     "boss chats abc123 --json",
					Explanation: "Machine-readable rows for a settled-green merge gate",
				},
			},
		},
		"boss merge": {
			Long: "Merges the session's pull request through the daemon, which owns the " +
				"merge gate, the per-repo merge serialization, and the merge-strategy " +
				"resolution. A session with no PR takes the local-only-branch merge path. " +
				"Prompts for confirmation unless -y/--yes is given; when the gate refuses, " +
				"the command exits non-zero with the daemon's `merge blocked: gate=<slug>` " +
				"message naming the gate that stopped it. Use --json for a machine-readable " +
				"envelope: success prints {session, pr, detail}, failure prints " +
				"{error:{code, connect_code, message}} on stdout with a stable `code` such as " +
				"MERGE_STRATEGY_INCOMPATIBLE, FAILED_PRECONDITION or NOT_FOUND, so a driver " +
				"can branch on the outcome without matching message text. Every failure still " +
				"exits 1; the code, not the exit status, is the discriminator.",
			Examples: []Example{
				{Command: "boss merge abc123"},
				{
					Command:     "boss merge abc123 --yes",
					Explanation: "Skip the confirmation prompt (unattended callers)",
				},
				{
					Command:     "boss merge abc123 --yes --json",
					Explanation: "Machine-readable envelope; --json requires --yes",
				},
			},
		},
		"boss archive": {
			Examples: []Example{{Command: "boss archive abc123"}},
		},

		// --- Chat Control ---
		"boss chat new": {
			Long: "Starts a brand-new live chat inside an existing session, reusing that " +
				"session's worktree, branch and PR with a clean context. This is the CLI " +
				"counterpart of the MCP start_chat tool. `boss new` is not a substitute: on " +
				"a session that is already live the daemon attaches to it instead, and the " +
				"supplied prompt is never run. The agent_session_id is minted for you; the " +
				"command fails rather than reporting success if the daemon could not spawn " +
				"a live agent behind the chat. An omitted --agent inherits the session's " +
				"own agent; --title names the chat. Print the id with --json and feed it " +
				"straight to `boss chat send`.",
			Examples: []Example{
				{Command: "boss chat new <session-id>"},
				{
					Command:     "boss chat new <session-id> --title \"repair round\" --json",
					Explanation: "Capture chat.agent_session_id, then `boss chat send <chat-id> ... --submit`",
				},
			},
		},
		"boss chat send": {
			Long: "Delivers a follow-up message to a running chat identified by a " +
				"session id or agent_session_id (the chat-id printed by `boss new --detach` " +
				"or `boss chat new --json`). " +
				"When given a session id, boss targets that session's primary chat. " +
				"The daemon wakes a sleeping chat before pasting the message; " +
				"--wake-if-asleep defaults to true and exists so a caller can pass " +
				"--wake-if-asleep=false to leave a deliberately stopped chat stopped.",
			Examples: []Example{
				{Command: "boss chat send <session-id|chat-id> \"please also add tests\""},
				{
					Command:     "boss chat send <chat-id> \"status?\" --wake-if-asleep=false",
					Explanation: "Do not wake a stopped chat just to deliver this message",
				},
			},
		},
		"boss chat rename": {
			Long: "Renames a chat identified by a session id or agent_session_id (the " +
				"chat-id printed by `boss new --detach` or `boss chat new --json`). " +
				"When given a session id, boss renames that session's primary chat — " +
				"use `boss rename` to retitle the session itself. " +
				"The new title is the trailing arguments joined with single spaces, so a " +
				"multi-word title needs no quoting; a title that is empty or only " +
				"whitespace is rejected before anything is sent. " +
				"Note that renaming a chat id that does not exist reports success: the " +
				"daemon's update matches no rows and does not fail.",
			Examples: []Example{
				{Command: "boss chat rename <session-id|chat-id> \"repair round 2\""},
				{
					Command:     "boss chat rename <chat-id> second opinion on PR 42",
					Explanation: "Trailing words are joined, so the quotes are optional",
				},
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
		"boss tail": {
			Long: "Prints recent rotated service logs without needing to locate them on disk. " +
				"It defaults to bossd; pass boss or bosso to select one source, or use " +
				"--all to merge all three by timestamp. Use -f to follow new output. " +
				"Raw non-JSON diagnostics always remain visible, including when filtering.",
			Examples: []Example{
				{Command: "boss tail"},
				{Command: "boss tail -f"},
				{Command: "boss tail --all -n 50"},
				{Command: "boss tail --plugin dependabot"},
				{Command: "boss tail --json | jq 'select(.level==\"error\")'"},
			},
		},
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
				"when GitHub says failing?\".\n\n" +
				"`--json` emits `{\"snapshots\": [...]}` (`[]`, never `null`, when " +
				"none are recorded yet), newest first and truncated by `--limit` " +
				"exactly as the text rendering is. Each entry carries `polled_at` " +
				"(RFC3339 UTC), `head_sha` (the FULL sha — the text rendering " +
				"abbreviates for width), `computed_status` (the same `DisplayStatus` " +
				"vocabulary the text rendering prints) and `raw`. `raw` is the " +
				"daemon's stored payload spliced in as a NESTED JSON value, not an " +
				"escaped string, so `.snapshots[0].raw.state` reads directly without " +
				"decoding twice. A payload that does not parse is preserved verbatim " +
				"under `raw_invalid` (with `raw` null) rather than being dropped or " +
				"corrupting the envelope.",
			Examples: []Example{
				{Command: "boss session checks abc123"},
				{Command: "boss session checks abc123 --limit 10"},
				{
					Command:     "boss session checks abc123 --json | jq '.snapshots[0].raw.check_runs'",
					Explanation: "raw is nested JSON, so it is queryable without a second decode",
				},
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
		"boss agents": {
			Long: "Lists the agent runners the daemon loaded — the plugins that " +
				"satisfy AgentRunnerService and can therefore back a session. This is " +
				"narrower than `boss plugin list`, which reports every loaded plugin " +
				"including task sources (linear, sentry) and automation reactors " +
				"(dependabot, repair). Use this to check that the agent you are about " +
				"to pass to `boss new --agent` is actually available; without a loaded " +
				"agent runner the daemon stays healthy but session creation fails.\n\n" +
				"The default table shows NAME, VERSION and a SETTINGS count. Use " +
				"--json for the full shape: each agent carries name, version and " +
				"user_settings, and each setting carries key, label, description, " +
				"default_value, type and allowed_values. `type` is the enum name " +
				"(BOOL, STRING, ENUM, UNSPECIFIED). user_settings, allowed_values and " +
				"the top-level agents list are always arrays, empty rather than null, " +
				"so a driver can iterate them without a null check. Zero loaded agents " +
				"is a valid answer: {\"agents\": []} with exit 0. A failure to reach " +
				"the daemon exits 1 with the shared {error:{code, connect_code, " +
				"message}} envelope.\n\n" +
				"Under --remote the answer is aggregated across every Ready daemon the " +
				"orchestrator knows about, and the aggregate carries no per-daemon " +
				"provenance — an agent in the list is loaded by at least one daemon, " +
				"not necessarily by the one that will run your session. The aggregate " +
				"is a plain concatenation in daemon order, so name is NOT unique in " +
				"it: two Ready daemons that both load claude yield two claude rows and " +
				"two JSON objects sharing that name. A driver keying by name silently " +
				"collapses them, and one that counts gets the daemon count rather than " +
				"the agent count — deduplicate client-side if you need a set. The CLI " +
				"deliberately does not, because that would hide fleet composition and " +
				"diverge from `boss plugin list` over the same aggregation. --host is " +
				"not affected: it tunnels to a single daemon and reports only that " +
				"daemon's runners.",
			Examples: []Example{
				{Command: "boss agents"},
				{
					Command:     "boss agents --json",
					Explanation: "Machine-readable, including each agent's user settings",
				},
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
