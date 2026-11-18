---
name: boss-verify-go
description: Built-in boss-verify proof-describe extension. Reports Go package exports, HTTP routes and touched tests using git and lexical parsing. Not invoked directly.
x-boss-extension:
  extends: boss-verify
  role: proof-describe
  describe: describe.mjs
  areas: migrations, infra, housekeeping, ui, no-sign-off, other
---

# boss-verify-go

Switches on for changed Go files governed by a `go.mod`. Reports added and removed exported
identifiers per package, route changes for `net/http`, chi and gin, and touched tests beside the
package's changed code. Generated files are skipped; exports from `package main` are skipped.
It never marks an area breaking, including removed methods and grouped declarations, whose
removal the deletions describer does not judge.

Routes use literal paths, chi `Route` prefixes and gin `Group` prefixes. Auth labels name detected
middleware; `not detected` means auth could not be identified, never that a route has no auth.
Dynamic paths, routers passed between functions and other frameworks are not reported. Repo-defined
areas are outside this built-in's declared areas.

The core runs `describe.mjs` directly, with no agent. A repo-local extension named `boss-verify-go`
replaces it. Set `proofDefaults.builtins.boss-verify-go` to `false` to disable this built-in.
