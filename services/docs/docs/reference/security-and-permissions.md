---
title: Security and Permissions
description: What a Bossanova agent session can touch on your machine, where approvals come in, the dangerously_skip_permissions toggle, and the three independent kill switches.
---

# Security and Permissions

This page is the trust model. It documents what
an agent running under Bossanova can do on your machine, where you get
asked to approve, what the toggles do, and how to disconnect entirely.
If a section here disagrees with the source code, the source code is
authoritative; please file a bug.

## The trust model

**The agent runs as the user that runs `bossd`.** It has the same
filesystem, network, and keychain access as that user.

There is no sandbox. No seccomp profile. No filesystem container. No
network egress filter. The `claude` or `codex` plugin
spawns its agent CLI as a normal child process of `bossd`. The agent
inherits `bossd`'s environment, working directory permissions, and OS
credentials. If you can `rm -rf ~/some-dir` from your shell, so can the
agent.

This is intentional. The agent needs to run `git`, `gh`, your
language toolchain, and your test suite, all of which require real
user-level access to be useful. The trade-off is that **you are
trusting the agent the way you'd trust a human colleague with shell
access to your machine.**

If you want a hardened version of this story (containers, ephemeral
VMs, scoped credentials) it is not in the box today. The pieces below
are the levers that exist.

## Per-tool permissions (Claude Code)

Out of the box, Claude Code prompts before each non-trivial tool call.
`Bash`, `Edit`, `Write`, etc. That prompt is the primary safety
boundary inside a session. Bossanova does not modify or suppress those
prompts; whatever Claude Code's defaults are, that's what you get. See
the [Claude Code permissions
docs](https://docs.anthropic.com/en/docs/claude-code/permissions) for the full
list of tool gates and how to configure them in
`~/.claude/settings.json`.

Bossanova's only contribution to per-tool permissions is the
`dangerously_skip_permissions` toggle below, i.e. the single switch
that turns the prompts off.

## The `dangerously_skip_permissions` toggle

`--dangerously-skip-permissions` is the Claude Code flag that disables
the per-tool prompts. With it on, the agent runs every tool call
without asking. It's the right setting for autonomous PR-driving
sessions where you've decided you trust the agent end-to-end. It's the
wrong setting for ad-hoc local experimentation.

Bossanova exposes the toggle in two places, both of which write to
`plugins[claude].config.dangerously_skip_permissions` in
[`settings.json`](./settings.md#claude-plugin-config-keys):

- **Terminal UI (TUI):** the _Settings_ view has a _Skip permissions_ checkbox.
- **CLI:** `boss settings --skip-permissions` /
  `boss settings --no-skip-permissions`.

The toggle is honoured by both the daemon-side tmux paths
(interactive TUI sessions, cron-spawned sessions) and the gRPC
plugin path: bossd projects the `Plugins[claude].Config` map into
the plugin subprocess as `BOSS_PLUGIN_*` environment variables, and
the `claude` plugin reads
`BOSS_PLUGIN_dangerously_skip_permissions` at startup.

## The WorkOS auth boundary

`boss login` runs the WorkOS device-code flow and stores three tokens
locally via `keyringutil`:

- The **WorkOS access token** (a JWT). Sent up the bidi stream as
  `Authorization: Bearer …`. Boss Cloud re-verifies it against WorkOS
  JWKS on every connect.
- The **WorkOS refresh token**. Stays on disk; never crosses the
  stream.
- The **daemon session token** issued at registration. Sent as
  `X-Daemon-Token` so Boss Cloud can cross-check that the JWT's user
  owns the daemon identified by the session token.

Storage backend selection: macOS Keychain on macOS, libsecret on
Linux, Wincred on Windows. `BOSS_KEYRING_BACKEND=file` forces the
encrypted file backend everywhere; in containers without a system
keychain that fallback is automatic. The file backend's passphrase is
a per-install random value at `~/.config/bossanova/keyring.key` mode
0600, not a hardcoded constant.

Scope of the access token: the WorkOS client is configured for plain
identity, so the JWT carries the WorkOS user id, email, and any
organization membership WorkOS exposes. The token authenticates you
to **Boss Cloud only**. It is not a GitHub token, a Linear token, or
an Anthropic API key. Those credentials live wherever you put them
(typically `gh`'s own keychain entry, environment variables, or the
per-repo Linear API key stored in bossd's DB).

`boss logout` clears all three locally.
`BOSSD_ORCHESTRATOR_URL=""` is the kill switch for using them at
all (see below).

## Pull the plug: three independent kills

These three are independent. You can use any combination.

### 1. `BOSSD_ORCHESTRATOR_URL=""`

Setting `BOSSD_ORCHESTRATOR_URL` to an explicit empty string in the
environment that launches `bossd` puts the daemon in **local-only
mode**. No registration call. No bidi stream. No terminal attach. The
TUI and plugins keep working over the local Unix socket. The web app
stops seeing this daemon's sessions. Reverse it by unsetting the
variable (or setting it to a URL) and restarting bossd.

This is the single biggest kill switch on the cloud side. Use it if
you want the local agentic flow without anything leaving the machine.

### 2. Per-repo `can_auto_*` flags

Repo-level automation flags (`boss repo show <name>`) gate the
daemon's autonomous PR actions per repo. Setting all four to `false`
means the live gates (`CanAutoMerge`, `CanAutoMergeDependabot`) take
no autonomous action.

:::warning Two of the four flags are dead code today

`CanAutoAddressReviews` and `CanAutoResolveConflicts` do not currently
gate the repair plugin. Repair runs unconditionally on every
Failing/Conflict/Rejected PR. To stop repair specifically, you must
pause / cancel the workflow, close the PR, or unload the plugin
binary entirely. See
[Plugins → Automation](../plugins.md#automation) for the kill
switches that work today.

:::

### 3. Stop the repair plugin process

`bossd-plugin-repair` is a separate process loaded by the daemon at
startup. Killing the running process is **not enough**. Bossd's
plugin manager will restart it. To actually stop repair you need to
either remove the binary from the discovery path or take it out of the
plugins list in `settings.json` and restart the daemon
(set `enabled: false` for the `repair` entry in `settings.json`).

The same pattern applies to any other plugin: `bossd-plugin-dependabot`,
`bossd-plugin-linear`, future agent runners. They live as autonomous
processes, supervised by bossd. Removing the binary is the durable kill.

## Setup scripts run as you

Per-repo setup scripts (configured in
your repo's setup script field (`boss repo show <name>`)) are executed as part of the
session bootstrap whenever a new worktree is created. They run with
your full shell environment: same `$PATH`, same `$HOME`, same
keychain agent, same SSH config. The cross-link is
[guides → Setup Scripts](../guides/setup-scripts.md).

The trust model: **a setup script is the same as something you'd
paste into your terminal yourself.** If a teammate gives you a
setup-script snippet, treat it the way you'd treat any pasted shell
command from a teammate. The same applies if a plugin or workflow
suggests a setup-script edit. Review it before saving.

There is no allowlist for what setup scripts can do. They can
`rm -rf`, they can write files outside the worktree, they can write
keychain entries, they can call `gh`, they can hit the network. None
of this is gated.

## Worktrees and credentials

Each session runs in a [git worktree](../concepts/worktrees.md) under
`~/.bossanova/worktrees/<repo>/<session>/`. The agent operates in that
directory and uses **your** git/`gh` credentials to push, comment, and
merge. Specifically:

- `git push` uses whatever credential helper your global git config
  points at. If `gh auth login` configured `gh auth git-credential`,
  pushes go via that token.
- `gh pr create` / `gh pr merge` etc. run with the `gh` token's full
  scope. Bossanova does not narrow scope per session.
- If a worktree has access to a private deploy key (e.g. `~/.ssh/id_ed25519`
  reachable from `ssh-agent`), the agent does too.

There is **no allowlist** for repos the agent can push to. If your
`gh` token has write access to twenty private repos, an agent in any
of those repos has write access to all twenty (via the token, which
the agent could in principle use directly). The mitigations are: (1)
trust the agent, (2) scope the token, (3) use `--dangerously-skip-permissions`
off so each `gh` invocation prompts.

## Plugins are trusted code

Plugins are separate processes spawned by `bossd` from a fixed
discovery path. The discovery is `DiscoverPlugins` in
[`lib/bossalib/config/config.go`](https://github.com/bossanova-dev/bossanova/blob/main/lib/bossalib/config/config.go),
which scans (in order):

1. `<bin-dir>/../libexec/plugins/`: the Homebrew layout.
2. `<bin-dir>/` itself: the `make build` development layout.

Any binary named `bossd-plugin-*` in those directories is loaded.
There is no signing check, no checksum, no per-plugin permission
declaration. **A malicious plugin can do anything `bossd` can do**.
read the SQLite DB, read the keychain, push to git, hit the network,
delete worktrees. Trust the plugins you install.

Bundled plugins (claude, dependabot, linear, repair) ship in the
release tarball and are reviewed in the same repo as the daemon. If
you side-load a plugin from elsewhere, you've extended the trust
boundary to its author.

The plugin lifecycle is supervised. If a plugin process dies, the
daemon restarts it. The way to permanently disable a plugin is to
remove its binary or take it out of the
`plugins` array in `settings.json`.

## What's not in scope today

Calling these out so you don't have to dig:

- **No mandatory access controls.** No SELinux profile, no AppArmor
  template, no macOS sandbox profile shipped with the daemon.
- **No agent network egress filter.** If you want one, run the daemon
  inside a network namespace or behind a per-process firewall yourself.
- **No "approve once, remember" policy file.** Claude Code's
  permissions are session-scoped; Bossanova does not maintain a
  cross-session policy database.
- **No per-session credential isolation.** Every session sees the same
  `gh` token, the same SSH agent, the same keychain.

If any of this matters for your environment, the supported pattern is
to run `bossd` as a dedicated OS user with narrowed credentials and
take advantage of standard OS-level controls. There is no special
Bossanova machinery to help with that today.

## See also

- [Privacy](./privacy.md): what bossd actually sends to Boss Cloud,
  and the opt-out path.
- [Setup scripts](../guides/setup-scripts.md): the trust model in
  practice.
