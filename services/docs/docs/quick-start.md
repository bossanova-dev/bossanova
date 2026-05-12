---
title: Quick Start
description: 'Walk through your first Bossanova session end-to-end: install, add a repo, chat with the agent, wait for CI, and archive when done.'
---

# Your first session

This page walks you from a fresh install to a merged-and-archived
session. Skip steps you've already done; cross-links point at the relevant
settings or guide page where one exists.

## 1. Install

Install `boss`, `bossd`, and the bundled agent plugins (`bossd-plugin-claude`
and `bossd-plugin-codex`) via Homebrew or from source. The full instructions, including how to
verify the daemon is running, live on the dedicated install page.

See **[Installation](./install.md)**.

## 2. Open boss for the first time

```bash
boss
```

The first launch runs a preflight check:
it verifies that `bossd` is reachable, that at least one agent runner
plugin (e.g. `bossd-plugin-claude` or `bossd-plugin-codex`) is loaded, and that your shell can
reach `git`. Without an agent plugin the daemon stays healthy but you
won't be able to start sessions. If the selected agent CLI is not on
`PATH`, install Claude Code or OpenAI Codex CLI first.

You'll land on the empty home view with a prompt to add a repo.

![Boss Terminal UI (TUI) on first launch, empty home view prompting you to add a repo](/img/screenshots/quick-start-first-launch.png)

## 3. Add a repo

There are three ways, in order of speed.

**A. One-liner from a local repo directory.** Fastest if you're
already inside the repo:

```bash
cd /path/to/your/repo
boss repo add
```

**B. Terminal UI (TUI) Repo Add (local path).** From the home view press `r`,
choose **Open project**, and point it at an existing local clone.

**C. TUI Repo Add (clone from URL).** From the home view press `r`,
choose **Clone from URL**, paste a `https://...` or `git@...` URL, and
pick a destination path. Bossd clones it for you and registers the
result.

The Repo Add wizard is documented in full at
the Repos screen (`r` from Home).

![Boss TUI Repo Add wizard showing the Open project / Clone from URL choice](/img/screenshots/quick-start-add-repo.png)

## 4. Configure (optional)

Before your first session, it's worth opening repo settings (press
`r` from the home view, then `enter` on the repo) to set:

- **Setup script:** runs after every worktree is created so the
  agent gets a working dev environment. See
  [Setup scripts](./guides/setup-scripts.md).
- **Linear API key:** unlocks the "Work on a Linear issue" session
  type when starting a session.
- **Automation flags:** opt this repo into auto-merge, CI repair, and
  other plugin-driven automation.

Run `boss repo update <repo-id> --help` for the full field list.
The defaults are fine. You can come back to this later.

![Boss TUI repo settings view with the setup script and automation toggles visible](/img/screenshots/quick-start-repo-settings.png)

## 5. Start a session

From the home view press `n`. The new-session wizard asks for two
things:

1. **Repo:** pick from the list of registered repos.
2. **Session type:** one of:
   - **Create a new PR:** fresh branch off your default branch.
   - **Work on an existing PR:** attach to an open PR.
   - **Quick Chat:** work directly in the repo's base folder, no
     worktree.
   - **Work on a Linear issue:** only shown when the repo has a
     Linear API key configured.

Then enter a session name (used as the branch name and PR title for
new-PR sessions). Bossd creates the worktree, runs the setup script,
and hands the agent its first prompt.

![New-session wizard showing the session-type table with PR / Quick Chat / Linear options](/img/screenshots/quick-start-new-session.png)

## 6. Chat with the agent

The TUI drops you into the agent's chat pane. Type your prompt and
press enter. The agent has the bundled
skills (small markdown helper files Bossanova installs alongside the agent) loaded: `boss`, `boss-repair`,
`boss-verify`, and `boss-finalize`, so it knows how to drive the
boss CLI and run the project's quality gates without you having to
spell them out.

Long-running sessions can be detached (`ctrl+x` or `ctrl+]` from
inside the chat pane. Press `?` from the chat pane for the keymap)
and re-attached later from home (`enter` on the row). See
[Parallel sessions](./concepts/worktrees.md#multiple-sessions) for how multiple
agents run side by side.

![Agent chat pane mid-conversation with skills loaded and a streaming reply](/img/screenshots/quick-start-chat.png)

## 7. Wait for CI to pass

For **Create a new PR** sessions, the agent opens the PR itself when
it considers the work done, typically via `gh pr create` after its
final commit. The PR number and CI status surface in the home view's
**PR** and **STATUS** columns within a few seconds (driven by the
daemon poll loop).

If automation is enabled for the repo, the
[repair plugin](./plugins.md#automation) watches the PR after it opens.
When CI fails, reviews request changes, or the base branch moves, it
starts a repair session that checks out the PR branch, makes the fix,
commits it, pushes it, and re-checks the PR until it is ready to merge.
See [PR Lifecycle](./guides/pr-lifecycle.md) for the full flow.

![Home view with a session showing a PR number and green CI status](/img/screenshots/quick-start-pr-open.png)

## 8. Archive when done

Once the PR is merged (or you've decided to abandon it), archive the
session:

- **From home:** highlight the row and press `a`.
- **From the CLI:** `boss archive <session-id>`.

Archiving removes the worktree but leaves the branch intact, so a
merged PR's history is untouched, and a stale branch can still be
inspected later. Archived sessions live on in the Trash view (`t` from Home)
until you purge them.

![Home view after archiving, the row is gone and the worktree directory is cleaned up](/img/screenshots/quick-start-archive.png)

## Next steps

- [How It Works](./how-it-works.md): the daemon, plugins, and event loop.
- [Web App](./guides/web.md): drive the same sessions from a browser.
- [Settings](./reference/settings.md): plugins, paths, cloud sync.
- [Troubleshooting](./help/troubleshooting.md): when things go sideways.
