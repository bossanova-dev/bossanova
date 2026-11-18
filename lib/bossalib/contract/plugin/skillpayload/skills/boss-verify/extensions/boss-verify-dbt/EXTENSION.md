---
name: boss-verify-dbt
description: Describe dbt models, columns, materialisations and personal-data flags.
x-boss-extension:
  extends: boss-verify
  role: proof-describe
  describe: describe.mjs
  areas: migrations, other, no-sign-off
---

# dbt models

Reports added, removed and changed dbt models with materialisation, inferred column types,
personal-data flags and the Database signal. Projects are discovered from changed paths.
`proofDefaults.dbt.piiTerms` adds repo terms to the generic personal-data defaults; settings
come from the base tree. Flags are information and never produce risks or a Security signal.

Set `proofDefaults.builtins.boss-verify-dbt` to `false` to disable this built-in, or replace it
with a repo-local extension of the same name.
