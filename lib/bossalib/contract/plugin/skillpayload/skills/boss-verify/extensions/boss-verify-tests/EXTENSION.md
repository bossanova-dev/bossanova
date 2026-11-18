---
name: boss-verify-tests
description: Built-in boss-verify proof-describe extension reporting changed tests and passed checks. Not invoked directly.
x-boss-extension:
  extends: boss-verify
  role: proof-describe
  describe: describe.mjs
  areas: migrations, api-contract, infra, agent-rules, housekeeping, ui, no-sign-off, other
---

# boss-verify-tests

Lists changed test files beside the changed code they test, with added and removed case names
for Go, JavaScript, TypeScript and Python. Never marks an area breaking. Case names are lexical;
dynamic titles and unsupported languages contribute only file facts.

The Tests and Checks banner signals come only from housekeeping, preserving reviewed areas'
own banner rows. A change without a housekeeping area has no PR-wide signals. Checks name only
passed checks supplied by the core; names are untrusted text from the head's CI configuration,
never a merge signal.

The core runs `describe.mjs` directly. A same-named repo extension replaces this built-in;
`proofDefaults.builtins.boss-verify-tests: false` disables it, and `"builtinExtensions": false`
disables every built-in in `.boss-skills.json`.
