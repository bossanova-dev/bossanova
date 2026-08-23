# Plan storage

Store every implementation plan as a native tracker attachment. Do not load publish credentials or
write a plan link. Before any description, labels, estimate, priority, or state update:

1. Call `preparePlanAttachment` with issue id, Markdown filename, `text/markdown`, and byte size.
2. Save `uploadRequest.headers` in a private scratch JSON file, retain its exact path until the PUT
   returns, then invoke `node "$BOSS_PLAN_TOOLBOX/plan-attachment.mjs" put "$PLAN_FILE" <uploadRequest.url>
<headers-json-file>` after running the toolbox preamble first. Delete that exact scratch file immediately after the PUT returns,
   whether it succeeds or fails; keep its path for terminal cleanup as a defense-in-depth fallback.
   **A successful PUT writes the HTTP status line to stdout, and that line is the proof of work.**
   Treat an exit 0 that printed **no** status line on stdout as a **failed PUT**, never a success:
   a helper whose entry-point guard does not fire exits 0 having uploaded nothing, and finalization
   would then mint an attachment row over bytes that were never written. Read the status, do not
   infer it from the exit code alone.
3. On a non-2xx PUT only, obtain one fresh prepare response and retry once with its URL and headers,
   using and immediately deleting a new scratch file for that response.
4. Call `finalizePlanAttachment` with the prepare response `assetUrl` and title
   `Implementation plan (<ISSUE-ID>)`; retain the returned attachment **id** and exact title for
   the completion report.
5. **Read the artifact back before trusting it.** Immediately after finalization, invoke
   `readPlanAttachment` with the retained attachment **id** and require **non-empty** content. On a
   transport error, retry the read **once**; a second transport error is an unverified artifact and
   takes the SAFE branch below without deleting anything, because an unreadable transport does not
   prove the bytes are missing. A read that **succeeds** and returns empty (or otherwise absent)
   content is a **confirmed-unreadable** artifact: the row exists but its object was never written,
   which still satisfies a consumer's "has a plan attachment" check and would strand the next build
   run. Delete that orphaned row with `deletePlanAttachment` on the retained id, then take the SAFE
   branch. Delete only on a confirmed-unreadable read — never on a transport error, which would
   destroy a healthy artifact.
6. **Supersede stale duplicate plan attachments only after verified read-back.** After the
   read-back succeeds, take a **single fresh** attachment list and call
   `selectSupersededPlanAttachments` with the freshly finalized id as `keepAttachmentId`. Delete
   each returned exact-title attachment id with `deletePlanAttachment`. A failed supersede list takes
   the SAFE branch: no plan metadata/state write, no deletes from stale state, and it does not roll
   back the successful publish. Retry each failed `deletePlanAttachment` once; if it still fails,
   report the surviving duplicate attachment id in the completion report and continue with the
   retained attachment as canonical. Every successful supersede deletion logs the deleted attachment
   id and exact title to stderr and carries both into the completion report next to the retained id.

Any prepare, PUT, finalization, **read-back**, or supersede-list failure means **no plan metadata/state write**. If a
PUT succeeds but finalization fails, report the orphaned upload; do not invent an attachment URL. The
SAFE branch on every failure edge is the same: **no plan metadata/state write**, a one-line stderr
reason, and a non-zero exit, leaving the ticket in its pre-run state for the next sweep to re-pick.
On success, save normal issue metadata without a plan link: the finalized **and read-back** attachment
is the canonical artifact.
On every terminal failure path, remove any retained attachment-header scratch file before returning.
