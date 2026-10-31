---
name: boss-verify-moves
description: Built-in boss-verify proof-describe extension. Reports renamed files and files whose only changed lines are import paths a rename explains. Not invoked directly.
x-boss-extension:
  extends: boss-verify
  role: proof-describe
  describe: describe.mjs
  areas: migrations, api-contract, infra, agent-rules, housekeeping, ui, other
---

# boss-verify-moves

Reports renamed files and files whose only changed lines are import paths a rename explains. Never marks an area breaking: the area detector already treats a pure move of a UI or other file as housekeeping.

The core runs `describe.mjs` directly, with no agent. It wraps
`toolbox/verify/verify-describe-moves.mjs`. A repo-local extension named `boss-verify-moves`
replaces it, and `"builtinExtensions": false` in `.boss-skills.json` turns every built-in off.
