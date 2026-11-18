---
name: boss-verify-section-deploy
description: Built-in boss-verify proof-section pack for deploy steps and settings around a merge. Not invoked directly.
x-boss-extension:
  extends: boss-verify
  role: proof-section
  sections:
    - id: deploy
      title: Before and after merge
      after: findings
---

# Before and after merge

- Fill `deploy` with what must happen around the merge for the change to work. Use a `checklist` grouped `Before deploy`, `Deploy` and `After deploy`.
- Use a `table` for new or changed settings per environment, and `facts` to say what happens when a setting is missing.
- Put observations that only confirm the change after merge in the core After merge list (`checklist.after`), rather than this section.
- Omit the section when the change deploys as usual with nothing extra to do.

A repository replaces this section by declaring `deploy` in its own `proof-section` extension, or hides it with `proofDefaults.sections.deploy: false`.
