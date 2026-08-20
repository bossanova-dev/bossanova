---
title: Settings File
description: 'JSON reference for the bossanova settings.json: every field, default, and precedence rule.'
---

# Settings File

Bossanova reads global settings from a JSON file on disk:

- **macOS:** `~/Library/Application Support/bossanova/settings.json`
- **Linux:** `$XDG_CONFIG_HOME/bossanova/settings.json` (defaults to `~/.config/bossanova/settings.json`)
- **Profile override:** set `BOSS_SETTINGS_PATH` to an absolute path to select a specific settings file.

The file is optional. When it's absent, defaults apply. Both `boss` and
`bossd` read the same file. Use `BOSS_SETTINGS_PATH` when you want a dev
build and an installed build to run in parallel without sharing local state.

Press `s` from the home screen to open the Settings view. It hosts the global
toggles (such as the _Skip permissions_ checkbox) and is also the gateway to the
Repos (`r`), Cron (`c`), and Trash (`t`) screens.

![Bossanova settings view](/img/screenshots/tui-settings.png)

## Example

```json
{
  "worktree_base_dir": "/Users/you/work/worktrees",
  "default_agent": "claude",
  "poll_interval_seconds": 120,
  "plugins": [
    {
      "name": "claude",
      "path": "/opt/homebrew/libexec/plugins/bossd-plugin-claude",
      "enabled": true,
      "config": {
        "dangerously_skip_permissions": "true"
      }
    },
    {
      "name": "codex",
      "path": "/opt/homebrew/libexec/plugins/bossd-plugin-codex",
      "enabled": true
    },
    {
      "name": "repair",
      "path": "/opt/homebrew/libexec/plugins/bossd-plugin-repair",
      "enabled": true
    }
  ],
  "repair": {
    "skills": { "repair": "boss-repair" },
    "cooldown_minutes": 1,
    "poll_interval_seconds": 5,
    "sweep_interval_minutes": 1
  }
}
```

Cloud-sync settings (orchestrator URL, WorkOS client ID, daemon ID)
are configured via environment variables. See
[Environment overrides](#environment-overrides) below.

## Top-level fields

| Field                     | Type   | Default                     | Description                                                                                                                                      |
| ------------------------- | ------ | --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `worktree_base_dir`       | string | `~/.bossanova/worktrees`    | Directory where per-session git worktrees are created. Auto-created on load.                                                                     |
| `app_data_dir`            | string | platform default            | Absolute directory for local daemon data: `bossd.db`, `bossd.lock`, profile plugin discovery, and default socket placement.                      |
| `socket_path`             | string | derived from data directory | Absolute path to the local `bossd` Unix-domain socket. If unset and `app_data_dir` is set, defaults to `app_data_dir/bossd.sock`.                |
| `default_agent`           | string | `claude`                    | Name of the default agent plugin used for new sessions.                                                                                          |
| `skills_declined`         | bool   | `false`                     | Set after the user declines the one-time skills install prompt so it's not shown again.                                                          |
| `poll_interval_seconds`   | int    | `120`                       | How often the Terminal UI (TUI) polls for PR display status, in seconds.                                                                         |
| `plugins`                 | array  | auto-discovered             | Plugin binaries to load (see below). If unset, `bossd` auto-discovers `bossd-plugin-*` binaries next to its own.                                 |
| `repair`                  | object | defaults below              | Repair plugin configuration.                                                                                                                     |
| `tmux_delivery`           | object | defaults below              | Composer-readiness deadlines for message delivery into an agent pane. See [`tmux_delivery` fields](#tmux_delivery-fields).                       |
| `daemon_path_extra`       | array  | `[]`                        | Directories **prepended** to the PATH written into the generated `bossd` service file. See [Daemon PATH](#daemon-path).                          |
| `subagent_dispatch_grant` | string | `always`                    | Which chats receive the bounded subagent-dispatch grant in their system prompt. See [`subagent_dispatch_grant`](#subagent_dispatch_grant) below. |

## Daemon PATH

A service manager never sources your interactive shell config, so the daemon
does not inherit the PATH your terminal has. A toolchain installed by **nodenv,
nvm, asdf or volta** — or a `claude` from the native installer in
`~/.local/bin` — is therefore invisible to `bossd` unless its directory is on
the service PATH.

The generated service file sets this PATH by default:

```
~/.nodenv/shims:~/.local/bin:/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin
```

on macOS (the LaunchAgent plist), and the same two shim directories prepended
to the installing shell's PATH on Linux (the systemd unit). The `bossd` service
and the MCP service render this from the same helper, so they can never
disagree about where `node` lives.

### `daemon_path_extra`

Set `daemon_path_extra` to put your own directories at the **front** of that
PATH:

```json
{
  "daemon_path_extra": ["~/.asdf/shims", "/opt/custom/bin"]
}
```

Entries must be **absolute** or `~/`-rooted. Anything else is dropped when the
service file is rendered: a relative path, an empty entry, an entry containing a
`:` (the PATH separator itself), a `\`, or one containing a newline or an
XML-special character (`<`, `>`, `&`, `"`), which would otherwise corrupt the
generated plist or inject a directive into the systemd unit. Duplicates are
removed, keeping the first occurrence, so listing a baseline directory moves it
to the front rather than repeating it.

A directory containing a **space** is fine — the systemd unit quotes its `PATH`
value, and the plist stores it as XML text.

The key is **prepend-only by design** — there is deliberately no
full-replacement override. The baseline entries can never be removed, so a typo
here costs you one tool rather than a daemon that cannot run `git`.

:::note Takes effect on the next daemon restart
Settings are read when the service file is rendered, so an edit is inert until
you run `boss daemon restart`.

`boss daemon doctor` reports both PATHs and tells them apart: the one in the
**installed** service file, which is what the running daemon actually has, and
the one the **next restart** will write. When they differ it says so and points
at the restart. It resolves `node` and `claude` under the installed PATH — not
your shell's, which is exactly the check that passes while the daemon is broken.
:::

## `plugins[]` entries

| Field     | Type   | Description                                             |
| --------- | ------ | ------------------------------------------------------- |
| `name`    | string | Plugin name (matches the suffix after `bossd-plugin-`). |
| `path`    | string | Absolute path to the plugin binary.                     |
| `enabled` | bool   | When `false`, the plugin is loaded-but-inert.           |
| `version` | string | Optional version string, informational.                 |
| `config`  | object | Plugin-specific string key/value pairs.                 |

## `claude` plugin `config` keys

| Key                            | Type                        | Default                      | Description                                                                                                                                                                                                             |
| ------------------------------ | --------------------------- | ---------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `dangerously_skip_permissions` | string `"true"` / `"false"` | `"false"` (omit for default) | Pass `--dangerously-skip-permissions` to the Claude Code CLI invoked by the `claude` plugin. Off by default. Toggle via `boss settings --skip-permissions` / `--no-skip-permissions`, or in the boss TUI settings view. |

## `repair` fields

| Field                    | Type   | Default       | Description                                              |
| ------------------------ | ------ | ------------- | -------------------------------------------------------- |
| `skills.repair`          | string | `boss-repair` | Skill invoked to attempt repair.                         |
| `cooldown_minutes`       | int    | `1`           | Minimum gap between repair attempts on the same session. |
| `poll_interval_seconds`  | int    | `5`           | Poll interval for repair status checks.                  |
| `sweep_interval_minutes` | int    | `1`           | How often the plugin sweeps for sessions needing repair. |

## `tmux_delivery` fields

Before bossanova types into an agent's pane it waits for the composer prompt to
appear. Delivery fails rather than typing into a pane that isn't ready, because
keystrokes sent early are swallowed or land in the wrong widget.

There are **two** deadlines because the two delivery paths have different
ceilings, and each is configured independently — setting one never moves the
other.

| Field                                  | Type | Default | Description                                                                                                                                                                                                                                                                                                                      |
| -------------------------------------- | ---- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `session_start_ready_deadline_seconds` | int  | `45`    | How long **each attempt** of a **session start or resume** waits for the composer prompt. This covers tmux spawn, interactive login-shell init, agent exec, node boot, and first paint. The start path makes up to **two** attempts, so the value you write is half the worst-case wall clock. Not clamped — raise it as needed. |
| `send_ready_deadline_seconds`          | int  | `5`     | How long a send into an **already-running** agent waits for the composer prompt. **Clamped to 20 seconds**, whatever you write.                                                                                                                                                                                                  |

Values of zero or below are ignored and the default applies; a settings file
written before this block existed keeps both defaults, so there is nothing to
migrate.

Raise `session_start_ready_deadline_seconds` when sessions fail to start on a
host with a slow shell profile — measured login-shell init alone has ranged from
under a second to twelve seconds on affected machines.

It is a **per-attempt** budget, not a total. The start path retries the
readiness wait once before giving up, so a start that is genuinely going to fail
spends roughly twice this value — about 90 seconds at the default of 45. Size
the knob against the boot you want to survive, then expect twice that before a
doomed start reports failure. The send deadline is **not** retried: one attempt,
always.

An attempt can also be **shortened** — not by this setting, but by whatever
context the start is running under. If the caller has less time left than a full
attempt needs, the wait is trimmed to fit rather than skipped, and the timeout
message says so: _shortened from 45s to stay inside the caller's context_. Read
that clause as pointing somewhere other than this file. The number you configured
was not the constraint, so raising it will not help; something above the start —
a request deadline, a cancelled parent — is what ran out.

The send deadline is clamped because that delivery runs inside a request the
cloud relay bounds at 30 seconds. A readiness wait that outlives the relay
returns an ambiguous result the caller **must not** retry: a retry would type the
message into the composer a second time. The 20-second ceiling leaves the relay
ten seconds of headroom.

One case is served by the shorter deadline even though it is a cold start:
sending to a chat that is asleep, with wake enabled. The wake only launches (or
resumes) the agent — it delivers nothing and never waits for the composer — so
the session-start deadline is not spent there. Your message is typed in
afterwards, by the ordinary send path, which is therefore waiting on a pane that
is still booting while holding only the **send** deadline. If that combination
fails for you, wake the chat first and send once it is live: the wake itself
does not time out on the composer, so the pane has as long as it needs, and the
separate send then meets a pane that is already up.

:::note Takes effect on the next daemon restart
Both deadlines are read once, when the daemon builds its tmux client, so an edit
here is inert until you run `boss daemon restart`.
:::

## `subagent_dispatch_grant`

Most boss skills mandate subagent dispatch as part of their protocol —
`boss-review` runs its lenses and round extensions as subagents, `boss-plan`
dispatches its drafting and reviewer extensions, `boss-repair` and `boss-epic`
fan out the same way. Coding-agent harnesses ship a standing system-prompt line
that tells the agent not to dispatch subagents unless the user asked for it.
That restriction comes from the **harness**, not from bossanova, and it has no
off switch — so without a counter-instruction those skills quietly collapse
their dispatched work into a single context and can still report a clean run.

Bossanova therefore appends a **bounded** dispatch grant to the system prompt it
builds for each chat. The grant authorises only the dispatches the running
skill's protocol already mandates, requires every dispatch to be awaited rather
than backgrounded, and treats a skill's documented inline fallback as a clean
result. It is not a general widening of tool access.

`subagent_dispatch_grant` selects which chats receive it:

| Value        | Effect                                                                                                   |
| ------------ | -------------------------------------------------------------------------------------------------------- |
| `always`     | **Default.** Unattended sessions and attended chats both receive the grant.                              |
| `unattended` | Opt out for attended chats. Only unattended sessions (cron, `boss-epic` children, detached runs) get it. |

```json
{
  "subagent_dispatch_grant": "unattended"
}
```

The key is absent from a fresh `settings.json` and behaves as `always`. An
unrecognised value also resolves to `always` — a typo must not withdraw the
shipped default, and it never fails daemon startup. Because that fallback would
otherwise discard an intended opt-out in silence, `bossd` logs a warning naming
the offending value each time it spawns or wakes a chat; check `bossd`'s log
if an opt-out does not seem to be taking effect.

Edit this key in `settings.json` directly — it is not yet exposed in the boss
settings form or the `update_settings` API, so the TUI neither shows nor
overwrites it. Unrelated settings changes made through the TUI preserve it.

Unattended sessions receive their grant in every configuration — `unattended` is
an opt-out for attended chats only, and cannot be used to withdraw authority
from an autonomous run.

The setting is read when a chat is **spawned**, so a change takes effect for
chats started afterwards. Chats already running keep the grant they were
launched with; start a new chat — or wake a sleeping one, which re-spawns it —
to pick up the change.

## Development profile

To run a development build beside the Homebrew build, keep a profile in the
repo-local `.config/` directory:

```bash
mkdir -p .config/data
```

Put this settings content in `.config/settings.json`, replacing
`/path/to/bossanova` with your repo path:

```json
{
  "app_data_dir": "/path/to/bossanova/.config/data",
  "socket_path": "/path/to/bossanova/.config/bossd.sock",
  "default_agent": "claude"
}
```

```bash
export BOSS_SETTINGS_PATH="/path/to/bossanova/.config/settings.json"
```

With that environment, the development `boss` CLI reads `.config/settings.json`,
starts or dials the daemon at `.config/bossd.sock`, and stores daemon data in
`.config/data`. The Homebrew install can continue using the standard macOS
Application Support paths.

If `BOSS_SOCKET` is set in your shell, it overrides the socket selected by
`socket_path`. Unset `BOSS_SOCKET` when using profile files unless you are
intentionally debugging a single socket path.

## Environment overrides

Cloud-sync settings (orchestrator URL, WorkOS client ID, daemon ID)
are configured exclusively via environment variables. Other settings
that have a `settings.json` field can also be overridden by env var.
Precedence (highest wins): environment variable → `settings.json` →
hardcoded default.

### `boss` (TUI / CLI)

| Variable                        | Notes                                                                                                                                                                                                                                                                                                                                                                                               |
| ------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `BOSS_WORKOS_CLIENT_ID`         | WorkOS client used by `boss login`; override when pointing at a staging orchestrator                                                                                                                                                                                                                                                                                                                |
| `BOSS_CLOUD_URL`                | overrides the authenticated cloud orchestrator URL used by `boss login` and remote CLI calls                                                                                                                                                                                                                                                                                                        |
| `BOSS_SKIP_SKILLS`              | any non-empty value suppresses the first-run skill-install prompt (persistent equivalent: `skills_declined` in `settings.json`)                                                                                                                                                                                                                                                                     |
| `BOSS_SETTINGS_PATH`            | absolute path to the settings file; selects a profile before `boss` chooses the daemon socket or reads other settings                                                                                                                                                                                                                                                                               |
| `BOSS_SOCKET`                   | explicit socket override for tests and one-off debugging; for normal profiles prefer `BOSS_SETTINGS_PATH` plus `socket_path`                                                                                                                                                                                                                                                                        |
| `BOSS_DAEMON_SKIP_LAUNCHCTL`    | any non-empty value skips `launchctl` calls in `boss daemon install`/`uninstall`/`status`                                                                                                                                                                                                                                                                                                           |
| `BOSS_REPORT_URL`               | overrides the bug-report submission URL                                                                                                                                                                                                                                                                                                                                                             |
| `BOSS_AUTH_E2E_EMAIL`           | **e2e tests only:** pre-seeds an authenticated identity so login flows can be exercised in CI; built only under the `e2e` build tag                                                                                                                                                                                                                                                                 |
| `BOSS_AUTH_E2E_NEEDS_RELOGIN`   | **e2e tests only:** flags the seeded identity as retained-but-needing-`boss login`; unset, empty, or an explicit falsey value (`0`/`false`/`no`/`off`) means no flag, `refresh_token_rejected` selects that reason and any other non-empty value selects `refresh_outcome_unknown`. Without `BOSS_AUTH_E2E_EMAIL` it seeds the identity `relogin@example.com`; built only under the `e2e` build tag |
| `BOSS_HOST_E2E_RECONNECT`       | **e2e tests only:** stages the `--host` tunnel-dropped wait screen before the TUI dials, so the reconnecting state can be captured on a harness with no network; unset, empty, or an explicit falsey value (`0`/`false`/`no`/`off`) means no seed, and any other value is used verbatim as the ssh destination shown on screen; built only under the `e2e` build tag                                |
| `BOSS_HOST_E2E_RECONNECT_POLLS` | **e2e tests only:** how many probes report the seeded tunnel as still down before it recovers unaided (default `3`); a malformed or negative value falls back to the default rather than failing the run, and it has no effect without `BOSS_HOST_E2E_RECONNECT`; built only under the `e2e` build tag                                                                                              |

### `bossd` (daemon)

| Variable                 | Notes                                                                                                                                          |
| ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `BOSSD_ORCHESTRATOR_URL` | URL `bossd` syncs with (default: `https://orchestrator.bossanova.dev`); set to `""` for local-only mode                                        |
| `BOSSD_DAEMON_ID`        | stable identifier this daemon registers under (defaults to machine hostname); each value creates a separate daemon record, so change carefully |
| `BOSSD_USER_JWT`         | bypass the keychain and pass a WorkOS JWT directly; used in CI                                                                                 |

### XDG and path variables

| Variable          | What it affects                                                                                                     |
| ----------------- | ------------------------------------------------------------------------------------------------------------------- |
| `XDG_CONFIG_HOME` | Where `settings.json` is read from on Linux (macOS uses `~/Library/Application Support/`)                           |
| `XDG_STATE_HOME`  | Where rotated log files live                                                                                        |
| `XDG_RUNTIME_DIR` | Not used for Bossanova's daemon socket; set `socket_path` in settings or use `BOSS_SOCKET` for an explicit override |
| `HOME`            | Used to resolve `~/.claude/skills/` and `~/.bossanova/`                                                             |

## GitHub App integration

Bossanova receives GitHub PR, check, status, review, and comment events through
the GitHub App webhook endpoint on the orchestrator.

Configure the GitHub App with these URLs:

| GitHub App setting              | Value                                                |
| ------------------------------- | ---------------------------------------------------- |
| Homepage URL                    | `https://app.bossanova.dev/github/setup`             |
| Setup URL                       | `https://app.bossanova.dev/github/setup`             |
| User authorization callback URL | `https://app.bossanova.dev/github/setup`             |
| Webhook URL                     | `https://orchestrator.bossanova.dev/webhooks/github` |

The Homepage URL, Setup URL, and User authorization callback URL must match
`BOSSO_GITHUB_APP_CALLBACK_URL`. Enable **Request user authorization during
installation** and **Redirect on update**. Set the GitHub webhook secret to the
same value as `BOSSO_GITHUB_APP_WEBHOOK_SECRET`.

Required repository permissions:

| Permission    | Access         |
| ------------- | -------------- |
| Pull requests | Read and write |
| Checks        | Read and write |
| Contents      | Read-only      |
| Metadata      | Read-only      |
| Statuses      | Read and write |

Subscribe to these events:

- `pull_request`
- `check_run`
- `check_suite`
- `status`
- `push`
- `issue_comment`
- `pull_request_review`

Required environment variables:

| Terraform Cloud variable                 | Kubernetes runtime env            | Source                                                                     |
| ---------------------------------------- | --------------------------------- | -------------------------------------------------------------------------- |
| `TF_VAR_bosso_github_app_id`             | `BOSSO_GITHUB_APP_ID`             | GitHub App settings page, App ID                                           |
| `TF_VAR_bosso_github_app_slug`           | `BOSSO_GITHUB_APP_SLUG`           | GitHub App URL slug                                                        |
| `TF_VAR_bosso_github_app_private_key`    | `BOSSO_GITHUB_APP_PRIVATE_KEY`    | GitHub App private key PEM, stored as one escaped env value                |
| `TF_VAR_bosso_github_app_webhook_secret` | `BOSSO_GITHUB_APP_WEBHOOK_SECRET` | Webhook secret configured on the GitHub App                                |
| `TF_VAR_bosso_github_app_callback_url`   | `BOSSO_GITHUB_APP_CALLBACK_URL`   | Frontend setup route, for example `https://app.bossanova.dev/github/setup` |
| `TF_VAR_bosso_github_app_client_id`      | `BOSSO_GITHUB_APP_CLIENT_ID`      | GitHub App settings page, Client ID                                        |
| `TF_VAR_bosso_github_app_client_secret`  | `BOSSO_GITHUB_APP_CLIENT_SECRET`  | GitHub App generated client secret                                         |

Mark the private key, webhook secret, and client secret as sensitive in
Terraform Cloud. Terraform stores the desired GitHub App values for
configuration wiring and exposes the runtime values through the
`kubernetes_secret_bosso` output. Do not pass secret values as command-line
arguments; process argv can be inspected.

```bash
cd infra/kustomize
./pull-secrets.sh production
```

For staging, run `./pull-secrets.sh staging`. The generated `.env-bosso` file is
gitignored and must not be committed.

### Bosso GKE Runtime

Bosso runs on GKE with Cloud SQL Postgres, Redis, Google Artifact Registry, and
the Google external ALB. Terraform writes the Kubernetes secret values:

```bash
BOSSO_DB_DRIVER=postgres
BOSSO_DATABASE_URL=postgres://...
BOSSO_MULTI_INSTANCE=true
BOSSO_REDIS_URL=redis://bs-redis-service.<namespace>.svc.cluster.local:6379/0
BOSSO_ROUTING_PROVIDER=kubernetes
BOSSO_WEBHOOK_ROUTING_URL=http://bs-bosso-service.<namespace>.svc.cluster.local:80
```

Generate each `BOSSO_INTERNAL_ROUTING_TOKEN` as a high-entropy secret, for
example with `openssl rand -base64 32`, and set the same value on every bosso
instance in that environment. The token authenticates internal routed delivery
when one bosso instance forwards webhook or command work to the instance that
owns the target daemon stream.

Use this token only for internal routed delivery between bosso
instances; it is separate from the GitHub App webhook secret.

Kubernetes sets `BOSSO_INSTANCE_ID` from the pod name. Local or manual
multi-instance runs must set `BOSSO_INSTANCE_ID` on each process to a distinct
stable value.

Webhook behavior:

- GitHub signs webhook payloads with the app webhook secret.
- The orchestrator verifies the signature, then routes the event by
  `installation_id` to the WorkOS user that completed setup.
- Pull request events trigger a targeted refresh for the affected repository
  and PR.
- The polling fallback backs off for 5 minutes on repositories that recently
  delivered webhooks.
