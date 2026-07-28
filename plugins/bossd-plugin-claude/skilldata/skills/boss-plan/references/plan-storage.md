# Plan storage

Resolve `planStorageFor(config).kind` once. `r2` is the default and keeps the existing
`scripts/plan-publish.mjs --issue <id> --file "$PLAN_FILE"` flow: load the Phase 0 R2
credentials/config in the same shell, capture its URL, and write the canonical titled link.

For `tracker-attachment`, do not load publish credentials or write a plan link. Before any
description, labels, estimate, priority, or state update:

1. Call `preparePlanAttachment` with issue id, Markdown filename, `text/markdown`, and byte size.
2. Save `uploadRequest.headers` in a private scratch JSON file; invoke
   `node "$BOSS_PLAN_TOOLBOX/plan-attachment.mjs" put "$PLAN_FILE" <uploadRequest.url> <headers-json-file>`.
3. On a non-2xx PUT only, obtain one fresh prepare response and retry once with its URL and headers.
4. Call `finalizePlanAttachment` with the prepare response `assetUrl` and title
   `Implementation plan (<ISSUE-ID>)`.

Any prepare, PUT, or finalization failure means **no plan metadata/state write**. If a PUT succeeds
but finalization fails, report the orphaned upload; do not invent an attachment URL. On success, save
normal issue metadata without a plan link: the finalized attachment is the canonical artifact.
