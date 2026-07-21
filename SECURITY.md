# Security Policy

This document describes how to report a vulnerability in Bossanova, what the CI
security gate enforces, how findings may be suppressed, and how to disable the
gate in an emergency.

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

## What blocks a release (the gate policy)

Security scanning runs in [`.github/workflows/security.yml`](.github/workflows/security.yml)
and [`.github/workflows/codeql.yml`](.github/workflows/codeql.yml). The intended
gate policy is:

| Scanner         | Scope                                                                                                                                                                          | Gate                                                     |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------- |
| **gosec**       | Go SAST, all `go.work` modules, threshold severity≥medium / confidence≥medium (`-severity`/`-confidence` flags in `security.yml`; `.gosec.json` carries the `global` settings) | **Blocks**                                               |
| **govulncheck** | Call-reachable vulnerabilities in Go code + stdlib/toolchain                                                                                                                   | **Blocks**                                               |
| **pnpm audit**  | `--prod --audit-level=high` (production dependency advisories, high/critical)                                                                                                  | **Blocks**                                               |
| **CodeQL**      | Semantic analysis (Go + JS/TS)                                                                                                                                                 | **Advisory** — results in the Security tab, never blocks |
| Secret scanning | Committed-credential detection                                                                                                                                                 | **GitGuardian (external)** — not in this workflow        |

> **Current status — gosec, govulncheck, and pnpm audit are BLOCKING.**
> The Go/dependency scanners fail the build on findings. The BOS-9 baseline was
> triaged to green (see the [`docs/security/threat-model.md`](docs/security/threat-model.md)
> findings register) and the flips landed via **BOS-411** (govulncheck),
> **BOS-412** (pnpm audit), and **BOS-28** (gosec). For reference, each flip
> removed a findings-only success-forcing control:
>
> - **gosec** — dropped `-no-fail` and now propagates gosec's own exit code
>   (`${PIPESTATUS[0]}`). `.gosec.json` no longer sets `nosec: enabled`, so the
>   scan honors metadata-complete `#nosec` suppressions and blocks only on
>   unsuppressed findings; the "Golang errors" scan-error branch still fails the
>   job on load/compile errors so the notify alert fires.
> - **pnpm audit** — removed the JSON wrapper that converted valid advisory
>   reports back to success (execution errors still fail the job). **Known
>   exception:** with `pnpm@10` pinned (`packageManager` in `package.json`), npm
>   has retired the legacy audit endpoint pnpm 10 targets, so the scan returns
>   `ERR_PNPM_AUDIT_BAD_RESPONSE` (HTTP 410) and is degraded to a non-blocking
>   warning pending the pnpm 11 bulk-advisory migration — see
>   [Dependency advisory exceptions](#dependency-advisory-exceptions). Every
>   other execution error and any high/critical production advisory still fails
>   the job.
> - **govulncheck** — fails when `.finding` records exist instead of exiting 0
>   under `-format json`.
>
> CodeQL is non-blocking by design and stays that way.

## Failure monitoring

There is no PR to surface a red ❌ when a scan fails on `main` or on a scheduled
run, so failures are pushed to a GitHub issue instead.

- **Scheduled coverage of `main`.** `codeql.yml` runs on push-to-`main` and
  weekly (`27 3 * * 1`). `security.yml` ignores pushes to `main`, so
  gosec/govulncheck/pnpm-audit also run weekly (`41 4 * * 1`) to cover `main`.
- **Notification.** Both workflows have a `notify` job
  (`if: failure() && github.event_name != 'pull_request'`) that runs the
  [`notify-security-failure`](.github/actions/notify-security-failure/action.yml)
  composite action. On a failed push/scheduled run it opens — or comments on the
  existing open — issue labelled **`security-scan-failure`** (deduped to one open
  issue). Close it once resolved; the next failure opens a fresh one. PR
  failures are intentionally excluded — they are already visible on the PR.
- **Scope.** Now that gosec/govulncheck/pnpm-audit are blocking, a finding fails
  the job and — on a push or scheduled run (not a PR, which is already visible) —
  the same notifier alerts on it, alongside genuine **workflow/infra failures**
  (broken build, matrix generation, setup/install errors, CodeQL init). The
  expected "GHAS not enabled" SARIF-upload error stays swallowed at the step level
  so it does not false-alarm.
- **Manual checks.** `gh run list --branch main --status failure`, and the
  **Security** tab for CodeQL/gosec SARIF findings.

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
  is greppable and auditable (`grep -rn '#nosec'`). The blocking gosec gate
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

**Standing environment exception — pnpm 10 audit endpoint.** npm retired the
legacy `/-/npm/v1/security/audits/quick` endpoint that `pnpm@10` (currently
pinned via `packageManager`) targets; it now returns HTTP 410
(`ERR_PNPM_AUDIT_BAD_RESPONSE`). The workflow treats that specific
endpoint-retired error as a non-blocking warning (`security.yml`, pnpm-audit
step) so the release gate is not held hostage to a dead endpoint. This means the
production-advisory scan is effectively skipped until the project migrates to
pnpm 11's bulk advisory endpoint; every **other** execution error still fails
loud. Restoring the fully-blocking advisory path is the pnpm 11 migration's
responsibility.

## Emergency disable / rollback

If the security gate misfires and blocks legitimate merges, restore merge
capability immediately using **one** of the following. The owner of this
procedure is the **repository admin (currently @recurser)**.

1. **Mark the `security` check non-required (fastest).**
   The gate runs on pull requests into (and pushes to) **`staging`** and
   **`production`** — not `main` (`security.yml` ignores `main` pushes; the
   weekly schedule covers `main`). GitHub → **Settings → Branches → Branch
   protection rules for `staging` and `production`** → under _Require status
   checks to pass_, remove the **`security`** check → **Save**. This unblocks
   merges without a code change. Re-add it once the gate is fixed.

2. **Revert the gate-flip commit.**
   If the gate was made blocking by a specific commit (the "flip the gate"
   change — e.g. gosec's `-no-fail` removal + exit propagation), revert it:

   ```bash
   git revert <flip-commit-sha>   # restores the scanner's findings-only success-forcing control
   git push
   ```

   The scanners return to non-blocking (still reporting), and merges flow again.

3. **Disable the workflow entirely (last resort).**
   GitHub → **Actions → `security` workflow → ⋯ → Disable workflow**, or set the
   job to `if: false`. Use only if 1 and 2 are insufficient.

**Re-enable requirement:** any emergency disable MUST be tracked by an issue and
the gate re-enabled within **5 business days**, once the underlying finding is
triaged. Leaving the gate disabled silently is itself a security regression.
