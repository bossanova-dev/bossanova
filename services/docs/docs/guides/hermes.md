---
title: Hermes Agent
description: Drive your Bossanova sessions from a Hermes Agent chat through the native bossanova plugin.
slug: /guides/hermes
---

# Hermes Agent

Once the native `bossanova` Hermes plugin is installed, [Hermes Agent](https://github.com/NousResearch/hermes-agent) lists, creates, messages, monitors and merges your Bossanova sessions for you. The plugin exposes every Bossanova [MCP tool](./mcp.md) as a Hermes tool named `bossanova_<tool>`, adds a `/bossanova status` command, and ships the `bossanova:orchestrator` skill.

`boss hermes install` writes the plugin into your Hermes home, points it at your own `bossd` socket and settings file, installs the boss skills into Hermes, and enables the plugin. The plugin starts the `boss-mcp` binary over stdio, so no loopback port is shared between users.

## Set it up, once per OS user

Every OS user that runs Hermes needs its own run of these steps, as that user. On a machine where several agents each run as their own OS user, each one gets its own daemon, its own settings file and its own plugin install.

### 1. Run a daemon that starts at login

```bash
boss daemon install
```

On a macOS host where users are routinely backgrounded by fast user switching (an agent farm, say), use the `unattended` supervision mode instead, so the daemon comes back without anyone logging in. See [bossd supervision modes](https://github.com/bossanova-dev/bossanova/blob/main/docs/ops/daemon-supervision-modes.md) for how to install it and what it needs.

### 2. Pick a non-default settings profile, if you want one

To run this user's daemon against a settings file other than the OS default, export it before installing the daemon and the plugin:

```bash
export BOSS_SETTINGS_PATH=/Users/agent1/bossanova/settings.json
boss daemon install
```

The `socket_path` (or `app_data_dir`) in that file decides where the daemon listens.

### 3. Install the plugin

```bash
boss hermes install
```

It resolves each value in this order, and prints what it used:

| Value         | Resolution order                                                                                               |
| ------------- | -------------------------------------------------------------------------------------------------------------- |
| Settings file | `--settings`, then `BOSS_SETTINGS_PATH`, then the OS default                                                   |
| bossd socket  | `--socket`, then `BOSS_SOCKET`, then `socket_path` (or `app_data_dir`) in that settings file, then the default |
| Hermes home   | `--hermes-home`, then `HERMES_HOME`, then `~/.hermes`                                                          |
| MCP binary    | `boss-mcp` (or `mcp`) next to `boss`, then on `PATH`                                                           |

The socket is read from the settings file the command selected, never from whichever profile your shell happens to export, so `--settings` really does select the profile.

What it does:

- Writes the plugin to `<hermes home>/plugins/bossanova/`, with the resolved values in `boss_install.json`. The settings path is recorded only for a non-default profile.
- Installs the boss skills into `<hermes home>/skills/`.
- Runs `hermes plugins enable bossanova` with `HERMES_HOME` set, when `hermes` is on your `PATH`. Without `hermes`, it leaves the plugin in place and prints the command to run later. `--no-enable` skips this step.
- Warns, without failing, when the daemon socket is unreachable or no daemon login registration is installed, and prints the `boss daemon install` command for this profile.

`boss hermes install` refuses to replace an existing `plugins/bossanova/` directory that it did not write (one without `boss_install.json`). Pass `--force` to replace it. Re-running it over its own install is always safe.

Preview everything without writing a file or running a command:

```bash
boss hermes install --dry-run
```

To install into a Hermes profile that has its own home, pass `--hermes-home`:

```bash
boss hermes install --hermes-home ~/.hermes-work
```

### 4. Check it from Hermes

The tools load in the next Hermes session. Start one and run:

```text
/bossanova status
```

### 5. Ask Hermes to drive Bossanova

Some prompts to try:

- "List my sessions."
- "Start a session on repo X to fix Y."
- "What is session Z waiting on?"

## Keep it current

```bash
boss hermes status
```

reports the resolved settings file, socket and Hermes home, whether the daemon is reachable, the `boss-mcp` path, and whether the installed plugin still matches what `boss hermes install` would write now. The plugin goes stale after a `boss` upgrade, a new MCP tool, or a moved `boss-mcp` binary: re-run `boss hermes install` when it says `stale`. It also reports whether Hermes lists the plugin as enabled (when `hermes` is on your `PATH`) and whether the Hermes copy of the boss skills is current. Add `--json` for a machine-readable report.

To turn the plugin off, let Hermes own that state:

```bash
hermes plugins disable bossanova
```

## Several users on one machine

- Each OS user runs its own `boss daemon install` and `boss hermes install`. The plugin install records that user's socket and settings file, so users never reach each other's daemon.
- Loopback ports must not collide between users: give each user's settings file its own `failover_proxy_port`, and if you also run the MCP HTTP service, its own `boss mcp install --port`.
- To override a value later, per Hermes profile, use the plugin's settings form (`mcp_bin`, `socket_path`, `settings_path`, `tool_timeout_seconds`). An empty field uses the value `boss hermes install` wrote.
