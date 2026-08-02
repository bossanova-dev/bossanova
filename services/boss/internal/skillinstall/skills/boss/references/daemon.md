<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Daemon Management

### `boss daemon`

Manage the bossd daemon

### `boss daemon install [flags]`

Install bossd as a background service (launchd on macOS, systemd on Linux)

**Flags:**

- `--force` — Overwrite existing service file

```bash
boss daemon install
boss daemon install --force
```

### `boss daemon restart`

Restart the bossd daemon

Restarts the bossd daemon via the platform service manager. Errors out if the daemon isn't installed.

```bash
boss daemon restart
```

### `boss daemon rotate-token`

Rotate the daemon socket auth token (regenerated on next daemon start)

### `boss daemon start`

Start the bossd daemon

No-op if it's already running. Falls back to spawning bossd directly if it isn't installed as a LaunchAgent.

```bash
boss daemon start
```

### `boss daemon status`

Show bossd daemon status

```bash
boss daemon status
```

### `boss daemon stop [flags]`

Stop the bossd daemon

Stops the bossd daemon for the current profile via the platform service manager or profile metadata. Idempotent — quietly succeeds if the daemon is already stopped or not installed. Normal stops leave plugin processes alone — bossd reaps its own children. Use `--all-standalone` only for explicit cleanup of every user-owned standalone bossd process and its `bossd-plugin-*` children across profiles.

**Flags:**

- `--all-standalone` — Stop all user-owned bossd processes instead of only the current profile

```bash
boss daemon stop
boss daemon stop --all-standalone
```

### `boss daemon uninstall`

Uninstall the bossd LaunchAgent

```bash
boss daemon uninstall
```
