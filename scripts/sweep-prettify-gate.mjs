// scripts/sweep-prettify-gate.mjs
// Pure-core drift detection for the bs-sweep-prettify cron gate. NO side effects
// beyond the injected `run` command runner, and NO imports beyond node builtins —
// the cron worktree is dependency-free. The thin gate/gate.mjs wires a real
// execFileSync-backed runner; the unit test injects fakes.
//
// The gate answers one question before an LLM session spawns: "would a whole-repo
// `make format` pass change anything?" It probes the formatters `make format`
// actually applies, in CHECK mode (prettier `--check`, `gofmt -l`), so the tree
// is never mutated. Exit 0 == run the sweep; non-zero == skip (spend zero tokens).
//
// The probe is a SOUND SUBSET of `make format`, not a complete mirror of it: it
// covers the two cheap, high-value formatters (docs prettier + gofmt) and omits
// the rest (`syncpack`, the biome-owned web target, `services/docs`, `scripts`).
// This is deliberate. Soundness — every drift it reports IS fixed by `make
// format`, so the gate never triggers a run that produces an empty diff — is what
// matters for a cost gate; a missed formatter just defers the sweep to a later
// trigger, it never wastes an agent run. Crucially it does NOT probe goimports:
// `make format` runs `gofmt -w` + read-only `golangci-lint` per module and never
// applies `goimports -w`, so a goimports probe would exit 0 (run) on drift the
// sweep could not reconcile -> a guaranteed wasted NO_CHANGE run.

// Batch size for the gofmt arg list so a 1000+ file repo never blows ARG_MAX.
export const GO_BATCH = 300

// Prettier `--check` prints this exact phrase (on stderr) only when it finds
// mis-formatted files. It is the reliable way to tell prettier's own exit-1
// drift signal apart from a `pnpm`/Corepack exit-1 tooling failure that never
// reached prettier — the dependency-free-worktree failure the gate MUST fail
// closed on rather than mistake for drift. Stable across prettier 3.x.
export const PRETTIER_DRIFT_MARKER = 'Code style issues found'

// True iff a completed `pnpm run lint:docs` result carries prettier's `--check`
// drift marker. Checks both captured streams because pnpm forwards prettier's
// stderr (where the marker lands) through its own stderr. Pure.
export function prettierReportedDrift(result) {
  return `${result.stdout ?? ''}\n${result.stderr ?? ''}`.includes(PRETTIER_DRIFT_MARKER)
}

/** Split `items` into consecutive chunks of at most `size` (pure). */
export function chunk(items, size = GO_BATCH) {
  const out = []
  for (let i = 0; i < items.length; i += size) out.push(items.slice(i, i + size))
  return out
}

// Parse a `git ls-files '*.go'` runner result into a clean file list, failing
// CLOSED on BOTH failure shapes so a broken enumeration never masquerades as an
// empty (== clean Go tree) list. Lives in the pure core (not the thin gate) so it
// is verified by behavior — injected `{error}` / `{status:1}` / `{status:0}` fakes
// — rather than a source-substring grep. `run` returns `{ status, stdout }` for a
// completed process or `{ error }` when the binary could not be spawned (ENOENT).
export function parseGoFiles(listed) {
  if (listed.error) throw new Error(`git ls-files failed: ${listed.error.message ?? listed.error}`)
  if (listed.status !== 0) throw new Error(`git ls-files failed: exit ${listed.status}`)
  return String(listed.stdout ?? '')
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
}

// detectDrift decides whether a whole-repo `make format` pass would change
// anything and returns the set of probes that reported drift.
//
// A prerequisite probe (`make lint-check-version`, the guard `make format`
// itself runs first) plus both formatter probes — prettier over the docs/skills
// markdown globs and gofmt over tracked Go — are REQUIRED and fail CLOSED: a
// spawn error, a failed prerequisite, or ANY completed exit that is neither a
// clean result nor the formatter's specific drift signal, THROWS so the gate
// cannot prove state and therefore skips. This symmetry
// matters most in a dependency-free worktree (the gate's design target): a missing
// prettier/pnpm, OR a `pnpm`/Corepack exit-1 that fails before prettier runs, must
// fail closed (skip), never be mistaken for drift and waste an agent run. There is
// intentionally no goimports probe: `make format` does not apply `goimports -w`, so
// probing it would trigger runs the sweep can't reconcile (see the file header).
export function detectDrift(run, { goFiles = [] } = {}) {
  const reasons = []

  // 0. `make format` prerequisite. The formatter entry point the sweep runs is
  //    `format: lint-check-version` in the Makefile: that prereq exits BEFORE any
  //    formatter runs when the pinned golangci-lint is absent or the wrong
  //    version. On such a host, reporting drift would trigger a run whose Phase 1
  //    `make format` fails and emits NO_CHANGE — leaving the same drift for the
  //    next scheduled gate to re-trigger, burning an agent session on a tooling
  //    problem. Probe the SAME prereq target first and fail CLOSED (throw → skip)
  //    when `make format` cannot run, so every drift we report is one the sweep
  //    can actually reconcile.
  const prereq = run('make', ['lint-check-version'])
  if (prereq.error) {
    throw new Error(
      `make prerequisite probe failed to run: ${prereq.error.message ?? prereq.error}`,
    )
  }
  if (prereq.status !== 0) {
    throw new Error(
      `make format prerequisite (lint-check-version) failed (exit ${prereq.status}); cannot format`,
    )
  }

  // 1. Prettier `--check` over the docs/skills markdown globs (respects
  //    .prettierignore), via `pnpm run lint:docs`. Exit 0 = clean. Exit 1 is
  //    AMBIGUOUS: it is prettier's own drift signal, but `pnpm`/Corepack ALSO
  //    exit 1 when they fail *before* prettier runs — e.g. the gate's design
  //    target, a dependency-free worktree where Corepack can't fetch the pinned
  //    pnpm. Counting that tooling failure as drift would wake an agent on a
  //    problem `make format` can't fix (a guaranteed NO_CHANGE run), breaking the
  //    fail-closed contract. So exit 1 is drift ONLY when the output carries
  //    prettier's `--check` marker; a bare exit 1 (no marker) and every other
  //    non-zero exit (2 = config/parse error, 127 = missing binary) fail CLOSED.
  const prettier = run('pnpm', ['run', 'lint:docs'])
  if (prettier.error) {
    throw new Error(`prettier probe failed to run: ${prettier.error.message ?? prettier.error}`)
  }
  if (prettier.status === 0) {
    // Clean tree — no docs/skills markdown drift.
  } else if (prettier.status === 1 && prettierReportedDrift(prettier)) {
    reasons.push('prettier-docs')
  } else {
    throw new Error(`prettier probe errored (exit ${prettier.status}); cannot prove format state`)
  }

  // 2. gofmt -l over tracked Go files (batched). `gofmt -l` exits 0 and lists
  //    mis-formatted paths on stdout, so a non-zero exit is a genuine error
  //    (unreadable/unparseable file) → fail closed, not "no drift". Any listed
  //    path == drift. (`make format` applies `gofmt -w` per module but not
  //    `goimports -w`, so gofmt is the only Go formatter the sweep reconciles.)
  for (const batch of chunk(goFiles)) {
    const gofmt = run('gofmt', ['-l', ...batch])
    if (gofmt.error) {
      throw new Error(`gofmt probe failed to run: ${gofmt.error.message ?? gofmt.error}`)
    }
    if (gofmt.status !== 0) {
      throw new Error(`gofmt probe errored (exit ${gofmt.status}); cannot prove format state`)
    }
    if (String(gofmt.stdout ?? '').trim() !== '') {
      reasons.push('gofmt')
      break
    }
  }

  return { drift: reasons.length > 0, reasons }
}
