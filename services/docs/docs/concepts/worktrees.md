---
title: Worktrees
description: How Bossanova uses git worktrees to isolate every agent session in its own directory.
---

# Worktrees

Every Bossanova session runs in its own git worktree. This page covers what
that means, where the worktrees live, when they're created and torn down,
and the gotchas worth knowing about.

## What is a worktree?

A git worktree is a second working copy of the same repository, on its own
branch, sharing the same `.git` object database as the main clone. You get
a real on-disk directory you can `cd` into, edit, run, and commit from
without touching the files in your primary checkout. The
[`git-worktree(1)`](https://git-scm.com/docs/git-worktree) man page is the
canonical reference; for Bossanova users the only thing you need to know
is that worktrees are cheap, isolated, and don't require a re-clone.

## Why a worktree per session?

A coding-agent CLI run bare against your main checkout has to take over
your branch and your working tree to make any changes. That makes running
two of them at once almost impossible. They fight over `git switch`,
trample each other's uncommitted work, and force you into a constant
`stash`/`switch`/`pop` dance.

Bossanova hands each session its own worktree, which gives you:

- **Parallel safety:** two sessions in the same repo can't step on each
  other's files. Each branches from `main` (or your configured base) and
  edits its own directory.
- **Inspectable state mid-run:** you can `cd` into any worktree directory
  and look at exactly what the agent has done so far. Run the tests. Open
  the diff. From the session view, press `t` to open a terminal window
  directly in the worktree folder. Nothing is hidden in a stash.
- **No branch ping-pong:** your primary checkout stays on whatever you
  had checked out. Bossanova never modifies it.
- **Cheap teardown:** archiving a session removes the worktree directory
  and prunes the metadata. The branch survives.

## Where worktrees live

The default base directory is `~/.bossanova/worktrees`, defined in
[`lib/bossalib/config/config.go`](https://github.com/bossanova-dev/bossanova/blob/main/lib/bossalib/config/config.go).
Override it with the `worktree_base_dir` field in `settings.json`; see
the [Settings reference](../reference/settings.md).

Inside the base directory, paths follow `<repo-name>/<branch>`:

```
~/.bossanova/worktrees/
├── my-app/
│   ├── fix-login-bug/        # session 1 worktree
│   └── add-dark-mode/        # session 2 worktree
└── infra/
    └── upgrade-terraform/    # session 3 worktree (different repo)
```

Your main clone (the one you ran `boss repo add` against) stays exactly
where it was. The relationship looks like this:

```
~/code/my-app/                         # your primary checkout
└── .git/                               # shared object DB

~/.bossanova/worktrees/my-app/
├── fix-login-bug/                     # branch: fix-login-bug
│   └── .git                            # file containing: gitdir: <abs path into primary .git/worktrees/...>
└── add-dark-mode/                     # branch: add-dark-mode
    └── .git                            # file containing: gitdir: <abs path into primary .git/worktrees/...>
```

Each session worktree's `.git` isn't a directory and isn't a symlink.
It's a small text file whose contents are `gitdir: <absolute path>`
pointing back into the primary clone's `.git/worktrees/` metadata. That
indirection is how a single object database serves all of them.

## Lifecycle

The flow lives in
[`services/bossd/internal/session/lifecycle.go`](https://github.com/bossanova-dev/bossanova/blob/main/services/bossd/internal/session/lifecycle.go).

1. **Start:** `StartSession` runs `git worktree add -b <branch>` from
   the primary clone. The branch is derived from the session title (or
   supplied explicitly for cron). The base is fetched fresh from `origin`
   so the worktree starts from the latest remote state.
2. **Setup:** if the repo has a setup script configured, it runs inside
   the new worktree. See [Setup Scripts](../guides/setup-scripts.md).
3. **Run:** the agent runner plugin starts its CLI inside the worktree.
4. **Archive:** `ArchiveSession` runs `git worktree remove --force`,
   which deletes the directory but leaves the branch in place. You can
   resurrect the session later (re-creates the worktree from the branch)
   or merge / delete the branch yourself.

## Gotchas

- **Branch already exists.** Two sessions in the same repo with the same
  derived branch name will collide. `StartSession` returns
  `ErrBranchExists` and the second one fails. Use a different title, or
  pass `ForceBranch` (the cron path does this with a unique
  `cron-<slug>-<unix>` suffix per fire).
- **Dirty worktrees.** If you make uncommitted changes inside a worktree
  and then archive the session, those changes are discarded along with
  the directory. The branch only retains what you committed.
- **Stale worktree refs after a daemon crash.** If `bossd` is killed
  mid-archive, git can be left with a worktree directory that no longer
  exists on disk but still appears in `git worktree list`. Run
  `git worktree prune` from your primary clone to clean it up. The
  archive path runs this automatically when forcing a branch overwrite.
- **The base directory is shared across daemons.** Don't point two
  `bossd` instances at the same `worktree_base_dir`; they'll race on
  directory creation. One daemon per machine is the normal case.

## See also

- [How It Works](../how-it-works.md): where the worktree leg sits in
  the bigger picture.
- [Setup Scripts](../guides/setup-scripts.md): what runs in the worktree
  before the agent starts.
- [Settings reference](../reference/settings.md): configuring
  `worktree_base_dir`.

## Multiple sessions

Each session gets its own worktree. Two sessions on the same repo run
in two separate directories with independent indexes. Neither blocks
the other. See [Scheduled Sessions](../guides/scheduled-sessions.md)
for how the scheduler uses this.
