# Plan storage

Store every implementation plan as a native tracker attachment. Do not load publish credentials or
write a plan link. Before any description, labels, estimate, priority, or state update:

1. Call `preparePlanAttachment` with issue id, Markdown filename, `text/markdown`, and byte size.
2. Save `uploadRequest.headers` in a private scratch JSON file, retain its exact path until the PUT
   returns, then invoke `node "$BOSS_PLAN_TOOLBOX/plan-attachment.mjs" put "$PLAN_FILE" <uploadRequest.url>
<headers-json-file>`. Delete that exact scratch file immediately after the PUT returns, whether it
   succeeds or fails; keep its path for terminal cleanup as a defense-in-depth fallback.
3. On a non-2xx PUT only, obtain one fresh prepare response and retry once with its URL and headers,
   using and immediately deleting a new scratch file for that response.
4. Call `finalizePlanAttachment` with the prepare response `assetUrl` and title
   `Implementation plan (<ISSUE-ID>)`; retain the returned attachment **id** and exact title for
   the completion report.

Any prepare, PUT, or finalization failure means **no plan metadata/state write**. If a PUT succeeds
but finalization fails, report the orphaned upload; do not invent an attachment URL. On success, save
normal issue metadata without a plan link: the finalized attachment is the canonical artifact.
On every terminal failure path, remove any retained attachment-header scratch file before returning.
