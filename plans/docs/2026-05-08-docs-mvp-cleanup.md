# Docs MVP Cleanup — Ship-Now Plan

**Status:** ready to execute
**Branch:** `create-a-documentation-site`
**Pre-cleanup snapshot:** commit `2f5144fc` (35 docs pages, ~33,600 words)
**Target:** 17 pages, ~13,500 words, all fact-checked, marketing-aligned, build green

## Why this plan exists

We bit off more than we could chew. The original spec
(`docs/superpowers/specs/2026-05-04-docs-site-design.md`) called for **10
pages** organized into Get Started → Guides → Configuration → Help. The
current site has **35 pages** spanning a deep TUI reference, per-plugin
reference pages, four concept pages, and six task guides — most of which
were not in the original spec and have not been reviewed against the
behavior of `boss`/`bossd` HEAD.

`services/marketing` has shipped since the docs site was scoped. It
locks in the public selling proposition:

> "Bossanova — Ship the PRs your AI agents open. PR-first workflow with
> auto-repair, multi-machine web UI, and on-demand cloud sandboxes for
> Claude Code, OpenCode, and Codex sessions."

Marketing pillars (homepage section order):

1. **PR-first** — monitor PRs, auto-respond to issues, work concurrently while you sleep.
2. **Multi-machine** — sign in to control remote Claude sessions; code never leaves your boxes.
3. **Scheduler** — recurring agent runs that open PRs.
4. **Native agent** — Claude Code in tmux, locally or streamed.
5. **Pluggable agents** — Claude today, Codex/OpenCode on the roadmap.

The MVP docs site must cover these pillars and nothing else that we
can't confidently fact-check before shipping. Everything beyond that
moves into `docs/plans/2026-05-08-docs-future-extensions.md`.

## Scope summary

```
                              CURRENT       MVP        DEFERRED
Get Started                     3            3            0
Concepts                        4            3            3   (worktrees + how-it-works promoted from top + plugins promoted from top)
Guides                          6            4            3   (+pr-lifecycle new, -agent-runners, -issue-to-pr, -review-and-merge merged into pr-lifecycle)
Reference                      14            4           10   (10 = 6 TUI + 4 plugin deep-dives + agent-behavior + skills − env-vars merged)
Help                            3            3            0
                              ─────         ───          ───
Total .md files                 35           17           18
```

Reconciliation against the actual filesystem at SHA `2f5144fc`:

```
$ find services/docs/docs -name '*.md' | wc -l
       35
```

Breakdown of the 35 source files: 5 top-level (`intro`, `install`,
`quick-start`, `how-it-works`, `plugins`), 4 concepts, 6 guides, 17
reference (`agent-behavior`, `cli-reference`, `environment-variables`,
`privacy`, `security-and-permissions`, `settings`, `skills`, 6 tui/, 4
plugins/), 3 help. 5 + 4 + 6 + 17 + 3 = 35.

## Final MVP page set (17 pages)

The kept pages are chosen because they (a) map directly to a marketing
pillar, (b) are short enough to fact-check in one pass, or (c) are
trust-signal pages (privacy, security) that a public docs site cannot
ship without.

| # | Page | Words | Edit | Marketing pillar | Why kept |
|---|------|-------|------|------------------|----------|
| 1 | `intro.md` | 238 | light | all | landing page; rewrite "Where to next" to repoint to MVP pages |
| 2 | `install.md` | 218 | none | — | install commands, easy to verify |
| 3 | `quick-start.md` | 994 | **heavy** | all | first-run walkthrough; strip 13 cross-refs to deferred pages and rewrite the line-9 lead-in |
| 4 | `how-it-works.md` | 311 | light | all | restore as canonical "how Bossanova works"; remove "moved" banner; **promoted into Concepts** |
| 5 | `concepts/worktrees.md` | 818 | light | sandbox / native agent | foundational concept; strip refs to parallel-sessions/workflow |
| 6 | `plugins.md` | 328 | light | pluggable agents | overview of bundled plugins; remove "moved" banner; **promoted into Concepts** |
| 7 | `guides/setup-scripts.md` | 176 | none | — | referenced from quick-start; small and stable |
| 8 | `guides/pr-lifecycle.md` | **NEW (~250)** | new page | **PR-first / auto-repair** | slim "PR opens → CI runs → repair fires → merge → archive" walkthrough. Closes the gap left by deleting `review-and-merge.md`. PR-first is the #1 marketing pillar; not having a docs page on it is shipping-incoherent. |
| 9 | `guides/scheduled-sessions.md` | 1,331 | medium | **scheduler** | core marketing pillar; strip 5 cross-refs (4 in body + line 183) |
| 10 | `guides/web.md` | 522 | none | **multi-machine** | core marketing pillar |
| 11 | `reference/settings.md` | 689 | medium | — | canonical settings doc + **absorb env-vars table** from deleted `environment-variables.md` (8 vars) |
| 12 | `reference/cli-reference.md` | 1,224 | **replace with stub** | — | rewrite as ~150-word stub pointing at `boss --help` / `bossd --help` |
| 13 | `reference/privacy.md` | 1,606 | medium | trust signal | data-flow page; strip 2 refs to env-vars |
| 14 | `reference/security-and-permissions.md` | 1,757 | medium | trust signal | strip 8 refs (agent-behavior, repo-settings, plugins/repair) |
| 15 | `help/faq.md` | 1,307 | medium-heavy | — | strip 9 cross-refs **and** rewrite lines 11–12 ("source of truth") and line 40 ("full wiring diagram") |
| 16 | `help/troubleshooting.md` | 2,012 | medium-heavy | — | strip 7 cross-refs **and** shorten "Skills not installed" section (lines 152–163) once `skills.md` is gone |
| 17 | `help/uninstall.md` | 70 | none | — | trivially stable |

Total: ~13,500 words to fact-check, vs ~33,600 words in the current
build.

## Pages deferred (18 — see future-extensions plan)

| Section | Page | Reason for deferral |
|---------|------|---------------------|
| Concepts | `architecture.md` | duplicates `how-it-works.md`; pick one canonical "what's the system" page |
| Concepts | `parallel-sessions.md` | covered briefly inside `worktrees.md` and `how-it-works.md` |
| Concepts | `workflow.md` | covered by `quick-start.md` + `how-it-works.md` |
| Guides | `agent-runners.md` | only `claude` ships today; comparison page premature. **Day-one users may hit "no AgentRunner plugin loaded"** — covered by a 5-line callout in `install.md`'s "Verify your install" section (added in Step 6). |
| Guides | `issue-to-pr.md` | task-oriented walkthrough; nice-to-have, not pillar-aligned |
| Guides | `review-and-merge.md` | the *content* is largely absorbed into the new slim `pr-lifecycle.md`. The verbose original page does not ship; the slim page replaces it. |
| Reference | `agent-behavior.md` | deep behavioral contract page; high fact-check cost |
| Reference | `skills.md` | niche; user does not need to know skill names to use Bossanova |
| Reference | `environment-variables.md` | **content merged** into `settings.md` as a new "Environment overrides" section (Step 4 below). The standalone page is then deleted. |
| Reference / TUI | `home.md`, `keybindings.md`, `repo-settings.md`, `repos.md`, `settings.md`, `trash.md` | exhaustive TUI reference; each page is hundreds of lines describing one screen. Out-of-date risk is high. Replace with `boss --help` / `?` in-app help on first ship. |
| Reference / Plugins | `claude.md`, `dependabot.md`, `linear.md`, `repair.md` | per-plugin deep dives; behavior is changing too fast for a docs page to track. `plugins.md` overview suffices. |

The future-extensions plan (`docs/plans/2026-05-08-docs-future-extensions.md`)
records the source SHA for each deferred page so content can be
restored verbatim when we have the bandwidth to verify it.

## Sidebar after cleanup

`services/docs/sidebars.ts` becomes:

```
Get Started
  ├─ Introduction              (intro)
  ├─ Installation              (install)
  └─ Quick Start               (quick-start)

Concepts
  ├─ How It Works              (how-it-works)         [moved out of "Guides" — it's conceptual, not task-oriented]
  ├─ Worktrees                 (concepts/worktrees)
  └─ Plugins                   (plugins)              [moved out of top level — overview/concept material]

Guides
  ├─ Setup Scripts             (guides/setup-scripts)
  ├─ PR Lifecycle              (guides/pr-lifecycle)  [NEW]
  ├─ Scheduled Sessions        (guides/scheduled-sessions)
  └─ Multi-Machine (Web)       (guides/web)

Configuration
  ├─ Settings                  (reference/settings)
  └─ CLI Reference             (reference/cli-reference)

Security & Privacy
  ├─ Privacy                   (reference/privacy)
  └─ Security & Permissions    (reference/security-and-permissions)

Help
  ├─ FAQ                       (help/faq)
  ├─ Troubleshooting           (help/troubleshooting)
  └─ Uninstall                 (help/uninstall)
```

URLs do not change for `how-it-works` or `plugins` — both files stay at
`services/docs/docs/<file>.md`, only their sidebar grouping moves.
Docusaurus URL routing follows file location, not sidebar position.

The `link: { type: "generated-index", description: ... }` blocks are
removed from every section. Generated indexes work, but at v1 they
duplicate the section's first page. Reintroduce them per-section once
that section grows past four pages and an index page actually adds
information beyond the sidebar.

## Execution checklist

Each step ends with a single commit. Run `make -C services/docs build`
between steps to keep the broken-link blast radius small.

### Step 1 — Delete deferred page files

```bash
git rm \
  services/docs/docs/concepts/architecture.md \
  services/docs/docs/concepts/parallel-sessions.md \
  services/docs/docs/concepts/workflow.md \
  services/docs/docs/guides/agent-runners.md \
  services/docs/docs/guides/issue-to-pr.md \
  services/docs/docs/guides/review-and-merge.md \
  services/docs/docs/reference/agent-behavior.md \
  services/docs/docs/reference/skills.md
git rm -r \
  services/docs/docs/reference/tui \
  services/docs/docs/reference/plugins
```

`reference/environment-variables.md` is **not** deleted yet — it gets
deleted in Step 4 after its content lands in `settings.md`.

The future-extensions plan records the pre-deletion commit SHA. To
restore any page later: `git show <sha>:services/docs/docs/<path>`.

Commit:

```
chore(docs): [#225] remove deferred pages from MVP docs site

Move per-TUI-screen reference, per-plugin deep dives, and three concept
pages out of the MVP. See docs/plans/2026-05-08-docs-future-extensions.md
for the catalog and re-introduction plan.
```

### Step 2 — Replace the redirect banners

Two pages currently start with a "this page has moved" banner pointing
at deferred targets. Strip the banner; the rest of each file is the
canonical content.

**`services/docs/docs/how-it-works.md`** — delete lines 4–9:

```diff
 ---
 title: How It Works
-description: Legacy overview page — content has moved into the Concepts and Reference sections.
+description: How Bossanova orchestrates worktrees, the daemon, and plugins.
 ---

 # How It Works

-> This page has moved. See [Concepts → Workflow](./concepts/workflow.md) and the
-> [Plugins reference](./reference/plugins/claude.md) for the up-to-date material.
-
 Bossanova uses git worktrees to isolate each agent session in its own
```

**`services/docs/docs/plugins.md`** — delete lines 4–11:

```diff
 ---
 title: Plugins
-description: Legacy plugins overview — content has moved into the per-plugin Reference pages.
+description: The bundled bossd plugins and how they're loaded.
 ---

 # Plugins

-> This page has moved. See the per-plugin
-> [Reference → Plugins](./reference/plugins/claude.md) pages for
-> up-to-date details on the bundled plugins.
-
 `bossd` is extended via out-of-process **plugin binaries** named
```

Commit:

```
docs(site): [#225] restore how-it-works and plugins as canonical pages
```

### Step 3 — Fix cross-references in kept pages

The build runs with `onBrokenLinks: 'throw'`, so after Step 1 the build
fails until every link to a deleted page is resolved. The fix is one of
three patterns:

| Pattern | When to use | Example |
|---------|-------------|---------|
| **Repoint** | A kept page covers the same ground | `./concepts/workflow.md` → `./how-it-works.md` |
| **Inline** | The link is a one-noun reference; replace with prose | `[Repos](./reference/tui/repos.md)` → "the Repos screen" |
| **Strip** | The link adds nothing the user needs at v1 | "see [keybindings](./reference/tui/keybindings.md)" → delete the parenthetical |

Concrete edits per file (verified with `grep -n` against the
2f5144fc snapshot):

#### `intro.md` (1 fix)

| Line | Before | After |
|------|--------|-------|
| 30 | `[How It Works](./concepts/workflow.md)` | `[How It Works](./how-it-works.md)` |

#### `quick-start.md` (13 link fixes + 1 prose rewrite)

| Line | Before | After |
|------|--------|-------|
| 9 (prose) | `cross-links point at the full reference for each` | `cross-links point at the relevant settings or guide page where one exists` (the deep "full reference" pages no longer exist; the new lead-in must not promise them) |
| 26 | `[preflight check](./concepts/architecture.md)` | strip link, keep "preflight check" prose |
| 31 | `[Agent runners](./guides/agent-runners.md) if you hit that` | "If your `claude` CLI isn't on PATH, install Claude Code from `claude.ai/download` first." |
| 58 | `[Repos](./reference/tui/repos.md)` | "the Repos screen (`r` from Home)" |
| 69 | `[Setup scripts](./guides/setup-scripts.md)` | keep — guide is kept |
| 74 | `[Repo settings](./reference/tui/repo-settings.md)` | "the Repo Settings screen (run `boss repo show <name>` for the current values)" |
| 76 | `[Repo settings](./reference/tui/repo-settings.md)` | strip; replace with "Run `boss repo update <name> --help` for the full field list." |
| 105 | `[skills](./reference/skills.md)` | "skills (small markdown helper files Bossanova installs alongside the agent)" — drop link |
| 111 | `[keybindings](./reference/tui/keybindings.md#attach-claude-code-session)` | "press `?` from the chat pane for the keymap" |
| 113 | `[Parallel sessions](./concepts/parallel-sessions.md)` | repoint to `./concepts/worktrees.md#multiple-sessions` (add anchor section there in Step 4) |
| 138 | `[Home](./reference/tui/home.md#polling)` | strip parenthetical entirely; the sentence still parses |
| 141 | `[repair plugin](./reference/plugins/repair.md)` | repoint to `./plugins.md#automation` (anchor exists once `plugins.md` lists automation plugins under that heading) |
| 144 | `[Review and Merge](./guides/review-and-merge.md)` | repoint to `./guides/pr-lifecycle.md` (created in Step 5) |
| 158 | `[Trash](./reference/tui/trash.md)` | "the Trash view (`t` from Home)" |
| 165 | `[How It Works](./concepts/workflow.md)` | `[How It Works](./how-it-works.md)` |

#### `concepts/worktrees.md` (5 fixes)

| Line | Before | After |
|------|--------|-------|
| 90 | `[Setup Scripts](../guides/setup-scripts.md)` | keep |
| 104 | `[Parallel sessions](./parallel-sessions.md)` | strip — covered by adjacent prose |
| 119 | `[The session workflow](./workflow.md)` | repoint to `../how-it-works.md` |
| 121 | `[Parallel sessions](./parallel-sessions.md)` | strip |
| 123,125 | `[Setup Scripts]`, `[Settings reference]` | keep |

Add an anchor section near the bottom of `concepts/worktrees.md`:

```markdown
## Multiple sessions

Each session gets its own worktree. Two sessions on the same repo run
in two separate directories with independent indexes — neither blocks
the other. See [Scheduled Sessions](../guides/scheduled-sessions.md)
for how the scheduler uses this.
```

This is the target for `quick-start.md:113`.

#### `guides/scheduled-sessions.md` (5 fixes — codex caught the missed line 183)

| Line | Before | After |
|------|--------|-------|
| 19 | `[Keybindings](../reference/tui/keybindings.md)` | "press `?` from any screen for the keymap" |
| 136 | `[Concepts → Parallel sessions](../concepts/parallel-sessions.md)` | repoint to `../concepts/worktrees.md#multiple-sessions` |
| 183 | `[Review and Merge](review-and-merge.md)` | repoint to `./pr-lifecycle.md` (created in Step 5) |
| 187 | `[Parallel sessions](../concepts/parallel-sessions.md)` | repoint as above |
| 192 | `[Repo Settings](../reference/tui/repo-settings.md)` | "your repo's settings (`boss repo show <name>`)" |

#### `reference/privacy.md` (2 fixes — both env-vars refs)

| Line | Before | After |
|------|--------|-------|
| 215 | `[Security and Permissions]` | keep |
| 218 | `[Environment Variables](./environment-variables.md)` | repoint to `./settings.md#environment-overrides` (anchor created in Step 4) |
| 221 | `[Web App](../guides/web.md)` | keep |

#### `reference/security-and-permissions.md` (8 fixes)

| Line | Before | After |
|------|--------|-------|
| 85 | `[Agent behavior → the toggle](./agent-behavior.md...)` | repoint to `./settings.md#claude-plugin-config-keys` |
| 141 | `[automation flags](./tui/repo-settings.md)` | "your repo's automation flags (`boss repo show <name>`)" |
| 153 | `[Repair plugin → Gating](./plugins/repair.md#gating)` | repoint to `../plugins.md#automation` |
| 165 | `([plugins/repair.md → Disabling](./plugins/repair.md))` | replace with prose: "(set `enabled: false` for the `repair` entry in `settings.json`)" |
| 174 | `[Repo Settings](./tui/repo-settings.md)` | "your repo's setup script field (`boss repo show <name>`)" |
| 178 | `[guides → Setup Scripts](../guides/setup-scripts.md)` | keep |
| 193 | `[git worktree](../concepts/worktrees.md)` | keep |
| 260+ | "See also" bullets pointing at `./agent-behavior.md`, `./plugins/repair.md`, `./environment-variables.md` | drop those three bullets |

#### `help/faq.md` (9 link fixes + 2 prose rewrites)

Codex was right that the link table alone leaves stale prose claims.
Two paragraphs need to be rewritten outright:

**Lines 11–12 (the lead paragraph)** — currently:

> "the [Concepts](../concepts/architecture.md) and [Reference](../reference/settings.md) sections are the source of truth for behavior."

Rewrite to:

> "[How It Works](../how-it-works.md) covers the system, and [Settings](../reference/settings.md) covers configuration."

**Line 40 (the architecture overview claim)** — currently:

> "See the [architecture overview](../concepts/architecture.md) for the full wiring diagram."

`how-it-works.md` does not currently contain a wiring diagram. Either
(a) drop the sentence entirely, or (b) drop "wiring diagram" and link
to the kept page:

> "See [How It Works](../how-it-works.md) for the components and the worktree lifecycle."

Use option (b).

Link table:

| Line | Before | After |
|------|--------|-------|
| 11 | `[Concepts](../concepts/architecture.md)` | (replaced by the prose rewrite above) |
| 40 | `[architecture overview](../concepts/architecture.md)` | (replaced by the prose rewrite above) |
| 48 | `[claude plugin reference](../reference/plugins/claude.md)` | `[Plugins](../plugins.md)` |
| 63 | `[Repo Settings](../reference/tui/repo-settings.md)` | "the Repo Settings screen (`boss repo show <name>`)" |
| 68 | `[repair plugin](../reference/plugins/repair.md)` | `[plugins overview](../plugins.md#automation)` |
| 91 | `[Agent Runners](../guides/agent-runners.md)` | strip — replace with one-line "Bossanova ships the `claude` runner today; install Claude Code first." |
| 152 | `[`claude` plugin](../reference/plugins/claude.md)` | `[Plugins](../plugins.md)` |
| 158 | `[Skills](../reference/skills.md)` | strip parenthetical |
| 184 | `[agent runners guide](../guides/agent-runners.md)` | strip parenthetical |

(Lines 105, 110, 117, 177 already point at kept pages — no change.)

#### `help/troubleshooting.md` (7 link fixes + 1 section shortening)

The "Skills not installed in session" section at lines 152–163
references `skills.md`, which is being deleted. The section currently
ends with: *"The full skill model … is in [Skills](../reference/skills.md)."*

Rewrite that closing sentence to:

> "If neither check resolves it, file an issue with the daemon log — skill
> install is not user-configurable today."

This collapses six lines of content into the runbook's existing
"check / fix" pattern. The deep skill model docs come back when
`skills.md` is restored.

Link table:

| Line | Before | After |
|------|--------|-------|
| 13 | `[Architecture](../concepts/architecture.md)` | `[How It Works](../how-it-works.md)` |
| 133 | `[Agent Runners](../guides/agent-runners.md)` | "Install Claude Code from `claude.ai/download`." |
| 163 | `[Skills](../reference/skills.md)` | (replaced by section shortening above) |
| 179 | `[Permissions](../reference/security-and-permissions.md)` | keep |
| 237 | `[Repo Settings](../reference/tui/repo-settings.md)` | "your repo's automation flags (`boss repo show <name>`)" |
| 262 | `[settings](../reference/settings.md)` | keep |
| 329 | `[settings.json](../reference/settings.md)` | keep |
| 364 | `[Privacy](../reference/privacy.md)` | keep |

Commit:

```
docs(site): [#225] repoint cross-references and rewrite stale prose
```

### Step 4 — Merge env-vars table into `settings.md` and delete the source page

The current `reference/environment-variables.md` has eight env vars
(verified by reading the file at SHA `2f5144fc`):

```
BOSSD_ORCHESTRATOR_URL, BOSS_WORKOS_CLIENT_ID, BOSSD_DAEMON_ID,
BOSSD_LOG_LEVEL, BOSSD_DATA_DIR, BOSSD_PLUGINS_DIR,
BOSS_SETTINGS_FILE, BOSS_NO_COLOR
```

(Confirm count by `grep -E '^### `BOSSD?_|^### `BOSS_' services/docs/docs/reference/environment-variables.md`. If the count drifts, take whatever HEAD currently has — this plan does not freeze the table.)

Append a new section to `services/docs/docs/reference/settings.md`:

```markdown
## Environment overrides

Most settings can be overridden by environment variable. Precedence
(highest wins): CLI flag → environment variable → `settings.json` →
hardcoded default.

| Variable | Setting it overrides | Notes |
|----------|----------------------|-------|
| `BOSSD_ORCHESTRATOR_URL` | `cloud.orchestrator_url` | |
| `BOSS_WORKOS_CLIENT_ID` | `cloud.workos_client_id` | |
| `BOSSD_DAEMON_ID` | `cloud.daemon_id` | |
| `BOSSD_LOG_LEVEL` | n/a | `debug` / `info` / `warn` / `error` |
| `BOSSD_DATA_DIR` | n/a | overrides the daemon's local data directory |
| `BOSSD_PLUGINS_DIR` | plugin auto-discovery path | |
| `BOSS_SETTINGS_FILE` | n/a | absolute path to a settings file (overrides the platform default) |
| `BOSS_NO_COLOR` | n/a | disables ANSI color in `boss` output |
```

Take the **descriptions** from the source `environment-variables.md`
file at HEAD when actually editing — copy-pasting them here would
freeze content the merge step is supposed to copy live.

Then delete the source page:

```bash
git rm services/docs/docs/reference/environment-variables.md
```

Verify the build is still green:

```bash
make -C services/docs build
```

Commit:

```
docs(site): [#225] merge environment variables into settings page
```

### Step 5 — Create `guides/pr-lifecycle.md`

The PR-first / auto-repair pillar is the #1 marketing message
(`services/marketing/src/pages/index.astro:13` "Ship the PRs your AI
agents open"). Shipping a docs site without a PR-lifecycle page reads
as if we don't believe our own positioning.

Keep the page slim — under 300 words. Behavior across `repair`,
auto-merge, and review automation is in flux (`#245`, `#253`), so a
deep page would go stale fast.

Create `services/docs/docs/guides/pr-lifecycle.md`:

```markdown
---
title: PR Lifecycle
description: How a Bossanova session becomes a merged PR — what fires, what you decide, what to disable.
---

# The PR Lifecycle

A typical Bossanova session ends with a merged PR. Here's the loop and
where you sit inside it.

## Steps

1. **Session opens.** You start a session in `boss`. The daemon creates
   a worktree, runs your setup script, and spawns the agent.
2. **Agent works, PR opens.** The agent commits, pushes a branch, and
   opens a PR. The session's PR status appears in the Home view.
3. **CI runs.** Bossanova does not run CI — your usual GitHub Actions
   (or whatever) does.
4. **Repair fires (optional).** If CI fails or the PR has merge
   conflicts, the `repair` plugin spawns a follow-up session to fix
   them. Disable per-repo from the Repo Settings screen, or globally by
   setting `enabled: false` for the `repair` entry in
   [`settings.json`](../reference/settings.md).
5. **Review.** Reviewers comment as they normally would. (Automated
   review-comment response is on the roadmap, not in v1.)
6. **Merge.** You merge the PR. Bossanova does not auto-merge in v1.
7. **Archive.** Close the session in `boss`. The worktree is removed
   and the session row drops out of Home (visible later in Trash).

## Working multiple PRs concurrently

Each session is its own worktree, so there's no contention between
parallel sessions on the same repo. See
[Worktrees](../concepts/worktrees.md#multiple-sessions) for the model.

## Where to look when something stalls

- PR open but CI never starts → check your CI provider, not Bossanova.
- Repair fired but didn't fix the issue → see [Troubleshooting →
  Repair stuck](../help/troubleshooting.md#repair-stuck).
- Want to see what repair did → the session row in Trash links to the
  archived chat.
```

This page is intentionally conservative. Don't add claims that aren't
true on `boss`/`bossd` HEAD. If a step in the loop above is wrong
(e.g., we *do* auto-merge in some path), correct it during Step 7's
fact-check pass.

Commit:

```
docs(site): [#225] add slim PR lifecycle guide
```

### Step 6 — Replace `cli-reference.md` with a stub

The current `cli-reference.md` is 1,224 words enumerating every
subcommand and flag. Half its references go to deferred pages
(`agent-behavior.md`, `environment-variables.md`,
`tui/repo-settings.md`), and the page is at high risk of drifting from
`boss --help` whenever a flag changes. Replace it with a short stub.

Use **four-backtick outer fences** so the inner triple-backtick code
blocks render correctly. Plain content of the file (paste verbatim):

````markdown
---
sidebar_position: 2
title: CLI Reference
description: Pointers to the authoritative help text for boss and bossd.
---

# CLI Reference

The authoritative reference for every command, subcommand, and flag is
the help text built into the binaries:

```bash
boss --help
boss <subcommand> --help

bossd --help
bossd <subcommand> --help
```

## Top-level commands

### `boss`

The interactive terminal UI.

```bash
boss                          # launch the TUI on the Home screen
boss settings                  # open the settings screen
boss repo list                 # print configured repos
boss repo update <name> ...    # change repo fields from the shell
```

### `bossd`

The background daemon. Normally started by Homebrew's launchd plist or
your equivalent service manager. You rarely run it by hand.

```bash
bossd                          # run in foreground
bossd doctor                   # health check
```

## Settings overrides

Most settings can be overridden by environment variable or CLI flag.
Precedence (highest wins): CLI flag → environment variable →
`settings.json` → hardcoded default. See
[Settings → Environment overrides](./settings.md#environment-overrides)
for the table.

A hand-curated reference page is on the roadmap. If you hit something
ambiguous in the help text, open an issue at
[bossanova-dev/bossanova/issues](https://github.com/bossanova-dev/bossanova/issues).
````

(The four-backtick fences above are part of this plan's source. When
the agent or the user hand-pastes this content into
`services/docs/docs/reference/cli-reference.md`, the file should use
**triple backticks** as its actual code-block delimiters — not the
four-backtick wrapper.)

In `install.md`, append a small "Verify your install" section at the
bottom that absorbs the day-one "no AgentRunner plugin loaded" failure
mode (codex flagged this as the gap left by deleting `agent-runners.md`):

```markdown
## Verify your install

Run `bossd doctor`. It checks that `bossd` can find a working agent
runner plugin and reports any failures it sees.

If you see `no AgentRunner plugin loaded`, install Claude Code from
`claude.ai/download` (Bossanova ships the `claude` runner; it needs
the `claude` CLI on your `PATH`). Re-run `bossd doctor` and confirm a
green report before launching `boss`.
```

Commit:

```
docs(site): [#225] replace cli-reference with help-text pointer; add install verification step
```

### Step 7 — Update `sidebars.ts`

Replace `services/docs/sidebars.ts` with the structure shown in
**Sidebar after cleanup** above. Verify the build:

```bash
cd services/docs && pnpm build
```

Expected: `[SUCCESS]` and zero broken-link errors. If any link fires,
fix it on the source page (do not silence the build).

Commit:

```
docs(site): [#225] trim sidebar to MVP structure
```

### Step 8 — Fact-check pass on retained pages

Read each retained page in full. Verify against **the current branch
HEAD after the cleanup commits** (i.e. whatever `git rev-parse HEAD` is
at the time you run this step — not `2f5144fc`, which is just the
pre-cleanup snapshot). Specifically:

| Page | What to verify |
|------|----------------|
| `intro.md` | "What you get" bullets; the link list at "Where to next" |
| `install.md` | Homebrew tap name, build-from-source toolchain matches `make deps`; the new "Verify your install" section actually mirrors `bossd doctor` output |
| `quick-start.md` | Every `boss <command>` invocation actually works; screenshot filenames match `static/img/screenshots/`; the post-cut prose still flows; the line-9 lead-in matches the post-cut reality |
| `how-it-works.md` | Plugin list (claude, dependabot, linear, repair); roles of `boss`/`bossd` |
| `concepts/worktrees.md` | `worktree_base_dir` default; the lifecycle diagram matches what `bossd` does today; new "Multiple sessions" anchor reads correctly |
| `plugins.md` | Roadmap states for `codex`/`opencode`; loading mechanics; `make plugins` target |
| `guides/setup-scripts.md` | Env vars exposed to the script |
| `guides/pr-lifecycle.md` (new) | Every step in the loop matches actual behavior on HEAD; specifically that v1 does not auto-merge and review-response is not yet automated |
| `guides/scheduled-sessions.md` | Schedule format; cron plugin status; what "unassisted" runs do today |
| `guides/web.md` | Cloud sign-in flow; `daemon_id` field; what data leaves your box (cross-check with `privacy.md`) |
| `reference/settings.md` | Every field defaults against current `bossd` defaults; `cloud.*` block status (still approved-but-not-implemented?); the new Environment overrides section matches actual env-var handling |
| `reference/cli-reference.md` (new stub) | All listed subcommands exist on HEAD |
| `reference/privacy.md` | The data-flow inventory matches what bossd actually sends — this is a public trust commitment, get it right |
| `reference/security-and-permissions.md` | Default `dangerously_skip_permissions = false`; setup-script trust model |
| `help/faq.md` | All claims about behavior; the rewritten lines 11–12 and 40 read coherently; remove any FAQ rows referring to deferred features |
| `help/troubleshooting.md` | Each runbook actually resolves the symptom on HEAD; the shortened "Skills not installed" section still actionable |
| `help/uninstall.md` | Homebrew uninstall command; settings-file path |

For any claim that cannot be verified in 5 minutes, **delete the
claim** rather than leaving it. Wrong docs are worse than missing docs.

Commit per page (or per logical bundle):

```
docs(site): [#225] fact-check <page> against HEAD
```

### Step 9 — Final build, link check, deploy preview

```bash
cd services/docs
pnpm install
pnpm format
pnpm lint
pnpm build       # onBrokenLinks: 'throw' fails the run on any dead link
```

From the repo root:

```bash
make lint-docs
make test-docs
make build-docs
```

Manually open the Cloudflare Pages preview (auto-generated on the PR)
and click through every kept page. Verify search results return only
existing pages (the search index regenerates from the build output).

Commit (only if anything changed):

```
docs(site): [#225] final formatting and link sweep
```

### Step 10 — README sanity check

Confirm `README.md` still points at `docs.bossanova.dev` and does not
embed prose that contradicts the slimmer site. (Per `Task 9` of the
original implementation plan, the README is already a one-screen
pointer; just re-read it.)

## Out of scope (explicit "NOT in scope")

- **Real screenshots replacing placeholders.** All eight
  `quick-start-*.png` and three `tui-*.png` slots stay as
  deterministic-label placeholders. Capturing real screenshots is its
  own task (see `services/docs/SCREENSHOTS.md`).
- **Algolia DocSearch.** Local search via `@easyops-cn/docusaurus-search-local`
  stays. Migrating to Algolia is a future spec when traffic warrants.
- **Docs versioning.** Single "latest" tracking `main`.
- **Plugin-developer docs.** gRPC contract, `bossd-plugin-*` authoring
  stays out of the public site.
- **Internal/contributor docs.** `AGENTS.md`, `CLAUDE.md`,
  `docs/cron.md`, `docs/Mutation_Testing.md` stay where they are.
- **Adding a marketing landing page to the docs site.**
  `services/marketing` already serves `bossanova.dev`; the docs site
  serves docs only at `docs.bossanova.dev`.
- **Deleting `services/docs/.docusaurus/` build artifacts from git.**
  Not part of this change.
- **Restoring deferred pages.** Tracked in
  `docs/plans/2026-05-08-docs-future-extensions.md` with a
  re-introduction roadmap.

## What already exists (avoid reinventing)

- `services/docs/SCREENSHOTS.md` — single source of truth for screenshot
  slots. Reuse it; don't rename slots.
- `services/marketing/DESIGN.md` — design tokens and component
  primitives. Where the docs site needs visual touch-ups (e.g. the
  navbar accent color), pull from these tokens instead of inventing
  new values.
- The original implementation plan
  `docs/superpowers/plans/2026-05-04-docs-site.md` — its build/CI/Terraform
  sections are still authoritative for infra. This cleanup only
  touches content and sidebar.
- `make lint-docs` / `make test-docs` / `make build-docs` already wire
  through to `services/docs`. No Makefile changes needed.

## Failure modes to watch for

| Codepath | Realistic failure | Plan covers? |
|----------|-------------------|--------------|
| `docusaurus build` | Broken link to deleted page | yes — Step 7 / Step 9 fail loudly via `onBrokenLinks: 'throw'` |
| `cli-reference.md` stub | Drift between stub claims and `boss --help` output | yes — Step 8 fact-check pass |
| `privacy.md` | Documents a code path that no longer exists | yes — Step 8 explicit verify |
| `security-and-permissions.md` | Stale claim about `dangerously_skip_permissions` default | yes — Step 8 explicit verify |
| `quick-start.md` | Post-cut prose reads broken (paragraph leftover from a stripped link) | mitigated by Step 8 read-through and the explicit line-9 prose rewrite in Step 3 |
| `pr-lifecycle.md` (new) | Claims auto-merge or auto-review-response that aren't shipping | mitigated by the slim scope and Step 8 fact-check |
| `settings.md` (env table) | Drift from actual env-var handling | mitigated by Step 4 instruction to copy descriptions from HEAD, not from this plan |
| Cloudflare Pages preview | Build green locally, fails on Cloudflare | run a local `pnpm build && pnpm serve` and click around before merging |

If any retained page has a claim we cannot verify in 5 minutes, delete
the claim. Wrong docs are worse than missing docs.

## Acceptance criteria

The repo's `CLAUDE.md` mandates a session-completion workflow: quality
gates run, commits land, push succeeds. These gates are part of
acceptance — not a polish step.

- [ ] `services/docs/docs/` contains exactly the 17 files listed above (plus `_category_.json` files where Docusaurus needs them).
- [ ] `pnpm build` exits 0 with `onBrokenLinks: 'throw'` enforced.
- [ ] Sidebar matches the structure in **Sidebar after cleanup**.
- [ ] Every retained page has been read end-to-end during the fact-check pass; un-verifiable claims removed.
- [ ] **From repo root:** `make` (default target builds the Go workspace and the docs), `make lint`, `make lint-docs`, `make test`, `make test-docs`, `make build-docs` — all green.
- [ ] **CLAUDE.md session-completion workflow:** all changes committed via conventional-commit messages; `git pull --rebase`; `git push`; `git status` reports "up to date with origin".
- [ ] Cloudflare Pages preview clicked through manually for all 17 pages and the search box.
- [ ] `docs/plans/2026-05-08-docs-future-extensions.md` records the pre-deletion SHA and the catalog of removed pages.

When all eight boxes are checked, the docs site is shippable.
