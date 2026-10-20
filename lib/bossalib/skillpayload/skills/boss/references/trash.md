<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Trash Management

### `boss trash`

Manage archived sessions

### `boss trash delete <session-id> [flags]`

Permanently delete an archived session

**Flags:**

- `--yes`, `-y` — Skip confirmation prompt

```bash
boss trash delete abc123
boss trash delete abc123 --yes
```

### `boss trash empty [flags]`

Permanently delete all archived sessions

**Flags:**

- `--older-than` — Only delete sessions archived longer than this duration (e.g. 30d)

```bash
boss trash empty
boss trash empty --older-than 30d
```

### `boss trash ls`

List archived sessions

```bash
boss trash ls
```

### `boss trash restore <session-id>`

Restore an archived session

Restores an archived session, recreating its worktree.

```bash
boss trash restore abc123
```
