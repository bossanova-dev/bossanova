<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Daemon Management

### `boss daemon`

Manage the bossd daemon

### `boss daemon doctor`

Diagnose the bossd daemon: staged binary, LaunchAgent path, macOS folder permissions, and live upstream auth/registration state

### `boss daemon install [flags]`

Install bossd as a background service (launchd on macOS, systemd on Linux)

**Flags:**

- `--force` — Overwrite existing service file

```bash
boss daemon install
boss daemon install --force
```

### `boss daemon restart [flags]`

Restart the bossd daemon

Restarts the bossd daemon via the platform service manager. Errors out if the daemon isn't installed.

**Flags:**

- `--json` — Emit the restart outcome as a stable JSON schema, so a driver can tell a supervised restart from one that fell back to an unsupervised direct start

```bash
boss daemon restart
```

### `boss daemon rotate-token`

Rotate the daemon socket auth token (regenerated on next daemon start)

### `boss daemon start [flags]`

Start the bossd daemon

No-op if it's already running. Falls back to spawning bossd directly if no service (LaunchAgent, watchdog or systemd unit) is installed. If a service is installed but does not bring the daemon up, it exits non-zero without starting an unsupervised daemon — run `boss daemon doctor` or `boss daemon restart`, or pass `--detached` to accept an unsupervised daemon deliberately.

**Flags:**

- `--detached` — Start an unsupervised bossd directly if the installed service manager does not bring the daemon up

```bash
boss daemon start
boss daemon start --detached
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
