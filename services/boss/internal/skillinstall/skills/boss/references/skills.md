<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Skills

### `boss init [flags]`

Write a detected .boss-skills.json for this repository

**Flags:**

- `--agent` — Agent for the pipeline cron jobs: claude or codex (default: claude when both qualify)
- `--allow-branch` — Apply .boss-project.json from a checkout that is not on the repo's default branch
- `--assignee-me` — Include only my tickets when no shared assignee filter exists
- `--create-labels` — Create missing pipeline labels in the resolved Linear team (needs LINEAR_API_KEY)
- `--cron` — Create or converge the pipeline cron jobs for these stages (plan,build,verify,retro)
- `--dir` — Repository directory to inspect and write into (default: the working directory)
- `--force` — Overwrite an existing .boss-skills.json instead of refusing
- `--label` — Linear label override role=name (repeatable)
- `--linear-key-stdin` — Read the Linear API key from stdin
- `--merge` — Merge into an existing .boss-skills.json
- `--no-register` — Require an existing repository registration
- `--register` — Find or register this repository with local bossd
- `--sentry-org` — Sentry organization slug (requires a token)
- `--sentry-token-stdin` — Read the Sentry token from stdin
- `--state` — Linear state override role=name (repeatable)
- `--store-env-keys` — Store LINEAR_API_KEY and SENTRY_AUTH_TOKEN from the environment
- `--team` — Linear team name to pin as trackerConfig.linear.team (a key or id resolves to the name only when LINEAR_API_KEY lists it)
- `--trusted` — Register the repository as trusted (Codex project trust and proof secrets for its sessions)
- `--update-crons` — Update an existing pipeline job's prompt, gate and agent when they differ
- `--yes` — Apply .boss-project.json cron prompt and gate changes without asking

### `boss skills`

Manage installed boss skills

### `boss skills check [flags]`

Check installed boss skills against this binary and checkout sources

**Flags:**

- `--agent` — Restrict to one agent: claude, codex or hermes (default: all on PATH)
- `--gate` — Fail only when installed skills drift from checkout source for paths not edited by this branch
- `--heal` — With gate, repair unedited lossless or behind drift from authenticated default-branch bytes

### `boss skills install [flags]`

Install or refresh boss skills (fresh-installs missing trees); --force reinstalls even when current

**Flags:**

- `--agent` — Restrict to one agent: claude, codex or hermes (default: all on PATH)
- `--force` — Reinstall (Extract) unconditionally, even when current

### `boss skills sync [flags]`

Refresh installed boss skills from the selected checkout or embedded payload (update-only, no prompt)

**Flags:**

- `--agent` — Restrict to one agent: claude, codex or hermes (default: all on PATH)
