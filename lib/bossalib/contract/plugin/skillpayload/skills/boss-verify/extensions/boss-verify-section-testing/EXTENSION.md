---
name: boss-verify-section-testing
description: Built-in boss-verify proof-section pack for manual checks on a running build. Not invoked directly.
x-boss-extension:
  extends: boss-verify
  role: proof-section
  sections:
    - id: testing
      title: How to test it
      after: ticket
---

# How to test it

- Fill `testing` with checks a person makes by hand on a running build. Use a numbered `checklist`: each item's `context` states who, which environment and where to start; `text` gives the step; `expect` says what they should see.
- Give an item a `tag` when it can only run later, such as after deploy or another change, and explain why in a `facts` row.
- Leave out checks CI or the change's tests already prove. Omit the section when no manual check adds anything.

A repository replaces this section by declaring `testing` in its own `proof-section` extension, or hides it with `proofDefaults.sections.testing: false`.
