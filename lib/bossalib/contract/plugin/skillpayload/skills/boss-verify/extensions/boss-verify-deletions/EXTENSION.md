---
name: boss-verify-deletions
description: Built-in boss-verify proof-describe extension. Lists deleted files and deleted exports (JS/TS, Go and Python), then searches the head tree for each with a whole-word `git grep`. Not invoked directly.
x-boss-extension:
  extends: boss-verify
  role: proof-describe
  describe: describe.mjs
  areas: migrations, api-contract, infra, agent-rules, housekeeping, ui, other
---

# boss-verify-deletions

Lists deleted files and deleted exports (JS/TS, Go and Python), then searches the head tree for each with a whole-word `git grep`. Marks the area breaking when one is still referenced, or when a search could not run.

The core runs `describe.mjs` directly, with no agent. It wraps
`toolbox/verify/verify-describe-deletions.mjs`. A repo-local extension named `boss-verify-deletions`
replaces it, and `"builtinExtensions": false` in `.boss-skills.json` turns every built-in off.
