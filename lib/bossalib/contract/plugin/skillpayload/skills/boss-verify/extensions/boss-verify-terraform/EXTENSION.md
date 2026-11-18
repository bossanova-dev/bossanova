---
name: boss-verify-terraform
description: Built-in boss-verify proof-describe extension. Reports Terraform block changes and after-merge apply steps naming local calling roots. Not invoked directly.
x-boss-extension:
  extends: boss-verify
  role: proof-describe
  describe: describe.mjs
  areas: infra, no-sign-off
---

# boss-verify-terraform

Reports resources, data sources, modules, variables, outputs and local values changed in each directory holding
a changed `.tf` file. Moves, removals and renames without a moved block are stated without values.
Other top-level blocks, including providers, Terraform settings and imports, report configuration changes.
The result never marks an area breaking. Apply steps name roots reached through local module
sources, up to eight caller links with cycle detection. Registry and git sources cannot reveal
callers in another repository, so a module with no local caller reads as a root.
Deleted roots require decommissioning with their previous configuration and state; steps never
recommend applying in a root whose Terraform configuration was deleted. Configuration edits do
not establish whether a provider will update or replace a resource.
Git-detected directory migrations with renamed resource or module configuration keep before/after
comparisons together; boilerplate-only renames retain separate root actions. Migrations require review of state/backend
continuity and module source paths. Resource or module removals and unmoved address renames retain
conditional destroy or recreate warnings when state continuity is unknown. Ambiguous splits or merges
provide review-only migration steps.
Long or overflow steps retain change counts, required-variable instructions and migration review
before listing apply targets; detailed names remain in the facts when space allows.

The core runs `describe.mjs` directly, with no agent. It wraps
`toolbox/verify/verify-describe-terraform.mjs` and switches on only for changed `.tf` files.
A repo-local extension named `boss-verify-terraform` replaces it; `"builtinExtensions": false`
in `.boss-skills.json` turns every built-in off, and
`proofDefaults.builtins.boss-verify-terraform: false` disables this one.
