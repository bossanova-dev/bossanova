---
title: PR Lifecycle
description: 'How a Bossanova session becomes a merged PR: what fires, what you decide, what to disable.'
---

# The PR Lifecycle

A typical Bossanova session ends with a merged PR. Here's the loop and
where you sit inside it.

## Steps

1. **Session opens.** You start a session in `boss`. The daemon creates
   a worktree, runs your setup script, and spawns the agent.
2. **Agent works, PR opens.** The agent commits, pushes a branch, and
   opens a PR. The session's PR status appears in the Home view.
3. **CI runs.** Bossanova does not run CI. Your usual GitHub Actions
   (or whatever) does.
4. **Repair fires (optional).** If CI fails or the PR has merge
   conflicts, the `repair` plugin spawns a follow-up session to fix
   them. Disable per-repo from the Repo Settings screen, or globally by
   setting `enabled: false` for the `repair` entry in
   [`settings.json`](../reference/settings.md).
5. **Review.** Reviewers comment as they normally would. If the repo
   has `auto-address-reviews` enabled, the daemon dispatches a
   follow-up session to address review feedback automatically.
6. **Merge.** Dependabot PRs auto-merge once checks pass when
   `auto-merge-dependabot` is enabled (the default for new repos). The
   one exception is a repository with an enabled verify-stage cron job
   whose verify admission policy admits bot PRs
   (`verifyDefaults.admission.bot.admit` in `.boss-skills.json`): there
   verify or a person merges them, and the daemon repairs them but never
   auto-merges them. A verify cron without bot admission never looks at
   Dependabot PRs, so the daemon keeps merging them. A repair that turns a PR green and
   mergeable returns its repair slot within one Dependabot poll once
   its repair lease ends. Other PRs follow the repository's verify or
   manual merge path.

   Verify's auto-merge is opt-in. With no `verifyDefaults.merge` block
   in `.boss-skills.json`, a PR that verify passes waits for a person to
   merge it. A repository lists the PRs verify merges on its own as
   `auto` rules, read from the base branch so a PR cannot change them:

   ```json
   "verifyDefaults": {
     "merge": {
       "auto": [
         { "level": "high" },
         { "area": "ui", "proof": "tests" }
       ]
     }
   }
   ```

   `{ "level": "high" }` merges every PR verify rates high, which is
   what verify did before merge rules existed. Protected paths, an
   extension's sign-off and asks the verify judge raised always wait for
   a person. See the
   [`verifyDefaults.merge` reference](https://github.com/bossanova-dev/bossanova/blob/main/docs/skills/skill-config.md)
   for every rule key.

7. **Archive.** Close the session in `boss`. The worktree is removed
   and the session row drops out of Home (visible later in Trash).

## Working multiple PRs concurrently

Each session is its own worktree, so there's no contention between
parallel sessions on the same repo. See
[Worktrees](../concepts/worktrees.md#multiple-sessions) for the model.

## Where to look when something stalls

- PR open but CI never starts → check your CI provider, not Bossanova.
- Repair fired but didn't fix the issue → see [Troubleshooting →
  Repair loop edge cases](../help/troubleshooting.md#repair-loop-edge-cases).
- Want to see what repair did → the session row in Trash links to the
  archived chat.
- Want to be notified when a PR merges, closes, or passes checks → see
  [GitHub PR callbacks](./github-callbacks.md).
