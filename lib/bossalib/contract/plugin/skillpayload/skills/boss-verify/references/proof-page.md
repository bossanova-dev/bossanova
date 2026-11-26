# The core proof page

After `post` lands a `pass`, `blocked`, `defect` or `human` verdict, the core builds a
`boss-proof/1` record when the post was given `--judge <file>`. The record comes from that judge
output:

- **Confidence.** Its level, asks and the codes that lowered it. A record is `blocked` only when the
  judge listed blockers.
- **What it does.** The verify-intent record from `--intent`, else `unknown`.
- **Areas.** One per judge area, then one per ask no judge area carries. Each has a label,
  describer items and links, and the change class. A core record carries no media.
- **Review.** The build review and each extension that ran, the blockers with `file:line`, and
  the finding and not-applicable notes.
- **Health.** The judge's health codes. They describe the pipeline, not the PR, so the page shows
  them as one quiet line in its Generation check.

## Which verdicts get a page

By default (`proofDefaults.pages: "all"`), every terminal verdict gets a page. With
`proofDefaults.pages: "parked"` in the trusted base tree's config, only a `human` verdict gets
one, and a `pass`, `blocked` or `defect` note keeps the level with a null page and uploads
nothing; `"none"` publishes no page. A PR cannot enable pages for itself.

## Who writes the page

`proofDefaults.author`, read from the trusted base config like `pages`, says which pages the
[proof-author](proof-author.md) writes: `"all"` (default), `"parked"` (only `human` verdicts) or
`"none"`.

- **Dispatched.** `post --proof-spec <file>` adds the `proof-spec/1` the author wrote from the
  `proof-bundle` output.
- **Headless.** When a `post` or `proof` would record a page with no `--proof-spec` and the policy
  allows the author for this verdict, the proof process, including the sweep's detached one,
  builds the bundle and runs the author headless from a checkout at the head. It parses the last
  JSON object the author prints and records the page with it. `BOSS_PROOF_AUTHOR_TIMEOUT_MS`
  bounds the run (default ten minutes).

The core drops each invalid spec item and lists it in the page's Generation check; a spec for
another head is ignored whole. Every snippet is quoted from `git diff base..head`, never from the
spec. When the author is off, fails, times out or prints nothing usable, the page is the
deterministic one and its Generation check gives the reason in one line. That never changes the
verdict and is not a `proofMissing` loss.

## What the page shows

- **Header.** The spec's `title` and `lead`, the lead starting with the change type ("Bug fix.").
  Without a spec, the PR title (cut to 110 characters) and the first paragraph of the PR body as
  plain text (cut to 400 characters); an empty body gives no lead.
- **Banner.** The level, the reason and one row per signal. An author `signals` row replaces the
  describer's summary for its key and keeps the more severe pill; its `tests` and `checks` rows
  replace the file lists of the deterministic Tests and Checks rows.
- **Sections**, in order: Risks, Decisions, Bug fix, the ticket, repository and pack sections,
  the areas, Evidence, Tests and checks (a summary, then Proves, Doesn't prove and Also), Findings,
  After merge, Guardrails to add, Refactoring, Review noise, then Files. Bug fix, Tests and checks,
  Guardrails, Refactoring and Review noise come from the spec, each with a nav entry and count,
  and are left out when empty. Guardrail, refactoring and noise ids link like risk ids.
- **Diff links.** A risk or decision location, a snippet's path and a backticked changed
  `path:line` in prose open the PR's diff at that line in a new tab.

A parked head's page also always shows the record's park reasons, blockers and breaking changes.
Any page can also carry:

- **Repository sections.** Each section the base tree's `proof-section` extensions declare shows
  where they place it, filled from the spec's `sections`, with a nav entry and its own anchor
  (`#sec-<id>`). A section the spec leaves out is not shown, and its ticks work like decisions'.
- **Media.** `post --proof-media <dir>` reads the `proof-media/1` manifest `proof-bundle` left in
  `<dir>`; without it, the core runs the base tree's `proof-media` extension itself. Only files
  inside the extension's evidence directory are published, each as `media/<basename>`.
- **Player.** Every page loads the static `player.js` beside it, with or without media. It drives
  the video, the lightbox and section links that add no Back entry in a framed review tab, and
  does nothing it has no element for. The lightbox opens screenshots and, from each video's "Open
  large" button, video; its fullscreen button opens the media in a new tab where the page may not
  go fullscreen. The page loads no other script and works without it.
- **Documents.** Rendered `documents` show under Evidence, Documents: each head document's title,
  page count and a plain "Download PDF" link, with its page images opening in the lightbox, and
  base and head pages side by side when a base document pairs with it. The page never embeds a PDF.
  A framed page, which a sandbox may not let download, shows a note to open it in its own tab
  instead of the links.
- **Review findings and the ticket.** The page sidecar carries what review found: each unfixed
  and fixed finding with its severity, status, `file:line` and one line of evidence, shown once with
  the lines around it quoted from the merge-base diff, base side for a deleted line. A finding with
  no location shows as text. The acceptance rows fill "Does it satisfy" the ticket: a criterion is
  done only when its check passed on this head. Chips link a finding to the risk at its lines and to
  the decisions and criteria that name its file.
- **No recording.** A changed `ui` area with no head clip leads "Why it's waiting" with "No
  recording of the changed UI".

The core scans the page and its comment block for credential shapes. It also scans for the value
of any credential-named environment variable. A page with a hit is never uploaded.

## Where the page goes

The page is `index.html` under `<owner>/<repo>/<pr>/<head>/`, with its media and player beside
it. The first publisher that applies uploads them; only the trusted base tree is read:

1. The first `proof-publish` hook on the trusted base tree. The core never reads a hook from the PR
   head. The hook gets a private temporary `evidenceDir` and two calls: the media and the player
   first, with ten minutes, then `index.html` alone, with two minutes.
2. Else no page, when the base `.boss-skills.json` is unreadable (`config-unreadable`) or sets
   `publishConfig.proof: false` (`opted-out`).
3. Else the built-in `access-put` publisher, when the trusted base tree sets
   `publishConfig.proof.kind: "access-put"`. It PUTs media, player, then page to
   `<baseUrl>/_publish/<owner>/<repo>/<pr>/<head>/<name>`. Credentials come from
   `PROOF_ACCESS_CLIENT_ID` and `PROOF_ACCESS_CLIENT_SECRET`, falling back on macOS to the
   same-named generic-password accounts under `keychainService` (default `boss-proof-publish`).
   `baseUrl` must be HTTPS without userinfo, query or fragment; redirects are refused.
4. Else the daemon's evidence upload with destination `LOCAL_OR_HOSTED`, when the base tree sets a
   `publishConfig.proof` block: the daemon's own bucket, which must enable pages, else the
   daemon's hosted proof host.
5. Else the daemon's evidence upload with destination `HOSTED` (publisher `hosted`): the daemon's
   hosted proof host only.

When a hook or `access-put` fails for a setup reason (`credentials-missing`, `not-configured` or
`host-unreachable`), the same page goes through the `hosted` publisher instead, fitted to the same
limits. The proof note names the failed publisher and its reason, for example `access-put:
credentials missing (PROOF_ACCESS_CLIENT_ID)`, with variable names only. Content failures, HTTP
refusals and timeouts never fall back. The trusted base config turns this off with
`publishConfig.proof.hostedFallback: false`; `publishConfig.proof: false` and an unreadable config
also mean no fallback.

Both daemon uploads send the media, the player and the page (last) in one call when they fit 64
objects, with the media timeout; a larger page goes as 64-object media calls, then the page alone.
A hosted page is linked through the URL the daemon returns, not under the bucket prefix. A page
that is not uploaded keeps `page` `null` and records why: `opted-out`, `config-unreadable`,
`not-configured`, `quota-exhausted` or `over-limit` (the hosted host refused it), or
`daemon-unsupported` (a daemon too old to return a hosted page). An `over-limit` page is not
retried.

The page never goes up before its media, so a served page never links a missing file. Then
`post` leaves one proof note per head. The note holds the comment block, which is the page when
none was uploaded, and the `<!-- boss-proof: … -->` line the daemon reads for the head's page and
level. The note is rewritten in place only when its content changes.

After recording a note, verify mirrors its comment block (or one-line lead) into a `boss/proof`
GitHub check run through the daemon, with the proof page as its details URL when one exists.
This also shows proof in the checks list when no publisher is configured. A repository opts out
with `proofDefaults.fallback: "none"` in its trusted base config. The check never gates a merge;
a failed check publication is reported without changing the note or verdict.

## A repo's own proof wins

A repository extension can post a richer proof, with media, for a head. When any comment other than
the core's proof note already carries a valid proof line for the head, the core records nothing for
that head. The comment must come from an author with write access. If the core posted a note anyway,
it would become the newest comment and replace the richer record. The core also records nothing
when it cannot read the PR comments.

## Failures

A render, scan, media, upload, note or check-run failure is reported in `post`'s `proof` outcome. It
never changes or fails the verdict, and none of a hook's or extension's output is kept. When any
proof part is lost, `post` and `proof` also return `proofMissing` (listing each `part` and
`reason`, plus a `detail` when the failure has one), leave a head-bound `boss-proof-missing` PR
comment (rewritten in place as resolved if a later proof succeeds for the same head) and record a
`proof-missing` boss note. A `detail` such as `upload-failed`'s is one line of at most 160
characters naming the error's code or class and message; a daemon refusal names its code and the
failing stage, never the daemon's message. URLs are cut to their host, credential
shapes and credential-named environment values are redacted, and the comment and note both show
it. Benign reasons
count as no loss: `repo-proof-present` for the record; `not-parked`, `dry-run`, `not-configured`,
`disabled` and `opted-out` for the page; and `fallback-disabled`, `dry-run` and `daemon-unsupported`
for the check run. The sweep's mechanical posts pass `--proof-detach`. Their proof then runs as a
separate `verify-gate.mjs proof --pr <n> --head <sha> --judge <file>` process, so neither the upload nor the
author's run spends the sweep's post budget.

## Area names

Configured review areas get the id `config:<slug>`. The slug is the `alwaysReview` key in lower
case, with each run of other characters replaced by one `-`. For example, `API schema` gets the
slug `api-schema`, and a describer can target the id. The raw key stays the label. An id recorded
under the raw key, such as `config:API schema`, still resolves to its park code.

Proof record names and the daemon's asks allow no `:`. In the record, each run of characters
outside `[a-z0-9]` becomes `-`, so the `API schema` area is named `config-api-schema`.
