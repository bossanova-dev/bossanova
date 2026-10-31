# Verify blockers

`judge` returns `blocked`, `pr`, `headSha` and `blockers`. Write the following envelope to a
run-temp file, then call `post --pr <n> --head <sha> --verdict blocked --blockers <file>`:

```json
{
  "version": 1,
  "pr": 42,
  "head": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "blockers": [
    {
      "id": "review:3f1c0a9b2e7d",
      "kind": "review-finding",
      "severity": "P2",
      "file": "src/credentials.go",
      "line": 198,
      "summary": "Peer acknowledgements can wait on each other",
      "text": "The full evidence supporting the finding."
    }
  ]
}
```

`validateBlockerFile` validates this versioned shape on both writing and reading. The list must be
nonempty. Each blocker has exactly the fields shown. `summary` has one line; `text` retains the
full evidence. A review finding has severity P0, P1 or P2; known locations use a file and positive line number.
Other kinds have null severity, file and line.

Kinds and stable ids:

| Kind             | Id                                                                                 | Meaning                                                                                                        |
| ---------------- | ---------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `review-finding` | `review:` + first 12 hex digits of SHA-256 over file, line and title joined by NUL | Verified, introduced, unfixed P0–P2 finding                                                                    |
| `check`          | `check:` + check name                                                              | Settled failed CI check, excluding `boss/*` contexts                                                           |
| `conflict`       | `conflict`                                                                         | Head conflicts with its base                                                                                   |
| `no-review`      | `no-review`                                                                        | Legacy only: readable in standing comments, never emitted; verify reviews a boss head lacking a receipt itself |

Ids are unique. Order is review findings by severity, file and line, checks by name, then conflict. P3 and style findings are notes and never blockers. Pre-existing, unverified and
fixed findings never block. Missing receipts route eligible heads to review; forks and unadmitted heads retain their park rules.
PARTIAL builds retain their park after review; conflicts and failed checks still block PARTIAL builds.

The posted comment lists every blocker, then its verify marker, then a final encoded envelope.
Visible evidence may be shortened to fit the comment limit; the envelope retains the complete
blocker file, including its full text:

```text
<!-- boss-verify-blockers head=<40-hex> v1 <base64url JSON> -->
```

Recover it without writing to the PR:

```bash
node "$BOSS_VERIFY_TOOLBOX/verify/verify-gate.mjs" blockers --pr <n> --out <file>
```

Optional `--head <sha>` selects a head (default: current head), and `--repo <owner/name>` selects
the repository. The newest writer-authored blockers comment for that head wins. Exit codes are
0 (file written), 3 (no standing blockers comment), 1 (read failure), 2 (usage error).

The standing review report carries a final machine-readable line:

```text
<!-- bs-review-findings v1 base=<40-hex> head=<40-hex> <base64url JSON> -->
```

The JSON array contains verified, unfixed findings with `id`, `severity`, `file`, `line`, `title`,
`evidence` and `introduced`. `parseReportFindings` is the sole reader; missing or malformed trailers
return null. Encoding protects embedded comment terminators. Large evidence is truncated with `…`
around the 48 KB encoded payload budget, retaining titles and keeping the report under the comment
limit.

The most recently updated valid report from a writer stands for subsequent heads until a newer
report supersedes it. A repair fixing a review-finding blocker must publish a superseding review
report that omits it or marks it fixed. A push alone does not discharge that finding.

A blocked post writes a PR comment and a failure `boss/verify` status targeting it. With auto-repair
off it also parks for a person, using the `, needs human` suffix. After two distinct blocker-mode repair rounds, a still-blocked verdict parks the PR as
`repair-exhausted`. Rounds survive pushes and repeated outcomes for the same blockers head count
once. An operator approval or send-back resets the count, granting repair two more rounds. The
park note lists the latest not-fixable blockers with reasons and round numbers, plus every blocker
still standing on the parked head. The legacy per-head repair attempt cap also parks the PR. Rearm on transition; only a changed head can resolve the send-back.

## Accepting a blocker

Only an operator runs this verb; the verify skill never accepts a finding itself. Copy the
review-finding id printed in backticks in the blocked comment, save the reason to a private file,
and pin the request to the current head:

```bash
node "$BOSS_VERIFY_TOOLBOX/verify/verify-gate.mjs" accept --pr <n> --head <40-hex> --blocker <id> --reason-file <file>
```

Optional `--repo <owner/name>` selects the repository. `--dry-run` performs the reads and returns
planned writes without creating a ticket or posting a comment. `--json` is also accepted. The verb
prints one JSON line; success and refusal both exit 0, while usage errors exit 2. The reason must
be nonempty, within the operator-note character limit and free of control characters.

The PR must be open at the requested head, and its newest trusted blockers comment must name
that PR and head and list the id. Only `review-finding` blockers can be accepted. A standing
`operator-note` means the head was sent back and prevents acceptance. Checks, conflicts, missing
review receipts and operator notes remain blockers.

Read and validation refusals make no writes: `pr-unreadable`, `not-open`, `head-moved`,
`comments-unreadable`, `no-blockers`, `blocker-not-listed`, `blocker-not-acceptable` (with the kind
in `detail`), `sent-back` and `tracker-create-unavailable`. The last refusal also covers missing
tracker credentials or team configuration before a network request. A failed tracker create
returns `tracker-create-failed`; an uncertain create returns `tracker-create-indeterminate` and
posts no PR comment. Retry to settle an uncertain create through its marker. If the ticket exists
but posting fails, `comment-failed` leaves the ticket for the next retry to reuse.

The follow-up enters the configured unplanned state with the planning label. Its final line is:

```text
<!-- boss-accept-followup repo=<owner/name> pr=<n> blocker=<id> -->
```

The marker omits the head, so accepting the same id on another head reuses the ticket. The ticket
records the finding, evidence, PR, accepted head and fenced operator reason. Only after the ticket
exists does the operator-authored PR comment record the acceptance, ending with:

```text
<!-- boss-accept head=<40-hex> blocker=<id> ticket=<identifier> -->
```

As with approval, only comments from repository writers are trusted. Success returns
`accepted: true`, the head, blocker, ticket and comment URL; a reused ticket adds
`existingTicket: true`. Repeating an already-recorded acceptance on the same head returns
`already: true` with no writes.

The acceptance applies only to the head it names. An acceptance newer than the blocked status
requests a re-judge of that unchanged head; verify drops only the accepted review-finding ids.
Other blockers still block, and parks and extension checks still apply. A fresh blocked status
prevents repeated re-admission from the same acceptance. An unreadable status timestamp fails
closed. After a push the operator must accept again; the same blocker id reuses its follow-up.
If the finding's file, line or title changes, its id changes and it needs a separate acceptance.

Blocked comments show each blocker id in backticks. When accepted findings coexist with remaining
blockers, an **Accepted by the operator** section lists the ids, follow-up tickets and acceptance
comment links before the marker. A pass with acceptances records one digest-idempotent head note
with those links; its final line is `<!-- boss-accepted-note head=<40-hex> digest=<16-hex> -->`.
The note is informational and never holds the verdict.

## Notes

When `judge` returns a non-empty `findingNotes` array, write the array itself to a run-temp JSON
file and pass `--notes <file>` to `post` on `pass`, `blocked`, `defect` or `human`. The flag is
invalid on `claim` and `wait`. An empty array posts nothing.

Notes never block the PR. They appear as plain bullets in one collapsed notes comment per judged
head, separate from blockers. The final trailer carries the notes and any follow-up ticket links:

```text
<!-- boss-verify-notes head=<40-hex> digest=<16-hex> <base64url JSON {version: 1, notes, tickets}> -->
```

Repeated input updates no comment. Follow-up links from the newest writer-authored notes comment
survive a re-judge or new head for notes whose ids remain. A failed notes write appears only in the
post output's `notes` field; it never changes the verdict, status, description, labels, tracker
writes or merge eligibility.

An operator can file notes with
`node "$BOSS_VERIFY_TOOLBOX/verify/verify-followups.mjs" follow-up --pr <n> (--note <id> [--note <id>]… | --all) [--repo o/r] [--dry-run] [--json]`.
It reads only the newest notes comment by the current account, creates tickets in the configured
unplanned state with the agent-plan label, and updates that comment's links. Each issue ends with
`<!-- boss-follow-up repo=<owner/name> pr=<n> note=<id> -->` for idempotent creation. Results are
`created`, `already-filed`, `indeterminate`, `failed`, or `planned` (dry run). One JSON line reports
every result; exit 0 means all selected notes succeeded, exit 1 reports a refusal or any failed or
indeterminate operation, and exit 2 reports invalid usage. Dry run performs no writes. Verify never
runs this operator verb.
