---
name: boss-verify-protos
description: Built-in boss-verify proof-describe extension. Runs `<buf> breaking` on each buf module holding a changed `.proto`, against the module at the merge base. Not invoked directly.
x-boss-extension:
  extends: boss-verify
  role: proof-describe
  describe: describe.mjs
  areas: api-contract
---

# boss-verify-protos

Runs `<buf> breaking` on each buf module holding a changed `.proto`, against the module at the merge base. The binary is `BUF` when set to an executable, else `buf` on `PATH`. Marks the area breaking when buf reports a violation; a missing buf or buf config is reported as not checked.

The core runs `describe.mjs` directly, with no agent. It wraps
`toolbox/verify/verify-describe-protos.mjs`. A repo-local extension named `boss-verify-protos`
replaces it, and `"builtinExtensions": false` in `.boss-skills.json` turns every built-in off.
