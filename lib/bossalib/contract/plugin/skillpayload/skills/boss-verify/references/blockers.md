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

| Kind             | Id                                                                                 | Meaning                                              |
| ---------------- | ---------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `review-finding` | `review:` + first 12 hex digits of SHA-256 over file, line and title joined by NUL | Verified, introduced, unfixed P0–P2 finding          |
| `check`          | `check:` + check name                                                              | Settled failed CI check, excluding `boss/*` contexts |
| `conflict`       | `conflict`                                                                         | Head conflicts with its base                         |
| `no-review`      | `no-review`                                                                        | Boss-class head lacks a build receipt                |

Ids are unique. Order is review findings by severity, file and line, checks by name, conflict,
then no review. P3 and style findings are notes and never blockers. Pre-existing, unverified and
fixed findings never block. Human, bot and fork missing receipts and PARTIAL builds retain their
existing park rules; conflicts and failed checks still block PARTIAL builds.

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
