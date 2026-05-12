---
title: Setup Scripts
description: Configure a per-repo setup script that runs every time Bossanova creates a new worktree.
---

# Setup Scripts

Each repository can have an optional setup script that runs
automatically whenever a new worktree is created for a session. Useful
for installing dependencies, copying configuration files, or any other
per-worktree initialization.

## Configuring

Set a setup script when adding a repo, or update it later:

```bash
boss repo update my-repo --setup-script "npm install"
```

Clear it with an empty string:

```bash
boss repo update my-repo --setup-script ""
```

## Environment variables

The following environment variables are available to the setup script:

| Variable       | Description                                           |
| -------------- | ----------------------------------------------------- |
| `REPO_DIR`     | Path to the main git repository (the original clone). |
| `WORKTREE_DIR` | Path to the worktree being set up.                    |

These let you reference files in the main repo without hardcoding paths.
For example, to copy an `.env` file into each new worktree:

```bash
boss repo update my-repo \
  --setup-script 'cp "$REPO_DIR/.env" "$WORKTREE_DIR/.env" && npm install'
```
