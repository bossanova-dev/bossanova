---
sidebar_position: 2
title: CLI Reference
description: Pointers to the authoritative help text for boss and bossd.
---

import CommandTabs from '@site/src/components/CommandTabs';

# CLI Reference

The authoritative reference for every command, subcommand, and flag is
the help text built into the binary:

```bash
boss --help
boss <subcommand> --help
```

Read the help text first. The two invocations above are that reference itself
rather than an example of an operation, so they stay a plain block; every `boss`
example below carries its interface tabs. The subcommand and flag tables are
reference material rather than examples, so they stay plain too. The examples
show the same operations through the other two interfaces as well, but they are
illustrative — the binary is what is authoritative.

## Top-level commands

### `boss`

The interactive terminal UI and the CLI for non-interactive operations.

Launch the Terminal UI (TUI) on the Home screen:

<CommandTabs
cli="boss"
/>

The TUI is a terminal program on this machine, so there is no chat prompt and no
MCP tool for launching it. An agent reaches the same data through read tools
instead — `list_sessions` and `get_session_statuses` for the session state the
home screen shows, plus the tools named below. See the
[MCP guide](/guides/mcp) for the full catalog.

View global settings:

<CommandTabs
chat='"show me my boss settings"'
cli="boss settings"
mcp="get_settings"
/>

The same command _writes_ settings when you give it a flag — `boss settings --help`
lists them — and the MCP counterpart for that path is `update_settings`.

List configured repos:

<CommandTabs
chat='"list my repos"'
cli="boss repo ls"
mcp="list_repos"
/>

Change repo fields from the shell:

<CommandTabs
chat='"turn on auto-merge for the bossanova repo"'
cli="boss repo update <repo-id> ..."
mcp="update_repo"
/>

Merge a session's pull request:

<CommandTabs
chat='"merge the dark mode session"'
cli="boss merge <session-id>"
mcp="merge_session"
/>

The daemon owns the merge — the gate that refuses an unready PR, the per-repo
serialization, and the merge-strategy resolution — so all three interfaces get
the same answer. When the gate refuses, the CLI exits non-zero with the daemon's
`merge blocked: gate=<slug>` message naming the gate that stopped it. The CLI
prompts for confirmation unless you pass `-y`/`--yes`; the MCP tool requires
`confirm:true` for the same reason.

#### `boss merge --json`

`--json` makes `boss merge` machine-readable. Scripts that previously had to
match message text to tell one failure from another can branch on a stable
`code` instead. `--json` requires `--yes`: a confirmation prompt is not part of
a machine contract, and prompting would either block on a stdin nobody attached
or read EOF and report a merge as declined that was never offered. Used without
`--yes` the command fails with `CONFIRMATION_REQUIRED`.

The JSON envelope goes to **stdout**; the human `boss: ...` line still goes to
**stderr**, so the two channels never interleave. Every failure exits `1` — the
`code`, not the exit status, is the discriminator.

Success:

```json
{
  "session": { "id": "abc123", "title": "add dark mode", "state": "SESSION_STATE_MERGED" },
  "pr": { "number": 42, "url": "https://github.com/acme/app/pull/42" },
  "detail": "merge strategy squash substituted for rebase"
}
```

`pr` is omitted entirely for a session with no pull request (the local-only
branch path) rather than emitted with zero values. `detail` is the daemon's note
about how the merge was performed and is always present, empty when the merge
ran exactly as configured. `session.state` is the protobuf enum name, the same
vocabulary the wire uses.

The full enum name is deliberate, and it does **not** match the short form
`boss ls --state` accepts (`boss ls --state merged`, not
`--state SESSION_STATE_MERGED`). The flag is a human affordance and normalises
its input; the envelope is a machine contract, so it emits the wire value
unchanged rather than a second spelling that would have to be kept in sync. A
script that feeds one into the other must strip the `SESSION_STATE_` prefix.

Failure:

```json
{
  "error": {
    "code": "MERGE_STRATEGY_INCOMPATIBLE",
    "connect_code": "failed_precondition",
    "message": "MERGE_STRATEGY_INCOMPATIBLE: branch has 1 merge commit(s), repo strategy is rebase"
  }
}
```

`message` carries the daemon's sentinel token as its prefix — the token is
matched in the message, not stripped from it, so `code` and `message` agree
rather than one being derived by rewriting the other.

`code` is the stable value to branch on. It resolves in this order: a failure
the CLI classified itself (`CONFIRMATION_REQUIRED`, `NOT_FOUND`,
`AMBIGUOUS_PREFIX`), then a sentinel token the daemon embedded
(`MERGE_STRATEGY_INCOMPATIBLE`), then the connect code upper-cased
(`FAILED_PRECONDITION`, `NOT_FOUND`, `UNAVAILABLE`, ...). The token is checked
before the connect code because a strategy incompatibility and an ordinary gate
refusal both travel as `failed_precondition` — the connect code alone cannot
tell them apart. `connect_code` is always emitted alongside so a driver meeting
an unfamiliar `code` still has the transport-level classification to fall back
on. `message` is the daemon's own message, without the connect-code prefix or
the CLI's wrapping.

:::note
Over `--remote`, `detail` is always `""` — the orchestrator's
`ProxyMergeSessionResponse` carries no detail field, so a merge-strategy
substitution made on the remote daemon is not reported to a remote caller.
`detail` being empty over `--remote` does not mean the merge ran as configured.
`--host` is not affected: it tunnels to a real local client and carries the
daemon's `detail` through unchanged.

`session.state` is not the fallback signal. It is the daemon's value verbatim,
and on a successful merge it can still read pre-merge: the daemon answers from a
session it read before its own deferred refresh applied the `Merged` transition.
The successful envelope itself — exit `0` with no `error` object — is the
outcome. A caller that needs the settled state must re-read the session
(`boss show <id>`).
:::

List the agent runners the daemon loaded:

<CommandTabs
chat='"which agent runners are loaded?"'
cli="boss agents"
mcp="list_agents"
/>

This is narrower than `boss plugin list`. That command reports every loaded
plugin, including task sources (`linear`, `sentry`) and automation reactors
(`dependabot`, `repair`); this one reports only the plugins that satisfy
`AgentRunnerService` and can therefore back a session. Check it before passing
an agent to `boss new --agent` — with no agent runner loaded the daemon stays
healthy but session creation fails.

The default table shows NAME, VERSION and a SETTINGS count. `--json` emits the
full shape — each agent's `name`, `version` and `user_settings`, and each
setting's `key`, `label`, `description`, `default_value`, `type` and
`allowed_values`:

```json
{
  "agents": [
    {
      "name": "claude",
      "version": "1.2.3",
      "user_settings": [
        {
          "key": "model",
          "label": "Model",
          "description": "Which model to run",
          "default_value": "sonnet",
          "type": "ENUM",
          "allowed_values": ["sonnet", "opus"]
        }
      ]
    }
  ]
}
```

`type` is the enum name (`BOOL`, `STRING`, `ENUM`, `UNSPECIFIED`), not the
daemon's numeric value. `agents`, `user_settings` and `allowed_values` are
always arrays and never `null`, so a driver can iterate them without a null
check. Zero loaded agents is a valid answer — `{"agents": []}` with exit `0`,
not an error. A failure to reach the daemon exits `1` with the same
`{"error": {...}}` envelope `boss merge --json` uses.

:::note
Over `--remote` the list is **aggregated across every Ready daemon** the
orchestrator knows about, and `ProxyListAgentsAggregatedResponse` carries no
per-daemon field. An agent in that list is loaded by at least one daemon, not
necessarily by the one that will run your session, and there is no way to tell
which from the response.

The aggregate is a plain concatenation in daemon order, so **`name` is not
unique in it**: two Ready daemons that both load `claude` produce two `claude`
rows and two JSON objects sharing that name. A driver keying by name silently
collapses them, and one that counts agents gets the daemon count instead —
deduplicate client-side if you need a set. `boss` deliberately does not, because
that would hide fleet composition and diverge from `boss plugin list`, which
aggregates the same way.

`--host` is not affected: it tunnels to a single daemon and reports only that
daemon's runners.
:::

#### `--json` on the session read surfaces

`boss ls`, `boss show`, `boss session checks` and `boss session mcp` are the reads
a driver polls, so each takes `--json` and emits a stable envelope on **stdout**. As with
`boss merge --json`, a failure puts the shared error envelope on stdout, leaves
the human `boss: ...` line on stderr, and exits `1`. Passing `--json` changes
rendering only — the filters, the `--limit`, and the daemon call underneath are
identical with and without it. Every timestamp is RFC3339 in UTC, and every
list-valued field is `[]` when empty, never `null`.

List sessions:

<CommandTabs
chat='"list my sessions"'
cli="boss ls --json"
mcp="list_sessions"
/>

```json
{
  "sessions": [
    {
      "id": "abc123",
      "title": "add dark mode",
      "state": "READY_FOR_REVIEW",
      "repo_id": "repo-1",
      "agent": "claude",
      "pr_number": 42,
      "pr_url": "https://github.com/acme/app/pull/42",
      "branch": "add-dark-mode",
      "created_at": "2026-01-02T03:04:05Z",
      "updated_at": "2026-01-02T04:05:06Z",
      "tracker_id": "BOS-123",
      "last_agent_activity_at": "2026-01-02T04:06:07Z"
    }
  ]
}
```

`sessions` is `[]` when nothing matches — the human `No sessions found.` line is
never emitted under `--json`, so a driver decodes one shape either way.
`pr_number` is `null` rather than `0` for a session with no PR.
`tracker_id` is `null` when the session is not linked to a tracker issue, and
drivers can use `last_agent_activity_at` to tell whether a peer session is still
alive.

`state` here is the **short** enum name, with the `SESSION_STATE_` prefix
trimmed — the same vocabulary `boss ls --state` accepts, so a value read out of
one can be fed straight back into the other. This deliberately differs from
`boss merge --json`, whose `session.state` carries the full wire value; that
envelope reports a daemon response verbatim, while these reads own their
rendering. Neither emits the numeric value, which would couple callers to
protobuf field ordering.

Show one session:

<CommandTabs
chat='"show me the dark mode session"'
cli="boss show <session-id> --json"
mcp="get_session"
/>

`boss show --json` emits `{"session": {...}}`: every field of an `ls` row, plus
`repo_display_name`, `base_branch`, `worktree_path`, `account_id`,
`account_label`, `display_status`, `last_check_state`,
`last_check_state_observed`, `last_check_state_head_sha`,
`last_check_state_at`, `archived_at`, `setup_error`, and `last_repair` — an
object when the repair plugin has run for this session, `null` when it never
has. `last_check_state=UNSPECIFIED` means the daemon has no verdict
demonstrated at the current head; `last_check_state_observed` and its
observed-at fields show the raw cached latch.

The chats table that `boss show` prints below its header is **not** in the
envelope. `boss chats` owns that shape, because it has to join in per-chat live
status; emitting a half-populated version of it here would give a driver a
second, weaker spelling of the same data.

Read the persisted CI check snapshots:

<CommandTabs
chat='"what checks does boss have recorded for the dark mode session?"'
cli="boss session checks <session-id> --json"
mcp="list_check_snapshots"
/>

```json
{
  "snapshots": [
    {
      "polled_at": "2026-01-02T04:05:06Z",
      "head_sha": "9f2c1ab4d5e6f708192a3b4c5d6e7f8091a2b3c4",
      "computed_status": "passing",
      "raw": { "state": "success", "total_count": 3 }
    }
  ]
}
```

`raw` is the daemon's stored payload spliced in as a **nested JSON value**, not
an escaped string, so `.snapshots[0].raw.state` reads directly instead of
needing a second decode. A payload that does not parse is preserved verbatim
under `raw_invalid` with `raw` set to `null`, rather than being dropped or
corrupting the surrounding envelope.

`head_sha` is the full sha; the text rendering abbreviates it for width.
`computed_status` is the same `DisplayStatus` vocabulary the text rendering
prints. Snapshots are newest first and `--limit` (default 5) truncates from that
end, exactly as without `--json`; `snapshots` is `[]` when none are recorded yet.

Refresh one session's cached PR status immediately:

<CommandTabs
chat='"refresh the dark mode session PR status"'
cli="boss session refresh-pr <session-id>"
mcp="refresh_session_pr"
/>

Pass `--pr <number>` instead of a session id when you only have the pull request
number, or pass both to require that they match. The command fetches from the VCS
provider immediately; fetch failures return an error and leave the previous
cached snapshot intact.

Read which MCP servers a chat's agent actually resolved:

<CommandTabs
chat='"which MCP servers does this chat actually have?"'
cli="boss session mcp <chat-id> --json"
/>

```json
{
  "agent_name": "claude",
  "worktree_path": "/path/to/worktree",
  "is_supported": true,
  "unsupported_reason": "",
  "source": "claude -p --output-format stream-json (system/init event)",
  "probed_at": "2026-08-12T10:00:00Z",
  "probe_error": "",
  "servers": [
    {
      "name": "bossanova-linear",
      "is_declared": true,
      "tool_count": 34,
      "tool_name_prefix": "mcp__bossanova-linear__",
      "auth_status": "oauth",
      "source": "repo_file",
      "source_detail": ".mcp.json",
      "is_truncated": false
    }
  ]
}
```

The probe is live: it asks the chat's own agent, in the chat's own worktree,
under the chat's own resolved environment, and never runs a coding turn to
completion — it is killed the moment the startup event is read, though a probe
that never sees that event may briefly start one before its deadline. Asking the
model itself does not work — a session with dozens of MCP tools loaded has been
observed answering "none" — and `<agent> mcp list` reports the environment _it_
inherits rather than the session's.

It is a **fresh invocation**, not an inspection of the running pane: the answer
is what a new agent process resolves in that worktree under that re-derived
environment, which is what a restart of the chat would get.

Three outcomes are deliberately distinguishable, because conflating them is what
turns a five-second question into a multi-hour investigation:

- **`servers: []` with an empty `probe_error`** — the agent really resolved no
  MCP servers.
- **`probe_error` set** — the probe ran and failed, so the surface is _unknown_.
  `servers` is empty, and a caller must not read that as "none configured".
- **`is_supported: false`** — the agent has no way to report its MCP surface.
  The command still exits `0` and `unsupported_reason` says why.

Within `servers`, a server that is **declared but exposed zero tools** appears
with `is_declared: true` and `tool_count: 0` — the state a failed connection or
a missing credential produces. Read `auth_status` before concluding anything
from a zero count: it is the harness's own verbatim word, and a Claude session
can emit its startup event before every server has finished connecting, so a
server that will connect can appear as `pending` with zero tools. `pending, 0`
is a race; `failed, 0` is a verdict. A server that was never declared is **absent**
from the array entirely. `tool_name_prefix` is the literal prefix a skill must
call, so a config key that does not match the name a skill expects is visible
without invoking a single tool. `source` is one of `repo_file`, `user_config`,
`injected` or `unknown`, with `source_detail` naming the scanned file whenever
it is not `unknown`. `injected` is reserved for a launcher that supplies MCP
config on argv; boss does not, so it is never emitted today — a server that
matches no scanned config reports `unknown`. `tool_names` is omitted unless
`--tools` is passed; with `--tools` every server carries the key, and a
declared-but-empty server carries `[]` rather than dropping it.

Health-check the auto-repair pipeline:

<CommandTabs
chat='"run the repair doctor"'
cli="boss repair doctor"
mcp="repair_doctor"
/>

### `boss --host` (drive a daemon on another machine)

`--host` points `boss` at a `bossd` running on another machine, over an
SSH-forwarded unix socket. Every command works the same way it does locally,
apart from a handful that act on the machine `boss` runs on.

| Flag                   | Description                                                                              |
| ---------------------- | ---------------------------------------------------------------------------------------- |
| `--host <dest>`        | SSH destination of the machine whose `bossd` to drive                                    |
| `--host-socket <path>` | Remote `bossd` socket path, skipping discovery when remote `boss` is not on the SSH PATH |

See the [Remote Daemons guide](/guides/remote-daemons) for prerequisites, a
first-connection walkthrough, how `--host` differs from `--remote`, chat-pane
attach over SSH, and the commands that stay local.

### `boss mcp`

Manage the local MCP server that exposes Bossanova to AI agents.

| Subcommand           | Description                                                                                     |
| -------------------- | ----------------------------------------------------------------------------------------------- |
| `boss mcp install`   | Install and start the local MCP service (`--port`, `--force`)                                   |
| `boss mcp status`    | Show whether the service is installed / running, plus the instance inventory                    |
| `boss mcp start`     | Start or restart the installed service                                                          |
| `boss mcp stop`      | Stop the managed service, sweep stray/orphaned instances, leave live session-owned ones running |
| `boss mcp uninstall` | Stop and remove the service file                                                                |

See the [MCP guide](/guides/mcp) for agent wiring and the tool catalog.

### `boss upgrade`

Check for and install Bossanova upgrades.

| Flag              | Description                                                           |
| ----------------- | --------------------------------------------------------------------- |
| `--check`         | check for an upgrade without installing                               |
| `--yes`           | required to install; without it the command refuses                   |
| `--version <tag>` | install a specific stable release tag (prereleases are not supported) |
| `--no-restart`    | do not restart the daemon after upgrade                               |

See [Upgrade](/upgrade) for the full upgrade guide.

### `boss new` and `boss chat` (scripted chat control)

Create a session non-interactively and drive its chat from the shell (or, with the
same reach, from an MCP agent):

<CommandTabs
chat='"start a session on the bossanova repo to fix the flaky login test"'
cli="boss new --repo <r> --prompt <p>"
mcp="create_session"
/>

The CLI form creates the session, prints its session-id as soon as the session
row exists, prints chat-id later if the daemon provides one, then keeps streaming
setup progress on stderr until setup settles. `--detach` is deliberately absent:
the `--repo` + `--prompt` path
always runs the initial agent pass headlessly, so passing it changes nothing. For
a run that is **not** expected to change the repository, reach for `--defer-pr`
instead — see [Runs that may change nothing](#runs-that-may-change-nothing)
below.

`boss new` runs non-interactively when both `--repo` and `--prompt` are given (`--detach`
is then implicit; `--no-attach` is an alias). `--agent <a>` overrides the default agent
plugin; `--title <t>` is optional (auto-derived from the prompt when absent).

| Flag                      | Description                                                                                                                                                                                                                        |
| ------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--detach`, `--no-attach` | No-op on the `--repo` + `--prompt` path, which always runs the initial agent pass headlessly, prints session-id as soon as the session exists, prints chat-id later if available, and streams setup progress on stderr             |
| `--tmux-unattended`       | Host the session in a durable tmux pane that survives a daemon restart and is attach-safe                                                                                                                                          |
| `--defer-pr`              | Create the worktree-backed session but open **no** draft PR up front; a PR is opened at finalize only if the run produced commits. No PR exists until the run finalizes, so pair with `--tmux-unattended` for long unattended runs |
| `--quick-chat`            | Create a session with no worktree, branch, or PR, in the repository checkout. The agent starts when you attach. Mutually exclusive with `--defer-pr`                                                                               |
| `--tracker-id <id>`       | External issue id to bind the session to (e.g. `BOS-821`)                                                                                                                                                                          |
| `--tracker-source <src>`  | External issue tracker: `linear` or `sentry`. Any other value is rejected before any RPC is issued                                                                                                                                 |
| `--tracker-url <url>`     | URL of the external issue the session is bound to                                                                                                                                                                                  |
| `--json`                  | Emit the created session as a stable JSON object instead of the two-line output                                                                                                                                                    |

`--detach` and `--tmux-unattended` are independent, and the pairing is the part most
often got wrong. `--detach` governs only whether this command attaches a chat pane
before it exits; `--tmux-unattended` says where the session should live — a durable
tmux pane that outlives a daemon restart and can be attached to later. Neither one
decides whether the agent runs: supplying a prompt is what launches it headlessly. A
scripted launch that should stay reachable wants `--tmux-unattended`, not `--detach`,
which the non-interactive path already implies. Do not retry a launch just
because `boss ls` is temporarily empty while setup is still running; capture the
printed session id and verify that exact session with `boss show <session-id>`.

#### Runs that may change nothing

By default a scripted launch opens a draft PR up front. That is right for a run meant
to produce code, but wrong for planning, recon or review work: a run that legitimately
commits nothing leaves an empty PR behind and finalizes as a no-op that needs
attention. `--defer-pr` is the fix — the session still gets a worktree and branch, but
the PR is opened at finalize **only if** commits actually landed, so a no-op finishes
cleanly and nothing is left to clean up:

<CommandTabs
chat='"do a read-only review of the auth module on the bossanova repo"'
cli="boss new --repo <r> --prompt <p> --defer-pr --tmux-unattended"
mcp="create_session"
/>

The `--tmux-unattended` in that example is load-bearing, not decoration. `--defer-pr`
means no PR exists until the run finalizes, and the daemon opens it there — through the
finalize Stop hook on the durable tmux-hosted path, or through the run-completion poller
that drives finalize for a paneless headless run. What neither path can do is finalize a
run that was killed first: a bossd restart marks a paneless headless run _orphaned_
rather than finalizing it, so its commits stay on a branch with no PR at all — where a
run without `--defer-pr` would at least still have its up-front draft PR.
`--tmux-unattended` hosts the run in a pane that survives the restart, so pair the two
for anything long-running.

`--quick-chat` is the other shape, and the two are not interchangeable. It creates a
session with no worktree, branch or PR at all, directly in the repository checkout, and
starts no agent: the prompt is stored and runs when somebody attaches. Because such a
create launches nothing, the `chat-id:` line comes back empty, a notice is written to
stderr, and the `--json` envelope carries a `next_action` field saying how to start the
work. Use it for a chat you intend to attach to yourself.

For **unattended** runs — anything scripted, batched or scheduled — prefer `--defer-pr`.
Concurrent `--quick-chat` sessions share the one repository checkout, so they contend
over the working tree and git index with each other and with any uncommitted local work.
The two flags are mutually exclusive and passing both is rejected before any RPC is
issued: a quick chat has no worktree, branch or draft PR for `--defer-pr` to defer.

Bind a scripted launch to a tracker issue and read the ids back as JSON:

<CommandTabs
chat='"start a session on bossanova for BOS-821 and keep it in a durable pane"'
cli="boss new --repo <r> --prompt <p> --tmux-unattended --tracker-id BOS-821 --tracker-source linear --json"
mcp="create_session"
/>

Without `--json` the command prints the same two lines it always has —
`session-id:` then `chat-id:` — which existing scripts parse positionally. With
`--json` stdout carries exactly one object and the setup-script progress moves to
stderr, so a driver can read `.session.id` and `.session.chat_id` straight out of it:

```bash
CHAT=$(boss new --repo <r> --prompt <p> --json | jq -r .session.chat_id)
boss chat wait "$CHAT"
```

A failure under `--json` writes the shared error envelope — `.error.code`,
`.error.connect_code`, `.error.message` — to stdout and still exits 1. An unknown
`--tracker-source` is caught locally and reported as `INVALID_ARGUMENT` without a
session ever being created.

| Subcommand                                              | Description                                                                                   |
| ------------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| `boss chat new <session-id>`                            | Start a new live chat inside an existing session (clean context, same worktree and branch)    |
| `boss chat rename <session-id\|chat-id> <new-title...>` | Rename a chat; the trailing words are joined into the new title                               |
| `boss chat send <session-id\|chat-id> <msg>`            | Deliver a follow-up message to a running chat; wakes a sleeping chat by default               |
| `boss chat show <session-id\|chat-id>`                  | Print the transcript (`--result-only` for just the final result, `--limit N` to cap messages) |
| `boss chat wait <session-id\|chat-id>`                  | Block until the chat is idle / waiting, then print the final result (`--timeout 30m`)         |

A `<session-id>` targets that session's primary chat; a `<chat-id>` (the
`agent_session_id` printed by `boss new --detach` or `boss chat new --json`)
targets a specific chat.

`boss chat send` wakes a sleeping chat before delivering. `--wake-if-asleep`
defaults to `true`, so behaviour is unchanged unless you pass
`--wake-if-asleep=false` to leave a deliberately stopped chat stopped.

#### Starting a second chat in a live session

<CommandTabs
chat='"open a fresh chat in the flaky-login session to run the repair round"'
cli="boss chat new <session-id>"
mcp="start_chat"
/>

`boss new` is not a substitute here: on a session that is already live the daemon
attaches to it and the supplied prompt is never run. `boss chat new` mints the
`agent_session_id` for you and fails rather than reporting success when the daemon
could not spawn a live agent behind the chat. An omitted `--agent` inherits the
session's own agent; `--title <t>` names the chat.

With `--json` the new chat comes back as an envelope whose `chat.agent_session_id`
feeds straight into `boss chat send`:

```bash
chat_id=$(boss chat new <session-id> --title "repair round" --json | jq -r .chat.agent_session_id)
boss chat send "$chat_id" "fix the failing checks on this PR" --submit
```

#### `boss chats --json`

`boss chats <session-id>` lists a session's chats. The table carries `ID`,
`TITLE`, `CREATED`, `STATUS` and `LAST OUTPUT`; `--json` emits the same rows as
a machine contract, for a driver deciding whether a session has gone quiet
enough to merge.

`boss show <session-id>` prints the same chat table below the session details,
including the degraded `?` rendering and its stderr line. It has no `--json`:
drive a machine gate off `boss chats --json`, which fails loudly rather than
degrading.

<CommandTabs
chat='"what are the chats on the dark mode session doing?"'
cli="boss chats <session-id> --json"
mcp="list_chats"
/>

An MCP agent gets the same two halves from `list_chats` and `get_chat_statuses`;
the CLI joins them for you.

```json
{
  "chats": [
    {
      "agent_session_id": "0f3c...",
      "title": "add dark mode",
      "created_at": "2026-03-04T05:06:07Z",
      "status": "IDLE",
      "last_output_at": "2026-03-04T09:12:44Z",
      "waiting_reason": ""
    }
  ]
}
```

`status` is the protobuf enum name with its `CHAT_STATUS_` prefix stripped:
`IDLE`, `WORKING`, `QUESTION`, `STOPPED`, `LIMITED`, `WAITING`, or
`UNSPECIFIED`. Every field is always present — an absent field is exactly what a
caller should not have to guess about — so a chat the daemon holds no cached
status for reads `UNSPECIFIED` with an empty `last_output_at`, never a missing
key. A status a newer daemon reports that this build has no name for also reads
`UNSPECIFIED`, so `status` is never a bare number. `waiting_reason` is populated
only for a `WAITING` chat, and the table shows it beside the status.

No settled/not-settled boolean is emitted. The CLI reports state; the threshold
is the caller's, because how long a quiet chat must stay quiet depends on what
the caller is about to do with the answer.

:::warning
`last_output_at` is not a staleness clock while a chat is `WORKING`. For a
working chat it is the time of the read, so every working chat in one fetch
shares it to the nanosecond and it advances on every poll. It freezes at the
genuine last output only once the chat is `IDLE`. A settled-green gate must
therefore test `status == "IDLE"` **and** a `last_output_at` older than its
threshold. Staleness alone proves nothing, and a fresh `last_output_at` does not
mean the agent is doing anything.
:::

When the per-chat status read fails — a daemon too old to implement it — the two
output modes diverge deliberately:

| Mode     | Behaviour                                                                                         |
| -------- | ------------------------------------------------------------------------------------------------- |
| table    | Rows still print with `?` in `STATUS` and `LAST OUTPUT`, one explanatory line on stderr, exit `0` |
| `--json` | Exit `1`, `{"error":{"code":"CHAT_STATUS_UNAVAILABLE", ...}}`, and **no** `chats` array           |

The JSON path refuses to answer rather than degrade because degraded rows would
read `"status": "UNSPECIFIED"` — byte-identical to chats that genuinely have not
reported yet. A driver that missed one check would read that as "not working"
and merge. Exiting `1` turns a transport gap into a stall instead of a wrong
merge.

:::note
Neither transport is a limitation. `--remote` proxies the read through the
orchestrator's `ProxyGetChatStatuses`, which routes it by `session_id` to the
session's owning daemon; `--host` tunnels to a real local client over SSH. Both
return real statuses.

`get_session_statuses` is not a substitute. It reports session-level state, not
per-chat status, and a session can read quiet while a chat inside it is still
producing output.
:::

### `boss tail`

Read logs without needing to know where they live. It reads files on the local
machine, so `--remote` and `--host` do not redirect it.

Positional sources are `bossd` (the default), `boss`, `bosso` — or an
**agent-session id**, which reads that agent's own log from the `agent-logs`
directory beside your worktree base directory:

<CommandTabs
cli="boss tail 3f2a1b4c-5d6e-4f70-8a91-b2c3d4e5f607 -f"
/>

Both agent-log formats are read, detected per file: the raw `tmux pipe-pane`
capture an interactive chat writes, and the `{"ts","text"}` JSON lines a
headless run writes with `[runner]` diagnostics interleaved. Agent output has no
level, repo or plugin, so `--level`, `--repo` and `--plugin` never hide it.

Name several sources to interleave them by timestamp. An untimestamped raw line
inherits the previous timestamp from its own file, falling back to the file's
modification time, so a capture is never sorted to the Unix epoch:

<CommandTabs
cli="boss tail bossd 3f2a1b4c-5d6e-4f70-8a91-b2c3d4e5f607"
/>

| Flag             | Description                                                               |
| ---------------- | ------------------------------------------------------------------------- |
| `--all`          | merge every **service** log (mutually exclusive with a positional source) |
| `-n`, `--lines`  | physical lines to read per source before filtering (default 10)           |
| `-f`, `--follow` | keep reading as the log grows                                             |
| `--repo`         | only records for this repo                                                |
| `--plugin`       | only records from this plugin                                             |
| `--level`        | only records at this level                                                |
| `--json`         | emit one parseable JSON object per line                                   |

`--all` stays service-only — the `agent-logs` directory holds one file per agent
session ever run. An absent `agent-logs` directory prints a notice and exits 0.
See the [Logging guide](/guides/logging) for the full treatment.

### `bossd`

The background daemon. Normally started by Homebrew's launchd plist or
your equivalent service manager. You rarely run it by hand. It takes
configuration from environment variables and `settings.json`; there
are no subcommands. It is the daemon binary rather than the `boss` CLI, so
these invocations have no chat or MCP form and stay a plain block:

```bash
bossd               # run in foreground
bossd --version     # print version info
```

## Settings overrides

Most settings can be overridden by environment variable. Precedence
(highest wins): environment variable → `settings.json` → hardcoded
default. See
[Settings → Environment overrides](./settings.md#environment-overrides)
for the table.

A hand-curated reference page is on the roadmap. If you hit something
ambiguous in the help text, open an issue at
[bossanova-dev/bossanova/issues](https://github.com/bossanova-dev/bossanova/issues).
