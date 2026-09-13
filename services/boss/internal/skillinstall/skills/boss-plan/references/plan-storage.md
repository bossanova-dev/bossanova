# Plan storage

Store every implementation plan as a native tracker attachment. Do not load publish credentials or
write a plan link. Before any description, labels, estimate, priority, or state update:

1. Call `preparePlanAttachment` with the **target issue id**, Markdown filename, `text/markdown`,
   and byte size.
2. Save `uploadRequest.headers` in a private scratch JSON file named
   `.linear-plans/run-<RUN-SCRATCH-ID>/<RUN-ISSUE-ID>.attachment-headers-<n>.json`, retain its exact path until the PUT
   returns, then invoke `node "$BOSS_PLAN_TOOLBOX/plan-attachment.mjs" put "$PLAN_FILE" <uploadRequest.url>
<headers-json-file>` after running the toolbox preamble first. Delete that exact scratch file immediately after the PUT returns,
   whether it succeeds or fails; keep its path for terminal cleanup as a defense-in-depth fallback.
   `<RUN-ISSUE-ID>` is the planning run's issue id, not necessarily the target issue id: child epic
   attachments still use the parent epic id here, so one epic run's header scratch stays one
   recognisable set. Removability no longer rests on that prefix — every file above is inside this
   run's own `.linear-plans/run-<RUN-SCRATCH-ID>/` directory, which Phase 5 removes whole — but the basename
   is part of the declared `attachment-headers` family in
   `$BOSS_PLAN_TOOLBOX/plan-scratch-paths.mjs`, so keep it.
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

## Writing the description from a file

The description is composed and gated as a file, so it is written to the tracker as a file too.
Retyping those bytes into an inline argument is what defeats the gate: a block the guards proved
byte-verbatim stops being provably the same object the moment a model re-emits it, and a measured
incident recorded a two-character drift surviving every gate that way.

The write reuses the descriptor-emission pattern the comment path already proves. `write-description`
reads the body, validates it, and prints a `{tool, args}` record for you to execute through the
tracker's own interface — no raw API call, and the bytes never enter your context:

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
NEW=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-new.md"
node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" write-description --id "<ISSUE-ID>" --body-file "$NEW"
```

`$NEW` is deliberately the same file every Phase 4 gate read, not a fresh copy of it: a second
rendering is a second chance to differ, and the gates would have certified the file you did not send.

On success the verb exits 0 and prints one JSON line:

- `tool` — the adapter's `writeDescription` tool name. Execute `{tool, args}` as the description of
  the single tracker save; the remaining metadata fields ride the same save.
- `args.description` — the file's bytes, verbatim, including its trailing newline.
- `bytes` — the body's size **measured on disk** with `stat(2)`, not counted from a decoded string, so
  a multi-byte body reports its true size. Report this number; never substitute one you derived.
- `outcome` — `descriptor-emitted`. Branch on this, not on the exit status alone: an explicit success
  token is what stops a write that changed nothing from reading as a write that landed.

Every failure exits 2, writes a one-line reason to **stderr** and **nothing to stdout**, so a caller
that pipes stdout can never mistake an error for a descriptor. The failures split in two, and the
split decides whether a fallback is legitimate:

- **Capability absent** — the stderr line names a missing `writeDescription` operation. The op is
  optional, so this is a normal outcome for an adapter that does not declare it: fall back to sending
  the description inline on the existing save.
- **Body unusable** — an empty, whitespace-only, unreadable or missing `--body-file`. This is never a
  reason to fall back. The refusal is load-bearing: a blank description erases the reporter's original
  notes, and the tracker exposes no description history to recover them from. Fix what the run
  composed, or take the SAFE branch.

Verification of what landed belongs to the write-back check after the final save, and it reads the
tracker's **stored** description — asserting the section contract and the verbatim block against
those bytes. Do not add a byte comparison against the buffer you sent: the tracker renormalizes
markdown after every local gate has run (a `-` bullet stored as `*`), so such a check reds on every
run for a purely cosmetic reason while proving nothing the stored-document check does not.
