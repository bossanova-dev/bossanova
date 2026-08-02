<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Account Management

### `boss account`

Manage agent accounts (credential registry)

### `boss account add [provider] [flags]`

Register an agent account

**Flags:**

- `--credential-file` — Read the credential from a file (or '-' for stdin); preferred over --token
- `--label` — Human label, unique per provider (required for the non-interactive flag path)
- `--priority` — Sort order; lower = preferred (default: 0)
- `--provider` — Account provider (claude|codex) (or pass as a positional arg)
- `--timeout` — Deadline for an interactive registration walkthrough (default: 10m0s)
- `--token` — Credential token (prefer --credential-file - or stdin to keep it out of shell history)
- `--token-stdin` — claude only: read the setup token from stdin instead of running the walkthrough

### `boss account ls [flags]`

List accounts

**Flags:**

- `--json` — Emit a stable JSON schema instead of a table
- `--provider` — Filter by provider (claude|codex)
- `--refresh` — Force a live usage probe of each account before listing

### `boss account refresh <account-id> [flags]`

Replace an account's stored credential

**Flags:**

- `--credential-file` — Read the credential from a file (or '-' for stdin); preferred over --token
- `--json` — Emit a stable JSON schema instead of text
- `--test` — Validate the refreshed credential after saving
- `--token` — Credential token (prefer --credential-file - or stdin to keep it out of shell history)

### `boss account remove <account-id>`

Remove an account and its stored credential

### `boss account switch <session> <account> [flags]`

Stop a session's live chat, rebind it to the chosen account, and resume

**Flags:**

- `--chat` — Target a specific agent chat (agent session id); default: the session's primary live chat
- `--force` — Interrupt a mid-turn / WORKING chat

### `boss account test <account-id> [flags]`

Validate an account's credential and record the outcome

**Flags:**

- `--json` — Emit a stable JSON schema instead of text

### `boss account update <account-id> [flags]`

Update account metadata

**Flags:**

- `--allowed-models` — Replace the allowed-models set (comma-separated)
- `--label` — Set the label
- `--priority` — Set the priority (lower = preferred) (default: 0)
- `--status` — Set the status (active|disabled)
