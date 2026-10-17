<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Repository Management

### `boss project`

Work with the committed project config file (.boss-project.json)

### `boss project apply [flags]`

Converge this daemon onto .boss-project.json

**Flags:**

- `--allow-branch` — Apply from a checkout that is not on the repo's default branch
- `--cloud` — Also converge your cloud triggers and the organization's session webhooks (needs boss login)
- `--dir` — Checkout whose top-level .boss-project.json is applied (default: .)
- `--dry-run` — Print the plan and write nothing
- `--json` — Print the plan and the apply result as JSON
- `--org` — Organization id for --cloud (default: cloud.organization, else the repository's organization, else your only one)
- `--prune` — Delete managed crons (and, with --cloud, triggers and webhooks) the file dropped instead of disabling them
- `--rotate-secrets` — With --cloud, rotate the signing secrets of the project's HTTP triggers and webhooks
- `--yes` — Apply changes that need confirmation without asking

### `boss project diff [flags]`

Show what applying .boss-project.json would change on this daemon

**Flags:**

- `--cloud` — Also plan your cloud triggers and the organization's session webhooks (needs boss login)
- `--dir` — Checkout whose top-level .boss-project.json is read (default: .)
- `--exit-code` — Exit 1 when the plan is not converged, 2 on error
- `--json` — Print the plan as JSON
- `--org` — Organization id for --cloud (default: cloud.organization, else the repository's organization, else your only one)

### `boss project export [flags]`

Print this checkout's boss setup as a project config file

Reads the local daemon's registration of this checkout (repository settings, cron jobs and the daemon's own identity) and prints it as a canonical `.boss-project.json`. Pipeline cron jobs export symbolically (their stage, no prompt or gate), so the same job on two machines exports identically. With `--cloud` it also reads your inbound triggers on this repository and the organization's session webhooks. Secrets are never written: a repository credential that is set appears only as a reference to a `BOSS_*` environment variable. Warnings go to stderr, so stdout can be piped. `--write` writes the file to the main checkout and refuses to replace an existing one without `--force`.

**Flags:**

- `--cloud` — Include cloud triggers and session webhooks (needs boss login)
- `--dir` — Checkout to export (any worktree of the repository) (default: .)
- `--force` — With --write, overwrite an existing file
- `--org` — Organization id for --cloud (default: the repository's organization, else your only one)
- `--project` — Project slug (default: the existing file's, else the repository name)
- `--write` — Write the file to the main checkout instead of printing it

```bash
boss project export
boss project export --cloud --write
```

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
- `--trusted` — Trust this repository's own configuration: Codex project trust and proof secrets for its sessions (--trusted=false revokes)
- `--worktree-base-dir` — Set the absolute directory new session worktrees are created under (applies to new sessions)

```bash
boss repo update my-repo --name "My Repo" --merge-strategy squash
boss repo update my-repo --auto-merge-dependabot
```
