---
name: boss-verify-workflows
description: Built-in boss-verify proof-describe extension. Reports GitHub Actions workflow trigger and filter changes without values. Not invoked directly.
x-boss-extension:
  extends: boss-verify
  role: proof-describe
  describe: describe.mjs
  areas: infra, no-sign-off
---

# boss-verify-workflows

Reports new and removed workflows, added and removed triggers, and changed filter keys. It
switches on only for changed `.yml` or `.yaml` files directly under `.github/workflows/`.
Comment-only changes produce no facts. Values are never printed and the area is never marked breaking.
Quoted filter values retain hashes. Changes to flow mappings or multiline flow sequences that cannot be parsed produce a
generic trigger-configuration change fact without guessing trigger names.

The core runs `describe.mjs` directly, with no agent. It wraps
`toolbox/verify/verify-describe-workflows.mjs`. A repo-local extension named
`boss-verify-workflows` replaces it; `"builtinExtensions": false` in `.boss-skills.json`
turns every built-in off, and `proofDefaults.builtins.boss-verify-workflows: false` disables this one.
