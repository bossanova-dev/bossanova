<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Repository Management

### `boss repo`

Manage repositories

### `boss repo add`

Register a repository

```bash
boss repo add
```

### `boss repo ls`

List registered repositories

```bash
boss repo ls
```

### `boss repo remove <repo-id>`

Remove a registered repository

```bash
boss repo remove my-repo
```

### `boss repo update <repo-id> [flags]`

Update repository settings

**Flags:**

- `--auto-merge` — Enable auto-merge
- `--auto-merge-dependabot` — Enable auto-merge for Dependabot PRs
- `--auto-repair` — Enable automatic repair (failing checks, conflicts, review feedback)
- `--delete-branches` — Enable deleting safe local branches after archiving
- `--keep-branches-current` — Enable proactively rebasing in-flight session branches when the base advances
- `--merge-strategy` — Set merge strategy (merge, rebase, squash)
- `--name` — Set display name
- `--no-auto-merge` — Disable auto-merge
- `--no-auto-merge-dependabot` — Disable auto-merge for Dependabot PRs
- `--no-auto-repair` — Disable automatic repair
- `--no-delete-branches` — Disable deleting local branches after archiving
- `--no-keep-branches-current` — Disable proactively rebasing in-flight session branches when the base advances
- `--setup-script` — Set setup script (empty string to clear)

```bash
boss repo update my-repo --name "My Repo" --merge-strategy squash
boss repo update my-repo --auto-merge-dependabot
```
