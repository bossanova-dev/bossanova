# Security Policy

This document describes how to report a vulnerability in Bossanova, which
security scans run and when, how findings may be suppressed, and how to disable
a scan in an emergency.

## Reporting a vulnerability

**Do not open a public GitHub issue for a security vulnerability.** Public issues
disclose the problem before a fix is available.

Instead, use **GitHub private vulnerability reporting**:

1. Go to the repository's **Security** tab → **Report a vulnerability**.
2. Describe the issue, affected component (`boss` / `bossd` / `bosso` / a
   `bossd-plugin-*` / `services/web`), reproduction steps, and impact.

If private reporting is unavailable to you, contact the repository maintainer
(currently **@recurser**) directly and mark the message as security-sensitive.

**Expected response:** an acknowledgement within **3 business days** and an
initial assessment (severity + remediation plan) within **10 business days**.
Please give us a reasonable window to ship a fix before any public disclosure.

## What runs, and when

Security scanning lives in [`.github/workflows/security.yml`](.github/workflows/security.yml)
and [`.github/workflows/codeql.yml`](.github/workflows/codeql.yml).

| Scanner         | Scope                                                                                                                                                                          | When it runs                                                                                    | On findings                 |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------- | --------------------------- |
| **CodeQL**      | Semantic analysis (Go + JS/TS)                                                                                                                                                 | Pushes to and PRs into `staging`/`production`, weekly on `main` (`27 3 * * 1`), manual dispatch | **Advisory** — never blocks |
| **gosec**       | Go SAST, all `go.work` modules, threshold severity≥medium / confidence≥medium (`-severity`/`-confidence` flags in `security.yml`; `.gosec.json` carries the `global` settings) | **Manual dispatch only**                                                                        | Fails the run               |
| **govulncheck** | Call-reachable vulnerabilities in Go code + stdlib/toolchain                                                                                                                   | **Manual dispatch only**                                                                        | Fails the run               |
| **pnpm audit**  | `--prod --audit-level=high` (production dependency advisories, high/critical)                                                                                                  | **Manual dispatch only**                                                                        | Fails the run               |
| Secret scanning | Committed-credential detection                                                                                                                                                 | GitGuardian (external)                                                                          | Not part of these workflows |

> **gosec, govulncheck and pnpm audit do not run automatically and gate nothing.**
> `security.yml` used to run on every release PR, on pushes to the release
> branches and weekly on `main`; on 2026-08-07 the repository owner made it
> dispatch-only because it was blocking releases on advisory churn (the
> rationale is recorded in the workflow's `on:` block, and
> `scripts/security-workflow.test.mjs` pins the trigger so it is not re-added by
> accident). Within a run the three scanners still fail on findings — the BOS-9
> baseline was triaged to green and the flips landed via **BOS-411**
> (govulncheck), **BOS-412** (pnpm audit) and **BOS-28** (gosec) — but nothing
> stops a vulnerable dependency or an unjustified `#nosec` from reaching a
> release unless someone runs the workflow. The weekly sweep was the only scan
> of unchanged code against newly published advisories, so that gap is real.
>
> Run the scanners before a release, or after a dependency advisory you care
> about, with:
>
> ```bash
> gh workflow run security.yml --ref <branch>
> gh run watch "$(gh run list --workflow security.yml --limit 1 --json databaseId --jq '.[0].databaseId')"
> ```
>
> CodeQL runs with `upload: never` and GitHub Advanced Security is not enabled,
> so its results stay in the run's logs and artifacts — they do not appear in
> the Security tab and are not a required check.

## Failure monitoring

- **Notification.** Both workflows have a `notify` job that runs the
  [`notify-security-failure`](.github/actions/notify-security-failure/action.yml)
  composite action on a failed scheduled run, manual dispatch, or push to
  `staging`/`production` (PR runs are excluded — they are already visible on
  the PR). It opens — or comments on the existing open — issue labelled
  **`security-scan-failure`** (deduped to one open issue). Close it once
  resolved; the next failure opens a fresh one.
- **Scope.** A finding fails the job, and so does a genuine workflow/infra
  failure (broken build, matrix generation, setup/install errors, CodeQL init);
  either one triggers the notifier on the runs above.
- **Manual checks.** `gh run list --workflow security.yml` and
  `gh run list --workflow codeql.yml --status failure`.

## Suppressions

Findings may be suppressed only with **inline, auditable metadata**. Every
suppression MUST carry, on the suppressing line or comment:

- the **rule id** (e.g. `G304`, or the advisory id),
- a one-line **reason**,
- an **owner** (`owner=@handle`),
- a **review-by** date (`review-by=YYYY-MM-DD`),
- the tracking **issue id** (`issue=BOS-NN`).

gosec example:

```go
// #nosec G304 -- path is a fixed worktree root joined with a validated session id;
// owner=@recurser review-by=2026-09-14 issue=BOS-NN
data, err := os.ReadFile(filepath.Join(worktreeRoot, sessionID, "meta.json"))
```

Rules:

- **No silent suppressions.** Every suppression is an inline `#nosec` comment
  carrying the metadata above **and** is mirrored in the findings register in
  [`docs/security/threat-model.md`](docs/security/threat-model.md), so each one
  is greppable and auditable (`grep -rn '#nosec'`). The gosec scan
  honors `#nosec` and fails only on **unsuppressed** findings; it no longer runs
  in `nosec: enabled` audit mode, which ignored every suppression and would make
  the gate unpassable. Because the gate now honors inline suppressions, the
  **`nosec-metadata`** CI job ([`scripts/nosec-metadata-check.sh`](scripts/nosec-metadata-check.sh),
  wired into `security.yml`) fails the build unless every suppression in scanned
  Go code (1) uses the `#nosec` form — the alternative `//gosec:disable` form is
  rejected outright so no suppression can escape the `grep -rn '#nosec'` audit,
  (2) names an explicit rule id (a naked `#nosec` that mutes every rule is
  rejected), and (3) carries an inline `-- <reason>`. That mechanical gate
  enforces the universal floor (auditable form + rule id + reason); the fuller
  `owner=@ / review-by= / issue=` metadata and register entry for high-risk
  suppressions below remain a human-reviewed policy audited via the register.
- **High-confidence findings in auth / exec / secret code may NOT be suppressed
  by an autonomous agent.** They require human review and approval. An automated
  run that hits one must defer (file/extend a tracking ticket), never suppress.
- Every suppression is mirrored in the findings register in
  [`docs/security/threat-model.md`](docs/security/threat-model.md).

## Dependency advisory exceptions

A production dependency advisory that cannot be fixed immediately (no patched
version, or an unavoidable transitive pin) may be excepted via a
`pnpm-audit-ignore.txt` file at the repository root. Each entry MUST carry the
advisory id, the affected package + path, a justification, an `owner=@handle`,
and a `review-by=YYYY-MM-DD` date. Excepted advisories are reviewed at least
**quarterly** and removed as soon as a patched version is available. The file is
created on first need; until then there are no standing exceptions.

**Standing environment exception — retired audit endpoint.** npm retired the
legacy `/-/npm/v1/security/audits/quick` endpoint that `pnpm@10` targeted; it
returns HTTP 410 (`ERR_PNPM_AUDIT_BAD_RESPONSE`). The workflow treats that specific
endpoint-retired error as a non-blocking warning (`security.yml`, pnpm-audit
step) so a dead endpoint does not fail the run. This means the
production-advisory scan is effectively skipped until the project migrates to
pnpm 11's bulk advisory endpoint; every **other** execution error still fails
loud. Restoring the fully-blocking advisory path is the pnpm 11 migration's
responsibility.

## Emergency disable / rollback

`security.yml` runs only when dispatched, so it cannot block a merge or a
release on its own; if a manual run misfires, do not act on it and fix the scan.
CodeQL is advisory and never blocks. If a security workflow is ever made
required or automatic again and misfires, the repository admin (currently
@recurser) restores merge capability with **one** of:

1. **Mark the check non-required.** GitHub → **Settings → Branches → Branch
   protection rules** for the affected branch → under _Require status checks to
   pass_, remove the check → **Save**. Re-add it once the scan is fixed.
2. **Revert the commit that made it blocking** (`git revert <sha>`), which
   returns the scanner to reporting-only.
3. **Disable the workflow** (last resort): GitHub → **Actions** → the workflow
   → **⋯ → Disable workflow**.

**Re-enable requirement:** any emergency disable MUST be tracked by an issue and
the check re-enabled within **5 business days**, once the underlying finding is
triaged. Leaving a check disabled silently is itself a security regression.
