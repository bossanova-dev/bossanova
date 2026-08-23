---
title: Account Rotation
description: 'Register multiple provider accounts and let Bossanova rotate them automatically when a usage cap is hit.'
---

import CommandTabs from '@site/src/components/CommandTabs';

# Account Rotation

Coding-agent subscriptions (Claude and Codex) have usage caps. When a session
hits one, it stalls until the cap resets. If you have more than one account for
a provider, Bossanova can detect the cap, put the exhausted account on cooldown,
and move the session onto another account automatically — so long-running and
scheduled work keeps making progress instead of parking for hours.

This page explains what an account is, how to register one, how rotation
behaves, and how to turn it off.

## Accounts

An **account** is a registered provider credential that Bossanova can run
sessions under. Each account has:

- a **provider** (`claude` or `codex`),
- a human **label** (unique per provider),
- a **status** (`active` or `disabled`),
- a **priority** (lower is preferred when selecting the next account), and
- a **cooldown** — a "do not select until T" window applied after the account
  hits a usage cap.

Account **metadata** (label, status, priority, cooldown, last-tested time) lives
in the daemon's local store. The **secret credential itself is stored in your OS
keyring**, never in the database and never echoed back by any command.

### The system-default account

Your pre-existing `~/.claude` / `~/.codex` login is the implicit
**system-default account** ("account 0"). It is never imported into the account
registry and its credential is never read or injected by rotation — it is simply
the login the agent CLI already uses on its own. Registered accounts are
additive: rotation only becomes possible once you add at least one extra account
for a provider.

## Registering accounts

Use `boss account add` with the provider as a positional argument. Both flows
are interactive and register the credential on your **local** daemon (they
cannot target a remote daemon):

Claude runs the setup-token walkthrough:

<CommandTabs
chat='"add a new claude account"'
cli="boss account add claude"
mcp="add_account"
/>

Codex runs the interactive device flow:

<CommandTabs
chat='"add a new codex account"'
cli="boss account add codex"
mcp="add_account"
/>

The MCP `add_account` tool is **not** the interactive walkthrough: it registers
a credential you have already obtained (a Claude setup-token string, or the
contents of a Codex `auth.json`) and stores it in the keyring. Run the CLI or
the TUI to obtain one.

Useful flags (see `boss account add --help`):

- `--label` — a human label for the account (unique per provider).
- `--priority` — sort order; lower is preferred.
- `--token-stdin` — **claude only**: read the setup token from stdin instead of
  running the walkthrough. Codex has no stdin path (its device flow needs an
  interactive browser round-trip).

Manage the registry with the sibling commands:

List accounts (add `--json` for scripts):

<CommandTabs
chat='"list my registered accounts"'
cli="boss account ls"
mcp="list_accounts"
/>

Validate a credential and record the result:

<CommandTabs
chat='"test account <account-id>"'
cli="boss account test <account-id>"
mcp="test_account"
/>

Change label, priority, status, or allowed models:

<CommandTabs
chat='"update account <account-id>"'
cli="boss account update <account-id>"
mcp="update_account"
/>

Remove an account and its stored credential:

<CommandTabs
chat='"remove account <account-id>"'
cli="boss account remove <account-id>"
mcp="remove_account"
/>

`remove_account` is classed as a destructive tool, so the MCP call also requires
`confirm: true` and is rejected without it.

### Register accounts in the TUI

The CLI and the TUI are **equivalent entry points** to the same registry — use
whichever you prefer. To manage accounts inside the TUI:

1. Open the **Settings** view and press **`a`** (the action bar shows
   `[a]ccounts`) to open the accounts list. Its columns are LABEL, PROVIDER,
   STATUS, HEALTH, UTIL5H, UTIL7D, AGE, COOLDOWN, and LAST TEST.
   Codex publishes a single weekly (7-day) rate-limit window, so a Codex row
   populates UTIL7D and leaves UTIL5H at `0%`; Claude publishes both windows.
2. In the accounts list, use these keys:
   - **`a`** — register a new account. A `claude | codex` chooser runs the same
     interactive setup-token / device-flow walkthrough as `boss account add`,
     right inside the TUI (credentials stay masked).
   - **`e`** / **`enter`** — edit the selected account's label, status, or
     priority inline.
   - **`x`** — disable or re-enable the selected account.
   - **`d`** — remove the account (confirm-gated; purges the stored keyring
     credential).
   - **`t`** — run a live credential test.
   - **`r`** — refresh usage metadata.

Accounts you register in the TUI are the same records `boss account ls` shows on
the command line; there is one registry per local daemon.

You can always move a specific session onto a specific account by hand:

<CommandTabs
chat='"switch session <session> to account <account>"'
cli="boss account switch <session> <account>"
mcp="switch_account"
/>

Manual switching stops the session's live chat, rebinds it to the chosen
account, and respawns with resume. To send a session back to the system-default
account, `boss account switch` accepts `system-default` (along with a few
equivalent spellings) and resolves it to the empty account id the daemon reads
as account 0. That resolution is a CLI convenience: the `switch_account` tool
does no such mapping, so through MCP pass an empty `account_id` instead. A
mid-turn (working) chat is rejected unless you force it — `--force` on the CLI,
`force: true` through MCP.

### Switch from inside a chat

You can switch a running chat's account from the chat composer itself by
submitting a **`/boss switch`** (or **`/switch`**) control command:

```
/boss switch <account>
/switch <account>            # short form
/boss switch <account> --force   # interrupt a mid-turn (WORKING) chat
```

`<account>` is an optional account id or label; omit it to let the daemon pick
another eligible account. This is the **credit-free** in-chat switch: the daemon
intercepts the submitted command **before it reaches the agent pane** and runs
the account-switch primitive directly, returning the result as a notice. Because
no LLM call is made, **it works even when that chat is credit-exhausted** — which
is exactly when you need it.

By contrast, **asking the assistant to switch its own account does not work once
the chat is exhausted.** The in-chat `/boss` skill only gives the agent the
`boss account switch` CLI reference; for the agent to act on it, it must emit a
tool call — an LLM call on the very account that is already capped. Use the
`/boss switch` control command instead (or switch from outside the chat).

Two caveats worth knowing:

- The interception only guards the RPC send path (the web/TUI composer submit).
  **Raw keystrokes typed directly into the tmux pane over SSH bypass it** and
  still reach the agent, so a `/boss switch` typed straight into an exhausted
  pane hits a 401 rather than switching.
- You can also switch from **outside** the chat at any time: the TUI chat picker
  / session-detail view (press **`c`**, "swit[c]h account"), or the CLI
  `boss account switch <session> <account>`.

## Rotation behavior

Once you have registered extra accounts, automatic rotation is **on by default**
— registering additional accounts is itself the opt-in. When a session's agent
hits a usage cap, the daemon:

1. **Detects the usage limit** from the provider's limit banner (scraped
   read-only from the interactive tmux pane) or from the exit-log tail of a
   headless run — never inferred from ordinary output.
2. **Puts the exhausted account on cooldown** until the reset time parsed from
   the limit message. When the message carries no parseable reset time, a
   conservative default cooldown is applied instead.
3. **Selects the next eligible account** for the same provider (an active
   account that is not itself cooling), preferring lower priority values.
4. **Respawns and resumes** the interrupted session under the new account and
   posts an **in-chat notice** recording the switch.

**Your interrupted prompt is never automatically re-sent.** Rotation restores
the session on a fresh account and resumes the conversation, but if a turn was
cut off mid-flight you decide whether to re-issue it — Bossanova will not replay
it for you.

Automatic rotation of interactive chats can be scoped **per repository**: a
repo-level override in the TUI repo settings (Automations section) can turn
auto-rotation off for one repository while leaving it on globally, or vice
versa. Above every per-repo setting sits the global kill-switch.

## Kill-switch

Setting `managed_accounts.enabled=false` is the global kill-switch. It halts
**all** automatic rotation instantly — the daemon re-reads the flag on every
rotation decision, so no restart is needed:

Halt automatic rotation:

<CommandTabs
chat='"turn off automatic account rotation"'
cli="boss settings --no-managed-accounts"
/>

Re-enable it:

<CommandTabs
chat='"turn on automatic account rotation"'
cli="boss settings --managed-accounts"
/>

(`--no-rotation` / `--rotation` are deprecated hidden aliases for the same
two flags, kept for back-compat scripts.)

There is no MCP equivalent for the kill-switch. The `update_settings` tool
covers the worktree base directory, poll interval, default agent, tracing, and
per-agent config — it carries no managed-accounts field — so the Chat, CLI, and
TUI paths above are the ways to flip it.

You can also flip the same toggle from the TUI Settings view: it is the
**"Enable automatic account rotation"** checkbox, toggled with `enter`/`space`.
`boss settings` with no flags prints all current settings; the two
rotation-relevant lines are:

```
Managed accounts: true|false
Failover proxy:   true|false
```

Turning off managed accounts also turns off the local failover proxy (see
[Privacy: Local failover proxy](../reference/privacy.md#local-failover-proxy-on-by-default)),
since the proxy depends on account management being enabled. To keep
rotation on but turn off only the proxy, set
`managed_accounts.failover_proxy_enabled=false` instead.

The kill-switch only disables **automatic** rotation. Manual
`boss account switch` keeps working while rotation is off — you remain in full
control of which account a session runs under.

## When every account is limited

If every account for a provider is cooling at once, there is nowhere to rotate.
The session **parks** rather than failing: it shows an
**"all accounts limited until ~T"** badge, where T is the earliest cooldown
expiry across your accounts. The session resumes automatically at that earliest
reset, and you get **one notification per episode** — Bossanova will not spam you
on every poll while the accounts remain capped.

## Credential injection failures

When a session is bound to a managed account, Bossanova materializes that
account's credentials and injects them into the agent process. If that
injection **fails**, the agent still starts — but it starts on the CLI's own
ambient login (`~/.codex`, `~/.claude`) instead of the account you bound. The
session keeps working, which is exactly what makes the failure easy to miss:
the usage lands on whichever account happens to be logged in locally, and the
bound account's usage stays at zero.

Bossanova records that downgrade on the account itself. An account whose
credentials could not be injected shows:

- **HEALTH `failed`** in the TUI Accounts list, on the account detail screen,
  and in `boss account ls`;
- a **LAST TEST** reason beginning `credential injection failed:`, which is what
  distinguishes it from a rejected credential;
- the provider's **"no eligible account"** hint in `boss account ls`, because
  rotation cannot select a `failed`-health account.

The daemon log carries the matching line at `ERROR`, naming the account id and
provider.

**What to do — often nothing.** The reason string names the entry that could
not be projected, most often a file the agent itself wrote into the managed
account home. The record is withdrawn automatically on the next spawn that
materializes successfully: health returns to `ok` and the reason is cleared.
Only injection failures clear this way — a genuine account-test failure or a
confirmed suspension is left untouched, so a real credential problem never
disappears behind a successful spawn.

That automatic path needs a next spawn on **that** account, which a
`failed`-health account will not normally get: rotation does not select one. It
is reached when the account is the provider's only active one, or when a chat
is already bound to it. With several healthy accounts for the provider, expect
to clear it by hand instead.

**Do not reach for `boss account test` first.** The prefixed reason is the only
thing that marks the failure as self-clearing, and `boss account test` records
its own outcome into that same field. A _passing_ test therefore overwrites the
prefix while leaving `health = failed`, and nothing clears that health
automatically afterwards — the account stays out of rotation with a blank
reason. Run the test when you suspect the credential itself; for a credential
**injection** failure, let the next spawn clear it.

**To clear it by hand**, re-save the credential — that is what restores health
directly. `boss account refresh` always writes a credential, so it needs one of
`--token` or `--credential-file` (`--credential-file -` reads stdin); it will
not run without one:

<CommandTabs
chat='"refresh account <account-id>"'
cli="boss account refresh <account-id> --credential-file -"
/>

The refresh restores `health = ok`; the `credential injection failed:` reason
stays on the row as the last recorded LAST TEST result until the next
`boss account test` overwrites it.

## Audit trail

Every rotation decision is recorded as a rotation event — including the ones
where nothing swapped (for example, a limit detected while the kill-switch is
off, or with no eligible account to move to). Each event captures the session,
provider, trigger, the account moved from and to, the reset time, and the
outcome.

Rotation history is visible in **session detail** in both the TUI and the web
UI, and every decision also emits a structured line to the daemon log. Events
record account **labels only** — never credentials.

## Terms-of-service note

Multi-account rotation is a **personal-use, opt-in** feature. Whether running
more than one subscription account — and rotating between them — is acceptable
under your Claude or Codex provider's terms of service is **your
responsibility**. Bossanova does not register or import any account you have not
explicitly added, and it **never shares accounts between users**: every
registered credential stays in your own OS keyring on your own machine.
