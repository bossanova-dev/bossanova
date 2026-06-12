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

| Field                   | Type   | Default                     | Description                                                                                                                       |
| ----------------------- | ------ | --------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `worktree_base_dir`     | string | `~/.bossanova/worktrees`    | Directory where per-session git worktrees are created. Auto-created on load.                                                      |
| `app_data_dir`          | string | platform default            | Absolute directory for local daemon data: `bossd.db`, `bossd.lock`, profile plugin discovery, and default socket placement.       |
| `socket_path`           | string | derived from data directory | Absolute path to the local `bossd` Unix-domain socket. If unset and `app_data_dir` is set, defaults to `app_data_dir/bossd.sock`. |
| `default_agent`         | string | `claude`                    | Name of the default agent plugin used for new sessions.                                                                           |
| `skills_declined`       | bool   | `false`                     | Set after the user declines the one-time skills install prompt so it's not shown again.                                           |
| `poll_interval_seconds` | int    | `120`                       | How often the Terminal UI (TUI) polls for PR display status, in seconds.                                                          |
| `plugins`               | array  | auto-discovered             | Plugin binaries to load (see below). If unset, `bossd` auto-discovers `bossd-plugin-*` binaries next to its own.                  |
| `repair`                | object | defaults below              | Repair plugin configuration.                                                                                                      |

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

| Variable                     | Notes                                                                                                                               |
| ---------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `BOSS_WORKOS_CLIENT_ID`      | WorkOS client used by `boss login`; override when pointing at a staging orchestrator                                                |
| `BOSS_CLOUD_URL`             | overrides the authenticated cloud orchestrator URL used by `boss login` and remote CLI calls                                        |
| `BOSS_SKIP_SKILLS`           | any non-empty value suppresses the first-run skill-install prompt (persistent equivalent: `skills_declined` in `settings.json`)     |
| `BOSS_SETTINGS_PATH`         | absolute path to the settings file; selects a profile before `boss` chooses the daemon socket or reads other settings               |
| `BOSS_SOCKET`                | explicit socket override for tests and one-off debugging; for normal profiles prefer `BOSS_SETTINGS_PATH` plus `socket_path`        |
| `BOSS_DAEMON_SKIP_LAUNCHCTL` | any non-empty value skips `launchctl` calls in `boss daemon install`/`uninstall`/`status`                                           |
| `BOSS_REPORT_URL`            | overrides the bug-report submission URL                                                                                             |
| `BOSS_AUTH_E2E_EMAIL`        | **e2e tests only:** pre-seeds an authenticated identity so login flows can be exercised in CI; built only under the `e2e` build tag |

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

Generate each `BOSSO_INTERNAL_WEBHOOK_TOKEN` as a high-entropy secret, for
example with `openssl rand -base64 32`, and set the same value on every bosso
instance in that environment. The token authenticates internal routed webhook
delivery when one bosso instance forwards webhook work to the instance that owns
the target daemon stream.

Use this token only for internal routed webhook delivery between bosso
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
