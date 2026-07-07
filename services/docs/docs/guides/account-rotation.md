---
title: Account Rotation
description: 'Register multiple provider accounts and let Bossanova rotate them automatically when a usage cap is hit.'
---

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

```bash
# Claude: runs the setup-token walkthrough
boss account add claude

# Codex: runs the interactive device flow
boss account add codex
```

Useful flags (see `boss account add --help`):

- `--label` — a human label for the account (unique per provider).
- `--email` — an informational account email.
- `--priority` — sort order; lower is preferred.
- `--token-stdin` — **claude only**: read the setup token from stdin instead of
  running the walkthrough. Codex has no stdin path (its device flow needs an
  interactive browser round-trip).

Manage the registry with the sibling commands:

```bash
boss account ls                     # list accounts (add --json for scripts)
boss account test <account-id>      # validate a credential and record the result
boss account update <account-id>    # change label, email, priority, or status
boss account remove <account-id>    # remove an account and its stored credential
```

You can always move a specific session onto a specific account by hand:

```bash
boss account switch <session> <account>
```

Manual switching stops the session's live chat, rebinds it to the chosen
account, and respawns with resume. Pass `system-default` (or `0`) as the account
to return a session to account 0. A mid-turn (working) chat is rejected unless
you add `--force`.

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

Setting `rotation.enabled=false` is the global kill-switch. It halts **all**
automatic rotation instantly — the daemon re-reads the flag on every rotation
decision, so no restart is needed:

```bash
boss settings --no-rotation   # halt automatic rotation
boss settings --rotation      # re-enable it
```

You can also flip the same toggle from the TUI Settings view. `boss settings`
with no flags prints the current value (`Rotation enabled: true|false`).

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

## Verifying your setup

After registering accounts, follow
[Verifying Account Rotation](./account-rotation-verification.md) for an
end-to-end local checklist and the optional smoke script.
