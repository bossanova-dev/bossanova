---
name: boss-verify-ci-media
description: Fetch CI proof recordings and rendered documents from configured artifact sources.
x-boss-extension:
  extends: boss-verify
  role: proof-media
  media: media.mjs
  order: 1000
---

# CI proof media

Read producers from the base revision's `proofDefaults.media.sources`. Fetch passing CI captures,
validate their files and print the merged `proof-media/1` manifest. Exit 10 when no sources exist.
Repo-local media extensions with the default order run first. Replace this extension by name or
disable it with `proofDefaults.builtins.boss-verify-ci-media: false`.

See the [manifest validator](../../toolbox/verify/verify-proof-media.mjs) for the accepted media contract.
