<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Skills

### `boss skills`

Manage installed boss skills

### `boss skills check [flags]`

Check installed boss skills against this binary and checkout sources

**Flags:**

- `--agent` — Restrict to one agent: claude or codex (default: all on PATH)

### `boss skills install [flags]`

Install or refresh boss skills (fresh-installs missing trees); --force reinstalls even when current

**Flags:**

- `--agent` — Restrict to one agent: claude or codex (default: all on PATH)
- `--force` — Reinstall (Extract) unconditionally, even when current

### `boss skills sync [flags]`

Refresh installed boss skills to match this binary (update-only, no prompt)

**Flags:**

- `--agent` — Restrict to one agent: claude or codex (default: all on PATH)
