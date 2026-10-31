---
name: boss-verify-dependencies
description: Built-in boss-verify proof-describe extension. Lists each manifest change with its bump level, added and removed direct dependencies, new install or lifecycle scripts and new transitive packages. Not invoked directly.
x-boss-extension:
  extends: boss-verify
  role: proof-describe
  describe: describe.mjs
  areas: dependencies
---

# boss-verify-dependencies

Lists each manifest change with its bump level, added and removed direct dependencies, new install or lifecycle scripts and new transitive packages. Marks the area breaking for a major bump or a new or changed install script.

The core runs `describe.mjs` directly, with no agent. It wraps
`toolbox/verify/verify-describe-dependencies.mjs`. A repo-local extension named `boss-verify-dependencies`
replaces it, and `"builtinExtensions": false` in `.boss-skills.json` turns every built-in off.
