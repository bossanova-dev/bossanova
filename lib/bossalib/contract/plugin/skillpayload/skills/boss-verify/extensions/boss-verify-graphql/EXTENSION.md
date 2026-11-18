---
name: boss-verify-graphql
description: Built-in boss-verify proof-describe extension that reports GraphQL schema blocks, API signals and breaking changes using the gate's comparator. Not invoked directly.
x-boss-extension:
  extends: boss-verify
  role: proof-describe
  describe: describe.mjs
  areas: api-contract
---

# boss-verify-graphql

Reads `.graphql`, `.graphqls` and `.gql` SDL at the merge base and head. It includes every
GraphQL file directly in a directory holding a changed schema file, so unchanged sibling
declarations complete split schemas. Declarations in other directories are not included.
Schemas split across directories can consequently show an extension as a new type.

Reports a schema block, an API banner signal and one item per breaking change, using the
gate's own comparator. Added enum values and union members are displayed as dangerous;
they remain additive for the gate. If the directory schema is unreadable or its breaking
verdict differs from the changed files the gate reads, it uses only changed files for all
reported facts. Unreadable SDL is reported as not described, without counting another
breaking change: the gate already asks for that contract.

The core runs `describe.mjs` directly, with no agent. A repo-local extension named
`boss-verify-graphql` replaces this built-in. Set `proofDefaults.builtins.boss-verify-graphql`
to `false` in `.boss-skills.json` to disable it by name.
