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

| Scanner | Scope | Gate |
| --- | --- | --- |
| **gosec** | Go SAST, all `go.work` modules, threshold severity≥medium / confidence≥medium (`-severity`/`-confidence` flags in `security.yml`; `.gosec.json` carries the `global` settings) | **Blocks** |
| **govulncheck** | Call-reachable vulnerabilities in Go code + stdlib/toolchain | **Blocks** |
| **pnpm audit** | `--prod --audit-level=high` (production dependency advisories, high/critical) | **Blocks** |
| **CodeQL** | Semantic analysis (Go + JS/TS) | **Advisory** — results in the Security tab, never blocks |
| Secret scanning | Committed-credential detection | **GitGuardian (external)** — not in this workflow |

> **Current status — scanners are NON-BLOCKING pending baseline triage.**
> The scanners currently report findings without failing the build because the
> initial baseline exceeded the safe auto-triage threshold (see
> [`docs/security/threat-model.md`](docs/security/threat-model.md) findings
> register). Flipping gosec/govulncheck/pnpm-audit to blocking once the baseline
> is green is tracked in **BOS-28**. Each scanner carries a findings-only
> success-forcing control that the flip MUST remove, or findings will keep
> reporting green:
>
> - **gosec** — drop the `-no-fail` flag from its `args` (gosec exits 0 with
>   `-no-fail` regardless of findings).
> - **pnpm audit** — remove the JSON wrapper that converts valid advisory reports
>   back to success (pnpm exits non-zero for both advisories and registry/scanner
>   errors, so keep execution errors failing).
> - **govulncheck** — `-format json` exits 0 when vulnerabilities are found; switch
>   to text mode or add an explicit `jq` failure when `.finding` records exist.
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
- **Scope.** Because findings stay non-blocking (see the gate policy above), the
  notifier fires on genuine **workflow/infra failures** (broken build, matrix
  generation, setup/install errors, CodeQL init) — **not** on a vuln merely being
  found. When the scanners are flipped to blocking (**BOS-28**), findings will
  fail the job and the same notifier will alert on them too. The expected
  "GHAS not enabled" SARIF-upload error stays swallowed at the step level so it
  does not false-alarm.
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

- **No silent suppressions.** gosec runs with `nosec: enabled` +
  `show-ignored: true`, so suppressed findings still appear in output.
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

## Emergency disable / rollback

If the security gate misfires and blocks legitimate merges, restore merge
capability immediately using **one** of the following. The owner of this
procedure is the **repository admin (currently @recurser)**.

1. **Mark the `security` check non-required (fastest).**
   GitHub → **Settings → Branches → Branch protection rule for `main`** → under
   *Require status checks to pass*, remove the **`security`** check → **Save**.
   This unblocks merges without a code change. Re-add it once the gate is fixed.

2. **Revert the gate-flip commit.**
   If the gate was made blocking by a specific commit (the "flip the gate"
   change that removed `continue-on-error`), revert it:

   ```bash
   git revert <flip-commit-sha>   # restores continue-on-error: true on the scanners
   git push
   ```

   The scanners return to non-blocking (still reporting), and merges flow again.

3. **Disable the workflow entirely (last resort).**
   GitHub → **Actions → `security` workflow → ⋯ → Disable workflow**, or set the
   job to `if: false`. Use only if 1 and 2 are insufficient.

**Re-enable requirement:** any emergency disable MUST be tracked by an issue and
the gate re-enabled within **5 business days**, once the underlying finding is
triaged. Leaving the gate disabled silently is itself a security regression.
