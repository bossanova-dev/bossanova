# The core proof page

After `post` lands a `pass`, `blocked`, `defect` or `human` verdict, the core builds a
`boss-proof/1` record when the post was given `--judge <file>`. The record comes from that judge
output:

- **Confidence.** Its level, asks and the codes that lowered it. A record is `blocked` only when the
  judge listed blockers.
- **What it does.** The verify-intent record from `--intent`, else `unknown`.
- **Areas.** One per judge area, then one per ask no judge area carries. Each has a label,
  describer items and links, the change class and the changes the description never mentions. A
  core record carries no media.
- **Review.** The build review and each extension that ran, the blockers with `file:line`, and
  the finding and not-applicable notes.
- **Health.** The judge's health codes.

The core renders the page and scans the page and its comment block for credential shapes. It also
scans for the value of any credential-named environment variable. A page with a hit is never
uploaded.

## Where the page goes

The page is `index.html` under `<owner>/<repo>/<pr>/<head>/`. The first publisher that applies
uploads it:

1. The first `proof-publish` hook on the trusted base tree. The core never reads a hook from the PR
   head. The hook gets a private temporary `evidenceDir` holding only `index.html`, and two
   minutes to upload it.
2. Else the daemon's evidence upload, when the base tree's `.boss-skills.json` sets
   `publishConfig.proof`. The daemon accepts a page only when its destination enables pages.
3. Else no page is uploaded and `page` stays `null`.

Then `post` leaves one proof note per head. The note holds the comment block, which is the page
when none was uploaded, and the `<!-- boss-proof: … -->` line the daemon reads for the head's page
and level. The note is rewritten in place only when its content changes.

## A repo's own proof wins

A repository extension can post a richer proof, with media, for a head. When any comment other than
the core's proof note already carries a valid proof line for the head, the core records nothing for
that head. The comment must come from an author with write access. If the core posted a note anyway,
it would become the newest comment and replace the richer record. The core also records nothing
when it cannot read the PR comments.

## Failures

A render, scan, upload or note failure is reported in `post`'s `proof` outcome. It never changes
or fails the verdict, and none of the hook's output is kept. The sweep's mechanical posts pass
`--proof-detach`. Their proof then runs as a separate
`verify-gate.mjs proof --pr <n> --head <sha> --judge <file>` process, so the upload never spends
the sweep's post budget.

## Area names

Configured review areas get the id `config:<slug>`. The slug is the `alwaysReview` key in lower
case, with each run of other characters replaced by one `-`. For example, `API schema` gets the
slug `api-schema`, and a describer can target the id. The raw key stays the label. An id recorded
under the raw key, such as `config:API schema`, still resolves to its park code.

Proof record names and the daemon's asks allow no `:`. In the record, each run of characters
outside `[a-z0-9]` becomes `-`, so the `API schema` area is named `config-api-schema`.
