---
title: Settings File
description: 'JSON reference for the bossanova settings.json: every field, default, and precedence rule.'
---

# Settings File

Bossanova reads global settings from a JSON file on disk:

- **macOS:** `~/Library/Application Support/bossanova/settings.json`
- **Linux:** `$XDG_CONFIG_HOME/bossanova/settings.json` (defaults to `~/.config/bossanova/settings.json`)

The file is optional. When it's absent, defaults apply. Both `boss` and
`bossd` read the same file.

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

| Field                   | Type   | Default                  | Description                                                                                                      |
| ----------------------- | ------ | ------------------------ | ---------------------------------------------------------------------------------------------------------------- |
| `worktree_base_dir`     | string | `~/.bossanova/worktrees` | Directory where per-session git worktrees are created. Auto-created on load.                                     |
| `default_agent`         | string | `claude`                 | Name of the default agent plugin used for new sessions.                                                          |
| `skills_declined`       | bool   | `false`                  | Set after the user declines the one-time skills install prompt so it's not shown again.                          |
| `poll_interval_seconds` | int    | `120`                    | How often the Terminal UI (TUI) polls for PR display status, in seconds.                                         |
| `plugins`               | array  | auto-discovered          | Plugin binaries to load (see below). If unset, `bossd` auto-discovers `bossd-plugin-*` binaries next to its own. |
| `repair`                | object | defaults below           | Repair plugin configuration.                                                                                     |

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
| `BOSS_SKIP_SKILLS`           | any non-empty value suppresses the first-run skill-install prompt (persistent equivalent: `skills_declined` in `settings.json`)     |
| `BOSS_SOCKET`                | overrides the path to the local `bossd` Unix-domain socket                                                                          |
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

| Variable          | What it affects                                                                           |
| ----------------- | ----------------------------------------------------------------------------------------- |
| `XDG_CONFIG_HOME` | Where `settings.json` is read from on Linux (macOS uses `~/Library/Application Support/`) |
| `XDG_STATE_HOME`  | Where rotated log files live                                                              |
| `XDG_RUNTIME_DIR` | Where the `bossd` Unix socket lives (override with `BOSS_SOCKET`)                         |
| `HOME`            | Used to resolve `~/.claude/skills/` and `~/.bossanova/`                                   |
