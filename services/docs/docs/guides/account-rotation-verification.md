---
title: Verifying Account Rotation
description: 'Follow an end-to-end checklist for local account rotation setup and optional smoke checks.'
sidebar_position: 2
---

# Verifying Account Rotation

Use this checklist after reading [Account Rotation](./account-rotation.md). It
verifies the local setup path: account registration, registry visibility,
credential checks, session binding, manual switching, usage-limit rotation, and
the UI surfaces that show the active account and rotation history.

## Prerequisites

- A local `bossd` daemon is running and reachable by the `boss` CLI.
- The `claude` and/or `codex` agent-runner plugin is installed for the provider
  you want to rotate.
- You have at least one additional account for each provider you want to rotate.
  Your existing local CLI login remains the system-default account; it is not
  imported into the rotation registry.
- For Codex, open [ChatGPT settings](https://chatgpt.com/#settings), go to
  Settings -> Security, and enable **Enable device code authorization for
  Codex** before running `boss account add codex`. Bossanova runs
  `codex login --device-auth` during registration, and that flow cannot complete
  without the toggle.

Registration is local-daemon only. `boss account add` stores credentials in your
local OS keyring through your local daemon; it does not register accounts on a
remote daemon.

## 1. Register Accounts

Register Claude with the interactive setup-token walkthrough:

```bash
boss account add claude --label claude-backup --priority 10
```

Register Codex with the device flow:

```bash
boss account add codex --label codex-backup --priority 10
```

Useful flags:

- `--label` sets a unique human label for the provider.
- `--email` stores an informational email in local metadata.
- `--priority` sets selection order; lower values are preferred.
- `--token-stdin` is Claude-only and reads the setup token from stdin instead of
  running the walkthrough.
- `--token` and `--credential-file` are for non-interactive registration paths;
  prefer `--credential-file -` over `--token` so secrets do not land in shell
  history.

## 2. Confirm The Registry

List accounts in the table view:

```bash
boss account ls
```

List the stable JSON schema for scripts:

```bash
boss account ls --json
```

For provider-specific checks:

```bash
boss account ls --provider claude
boss account ls --provider codex --json
```

A healthy registered account has the expected `provider`, `status: active`,
priority, and health metadata. The JSON schema includes `id`, `provider`,
`label`, `email`, `status`, `priority`, `health`, `tier`, `allowed_models`,
`cooldown_until`, `last_used_at`, `last_test_ok_at`, `last_test_error`,
`created_at`, and `updated_at`. It does not include a credential field.

## 3. Test Each Credential

Run the live account test for each registered account:

```bash
boss account test <account-id>
```

Use JSON when you want a machine-readable result:

```bash
boss account test <account-id> --json
```

Text output reports `Account <account-id>: ok` or
`Account <account-id>: error: ...`, plus `Provider check ran: yes|no`. JSON output
contains `account`, `live_smoke_ran`, and `detail`.

## 4. Verify New-Session Auto-Bind

Start a new session in a repository that uses the provider you registered. The
session should bind to an eligible registered account automatically; there is no
account picker or prompt.

Open the session detail view and confirm the active account badge shows the
registered account instead of the system-default account. If no registered
account is eligible, the session uses the system-default account.

## 5. Verify Manual Switch

Move a session to a specific account:

```bash
boss account switch <session> <account>
```

`<account>` can be an account id or label. To return to the system-default
account, pass any of these sentinels:

```bash
boss account switch <session> system-default
boss account switch <session> 0
boss account switch <session> none
```

Target a specific live agent chat when a session has more than one:

```bash
boss account switch <session> <account> --chat <agent-session-id>
```

A mid-turn `WORKING` chat is rejected by default. To interrupt and switch
anyway:

```bash
boss account switch <session> <account> --force
```

## 6. Verify Usage-Limit Rotation

To observe a real rotation, run a session until the provider reports a usage cap.
When rotation is working, Bossanova:

1. detects the provider usage-limit message,
2. applies a cooldown to the exhausted account,
3. selects the next eligible registered account for the same provider,
4. respawns and resumes the session under that account, and
5. posts an in-chat notice naming the switch.

Check the result in session detail: the active account badge changes, rotation
history records the decision, and daemon logs include the structured rotation
event. If every account for that provider is cooling, the session parks with an
all-accounts-limited status until the earliest cooldown expires.

The smoke script below does not simulate this step. A scripted fake rotation
path exists only in internal tests; there is no production runtime fake-rotation
harness.

## 7. Verify Web Account Badge And Switch

Open the session in the web app. Session detail shows an account badge and
switch control for the active rotation account. Use the switch control to move
the session to another eligible account, then confirm the active account badge
updates and the rotation history remains visible.

There is no global web account-setup UI. Add and test accounts through the local
`boss account add`, `boss account ls`, and `boss account test` CLI commands.

## 8. Verify TUI Surfaces

The TUI session detail view shows rotation warnings and rotation history for a
session. Account settings controls for registration and management are
forthcoming with Epic B / BOS-258; until that lands, use the CLI for account
setup and the TUI for session-level status.

## Unmanaged Local Credentials

Your pre-existing `~/.claude` or `~/.codex` login is the implicit
system-default account, also called account 0. It is never imported into the
registry, never rotated into as a registered account, and never has its
credential read by Bossanova. It is the fallback used when no registered account
is bound.

Account 0 does not participate in automatic account selection. Add registered
accounts with `boss account add` before expecting rotation.

## Kill-Switch Reminder

Disable automatic rotation globally:

```bash
boss settings --no-rotation
```

Re-enable it:

```bash
boss settings --rotation
```

Manual switching still works while automatic rotation is disabled.

## Optional Smoke Script

Run local registry/list checks without triggering rotation:

```bash
node scripts/account-rotation-smoke.mjs
```

See what it would run:

```bash
node scripts/account-rotation-smoke.mjs --dry-run
```

Also run per-account live credential tests:

```bash
node scripts/account-rotation-smoke.mjs --test
```

An account is reported `ok` only when its credential test passes **and** it is
rotation-eligible — active, healthy, and not in cooldown — matching the
predicate the rotation selector uses. A disabled, unhealthy, or still-cooling
account is reported `error` even if its credentials are valid, because it cannot
be auto-bound or rotated in.

The script honors `BOSS_BIN`, so local builds can be checked with:

```bash
BOSS_BIN=/path/to/boss node scripts/account-rotation-smoke.mjs --test
```

The script prints provider counts and non-secret metadata only: account id,
provider, status, priority, health, and cooldown. It does not print email,
label, raw JSON, or credentials. It does not trigger usage-limit rotation.
