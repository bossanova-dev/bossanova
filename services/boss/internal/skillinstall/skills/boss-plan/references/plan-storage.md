# Plan storage

Store every implementation plan as a native tracker attachment. Do not load publish credentials or
write a plan link. Before any description, labels, estimate, priority, or state update:

1. Call `preparePlanAttachment` with the **target issue id**, Markdown filename, `text/markdown`,
   and byte size. **`size` is a BYTE count measured on the exact file about to be PUT** — take it
   from `wc -c < "$PLAN_FILE"` (or `stat` on that same path), never from anything else. A character
   count is **forbidden**: a string length counts code units, so one multi-byte character makes the
   declared size disagree with the bytes and the signed PUT is rejected for a reason nothing in the
   message names. A count taken from the **buffer used to build the file** is forbidden for the same
   reason and is the harder one to spot: it measures a value that was correct before a final newline
   or a normalization pass, so every upload is short by a byte or two and the declared size is
   plausible. Measure the file, after it is written, on the path you are about to send.
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
   **The signed URL is short-lived — treat its validity as seconds, not minutes.** Run the prepare
   and the PUT **back to back**: no other tool call, no intervening message, no batch of prepares
   ahead of the uploads. A single interleaved step is enough to expire it, and an expired URL is
   rejected with the same status as a payload mismatch.
   **A successful PUT writes the HTTP status line to stdout, and that line is the proof of work.**
   Treat an exit 0 that printed **no** status line on stdout as a **failed PUT**, never a success:
   a helper whose entry-point guard does not fire exits 0 having uploaded nothing, and finalization
   would then mint an attachment row over bytes that were never written. Read the status, do not
   infer it from the exit code alone.
   **A usage exit is a caller error to correct, never a PUT failure to retry.** The helper exits 2
   with `plan-attachment: usage-error (no request sent)` as its first stderr line when an operand is
   missing; nothing was sent, so re-preparing a signed URL cannot fix it. Fix the invocation and
   re-run step 2. A PUT that actually reached the server exits 1 and prints the status — and, on a
   non-2xx, the server's own response body, which is what separates an expired URL (re-prepare) from
   a declared-size or header mismatch (fix step 1, then re-run).
3. On a non-2xx PUT only — a PUT that **reached the server** and was rejected, never a usage exit —
   obtain one fresh prepare response and retry once with its URL and headers,
   using and immediately deleting a new scratch file for that response.
4. Call `finalizePlanAttachment` with the prepare response `assetUrl` and title
   `Implementation plan (<ISSUE-ID>)`; retain the returned attachment **id** and exact title for
   the completion report.
5. **Read the artifact back before trusting it.** Immediately after finalization, invoke
   `readPlanAttachment` with the retained attachment **id** **in the mode that returns content**
   (`format="content"`). The other mode, `format="url"`, returns a URL and **no content at all**, so
   a read-back specified against it is not merely weak — it is unexecutable, and a run that reports
   it as satisfied verified nothing.
   **An attachment record's own `url` field is never a body source.** The bare
   `attachment(id) { url }` shape the tracker's API exposes is **unsigned**: fetching it answers an
   authorization error whose short JSON body is small-but-present, which is exactly what a
   presence test scores as a healthy attachment. Only the content mode above, or a **signed** URL
   the tracker just issued, carries the stored bytes.
   **That signed URL has exactly one source: `readPlanAttachment` in its `format="url"` mode.** It is
   the same mode disqualified just above — disqualified _as the read-back_, because it returns no
   bytes, which is a different question from where a fetchable URL comes from. Call it on the
   retained id to obtain the `<signed-url>` the digest command below takes, and treat that URL as
   seconds-lived exactly like the upload one. Where that mode is absent, or hands back the bare
   unsigned record `url` instead of a freshly signed one, the content-mode comparison below is the
   whole recipe — there is no third source, and inventing one repeats the unsigned-url mistake.
   **Compare the digest, not the size.** Require the stored bytes to equal the local plan file:
   `node "$BOSS_PLAN_TOOLBOX/plan-attachment.mjs" verify "$PLAN_FILE" <signed-url>` fetches the
   signed URL and compares SHA-256 against the file, printing `verify: match …` on stdout and
   exiting 0, or naming both digests and both byte counts on stderr and exiting 1. It never puts the
   stored body in your context. Where only the content mode is available, write its returned bytes
   to a scratch file inside this run's own scratch directory and compare
   that file's SHA-256 with the plan file's. A **digest mismatch is a confirmed-unreadable artifact**
   and takes the same delete-then-SAFE branch as an empty read below: the row exists over bytes that
   are not the plan. On a
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
   `selectSupersededPlanAttachments` with the freshly finalized id as `keepAttachmentId`, **and with
   `keepJustFinalized: true`** — this run finalized that id moments ago, which is knowledge no field
   on the attachment payload carries. Delete
   each returned exact-title attachment id with `deletePlanAttachment`. **The selector throws rather
   than returning an empty set** when it is handed exact-title candidates it cannot order and the
   declaration is absent: an empty array and "nothing is stale" are the same value, so a selector
   that quietly returned one let a stale duplicate survive under a clean report. A thrown selector is
   a **failed supersede list** and takes the SAFE branch below — it is not a reason to delete
   anything, and it never rolls back the verified publish. A failed supersede list takes
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

## Reading the stored description into a file

Every gate that compares the tracker's **stored** description reads a file, and that file must be
written by code from the tracker's response — never retyped by a model from a tool result. A retyped
copy is a rendering: one transcription slip either certifies bytes the tracker never stored or
reports drift that is not there. `read-description` is the read half of `write-description`; it
writes the stored bytes straight to disk and prints only a receipt:

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
ORIG=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-orig.md"
node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" read-description --id "<PHASE-1-ISSUE-UUID>" --out-file "$ORIG"
```

The run reads three times, each into its own declared scratch file: Phase 2 step 2 into
`<ISSUE-ID>.image-guard-orig.md` (the snapshot), Phase 4 step 5(f) into
`<ISSUE-ID>.image-guard-final.md` (the base the second save is edited from, in place, and the
`--intended` input step 6 then reads), and Phase 4 step 6 into `<ISSUE-ID>.image-guard-stored.md`
(the `--stored` input). All three sit under `.linear-plans/run-<RUN-SCRATCH-ID>/`.

The bytes are written to a temporary sibling that is renamed onto `--out-file`, so the only
observable states are the prior file, no file, or the complete file — never a truncated one a gate
would then compare. They are written **verbatim**: the tracker stores a description without a
trailing newline, so append nothing to the file afterwards, or every comparison against it reports a
difference the run introduced itself.

On success the verb exits 0 and prints one JSON line:

- `bytes` — the file's size **measured on disk** with `stat(2)` after the rename.
- `outcome` — `stored-description-written`, or `stored-description-empty` for a zero-byte stored
  description. The empty outcome is the evidence Phase 4's image-parity guard needs before it may
  pass `--allow-empty-original`; a zero byte count alone is not.
- `id` and `identifier` — the issue that actually answered.

**Identity — the receipt `id` must be the Phase 1 UUID.** Pass the issue **UUID** from the Phase 1
`getIssue` result as `--id`, never the human identifier, and treat a receipt whose `id` differs as a
failed read. The direct read authenticates with the environment's API key, which may belong to a
different workspace than the tracker's MCP server; a human identifier can collide across
workspaces, and the wrong issue's description would become `## Original notes`. A UUID cannot
collide.

**Route — chosen once per run.** Phase 2 step 2 decides the route for the whole run:

- Exit 0 there: every later read this run (5(f), step 6) uses `read-description` too, and a later
  exit 2 **fails that step** — it is never a per-site fallback. The direct read returns upload URLs
  unsigned while the tracker adapter's `getIssue` operation returns them signed, so a run that mixed
  the two would compare two different byte forms and could report drift on a correct write.
- Exit 2 there: the whole run takes the fallback — execute the tracker adapter's `getIssue`
  operation and copy the returned description byte-for-byte to the same `--out-file` path, for every
  read this run. Exit 2 covers an adapter without the `readDescription` capability and an unset API
  key (both named on stderr) as well as a tracker or write failure; a read has no side effects, so
  falling back from it is safe.

**Usage error — exit 64 stops.** A missing, valueless, repeated or unknown flag exits 64 with nothing
on stdout. It is a bug in the invocation, not a tracker condition: stop and fix the call. Falling
back on it would silently route every run through the retyped path this verb exists to remove.

Every failure — exit 2 or 64 — writes a one-line reason to **stderr** and **nothing to stdout**, and
leaves a pre-existing `--out-file` byte-unchanged. Name the route the run took in the Phase 6 report.
