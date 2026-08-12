<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Settings & Auth

### `boss auth-status`

Show authentication status

```bash
boss auth-status
```

### `boss config`

Manage configuration

### `boss config init [flags]`

Initialize bossd plugin settings in settings.json from a plugin directory

**Flags:**

- `--plugin-dir` — Directory containing plugin binaries (auto-detected if omitted)

```bash
boss config init
boss config init --plugin-dir ./plugins
```

### `boss login`

Log in to Bossanova cloud (WorkOS)

```bash
boss login
```

### `boss logout`

Log out and remove stored credentials

```bash
boss logout
```

### `boss settings [flags]`

View or update global settings

**Flags:**

- `--default-agent` — Set the default agent plugin (e.g. claude, opencode)
- `--managed-accounts` — Enable managed accounts (bossd credential rotation)
- `--no-managed-accounts` — Disable managed accounts (use the terminal's own login)
- `--no-skip-permissions` — Disable Claude --dangerously-skip-permissions
- `--poll-interval` — Set poll interval in seconds (0 = default) (default: 0)
- `--skip-permissions` — Enable Claude --dangerously-skip-permissions
- `--worktree-dir` — Set worktree base directory

```bash
boss settings
boss settings --worktree-dir ~/work/bossanova/worktrees
boss settings --skip-permissions
```
