// scripts/sweep-prettify-gate.mjs
// Pure-core drift detection for the bs-sweep-prettify cron gate. NO side effects
// beyond the injected `run` command runner, and NO imports beyond node builtins —
// the cron worktree is dependency-free. The thin gate/gate.mjs wires a real
// execFileSync-backed runner; the unit test injects fakes.
//
// The gate answers one question before an LLM session spawns: "would a whole-repo
// `make format-all` pass change anything?" It probes the formatters `make
// format-all` actually applies, in CHECK mode (syncpack lint/format --check,
// prettier `--check`, biome `check`, `gofmt -l`), so the tree is never mutated.
// Exit 0 == run the sweep; non-zero == skip (zero tokens).
// The target is `format-all`, NOT `format`: BOS-371 made the bare `format` target
// the changed-files one, which on a fresh cron branch formats nothing (BOS-653).
//
// The probe is a sound mirror of the formatter families `make format-all` runs:
// root syncpack format/fix, docs/skills prettier, tracked Go gofmt, services/web
// biome, services/docs prettier, and scripts prettier. Soundness — every drift it
// reports IS fixed by `make format-all`, so the gate never triggers a run that
// produces an empty diff — is what matters for a cost gate. It deliberately does
// NOT probe goimports: each module's `format` target runs plain `gofmt -w` and
// never `goimports -w`, so a goimports probe would exit 0 (run) on drift the
// sweep could not reconcile -> a guaranteed wasted NO_CHANGE run.

// Batch size for the gofmt arg list so a 1000+ file repo never blows ARG_MAX.
export const GO_BATCH = 300

// Prettier `--check` prints this exact phrase (on stderr) only when it finds
// mis-formatted files. It is the reliable way to tell prettier's own exit-1
// drift signal apart from a `pnpm`/Corepack exit-1 tooling failure that never
// reached prettier — the dependency-free-worktree failure the gate MUST fail
// closed on rather than mistake for drift. Stable across prettier 3.x.
export const PRETTIER_DRIFT_MARKER = 'Code style issues found'
export const SYNCPACK_DRIFT_MARKER = '✘'
export const BIOME_FORMAT_DRIFT_MARKER = 'Formatter would have printed'

// True iff a completed `pnpm run lint:docs` result carries prettier's `--check`
// drift marker. Checks both captured streams because pnpm forwards prettier's
// stderr (where the marker lands) through its own stderr. Pure.
export function prettierReportedDrift(result) {
  return `${result.stdout ?? ''}\n${result.stderr ?? ''}`.includes(PRETTIER_DRIFT_MARKER)
}

export function syncpackReportedDrift(result) {
  const output = `${result.stdout ?? ''}\n${result.stderr ?? ''}`
  return output.includes(SYNCPACK_DRIFT_MARKER)
}

export function biomeReportedFormatDrift(result) {
  return `${result.stdout ?? ''}\n${result.stderr ?? ''}`.includes(BIOME_FORMAT_DRIFT_MARKER)
}

function assertNoSpawnError(result, label) {
  if (result.error)
    throw new Error(`${label} probe failed to run: ${result.error.message ?? result.error}`)
}

function checkMarkerProbe(result, { label, reason, reasons, isDrift }) {
  assertNoSpawnError(result, label)
  if (result.status === 0) return false
  if (result.status === 1 && isDrift(result)) {
    reasons.push(reason)
    return true
  }
  throw new Error(`${label} probe errored (exit ${result.status}); cannot prove format state`)
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

// detectDrift decides whether a whole-repo `make format-all` pass would change
// anything and returns the first probe that reported drift.
//
// An environment probe (`make lint-check-version`) plus both formatter probes —
// REQUIRED and fail CLOSED: a spawn error, a failed environment probe, or ANY
// completed exit that is neither a clean result nor the formatter's specific drift
// signal, THROWS so the gate cannot prove state and therefore skips. This symmetry
// matters most in a dependency-free worktree (the gate's design target): a missing
// formatter/pnpm, OR a `pnpm`/Corepack exit-1 that fails before the formatter runs,
// must fail closed (skip), never be mistaken for drift and waste an agent run. There
// is intentionally no goimports probe: `make format-all` does not apply
// `goimports -w`, so probing it would trigger runs the sweep can't reconcile (see
// the file header).
export function detectDrift(run, { goFiles = [] } = {}) {
  const reasons = []

  // 0. Toolchain environment probe. This is NOT a declared prerequisite of the
  //    formatter target — neither `format` nor `format-all` declares
  //    `lint-check-version`, and `format-all` delegates to per-module `format`
  //    targets that run gofmt/prettier and no golangci-lint at all (BOS-653
  //    corrected this comment; the probe itself is deliberately kept). It is a
  //    CONSERVATIVE check: a host that cannot satisfy the repo's pinned toolchain
  //    is a host whose formatter output we do not want to trust, so fail CLOSED
  //    (throw → skip) rather than wake an agent on it. The failure direction is a
  //    skipped sweep, never a bad one; the cost is an occasional deferred run.
  //    The throw's wording is pinned by bs-sweep-prettify-skill.test.mjs — change
  //    the message and that test, not this probe's behaviour.
  const prereq = run('make', ['lint-check-version'])
  if (prereq.error) {
    throw new Error(
      `make lint-check-version environment probe failed to run: ${prereq.error.message ?? prereq.error}`,
    )
  }
  if (prereq.status !== 0) {
    throw new Error(
      `make lint-check-version environment probe failed (exit ${prereq.status}); refusing to trust formatter output`,
    )
  }

  // 1. syncpack lint/format checks mirror the two root syncpack write-mode
  //    commands in `make format-all` (`syncpack fix` and `syncpack format`).
  //    Both commands use exit 1 for findings. Require syncpack's own non-clean
  //    output marker shape; bare lifecycle/Corepack failures fail closed.
  if (
    checkMarkerProbe(run('pnpm', ['syncpack', 'lint']), {
      label: 'syncpack lint',
      reason: 'syncpack',
      reasons,
      isDrift: syncpackReportedDrift,
    })
  ) {
    return { drift: true, reasons }
  }
  if (
    checkMarkerProbe(run('pnpm', ['syncpack', 'format', '--check']), {
      label: 'syncpack format',
      reason: 'syncpack',
      reasons,
      isDrift: syncpackReportedDrift,
    })
  ) {
    return { drift: true, reasons }
  }

  // 2. Prettier `--check` over the scripts formatter globs. The arg list is the
  //    check-mode twin of scripts/Makefile's write-mode `format` target.
  if (
    checkMarkerProbe(
      run('pnpm', [
        'exec',
        'prettier',
        '--check',
        'scripts/*.{cjs,mjs}',
        'scripts/bazel/*.mjs',
        'scripts/changelog/*.{cjs,mjs}',
        'scripts/skill-parity/*.{cjs,mjs}',
        'skills-toolbox/*.mjs',
        'skills-toolbox/{callback,cron-gates,session,finalize,tracker}/*.{cjs,mjs}',
        '.claude/skills/*/gate/*.mjs',
      ]),
      {
        label: 'scripts prettier',
        reason: 'scripts-prettier',
        reasons,
        isDrift: prettierReportedDrift,
      },
    )
  ) {
    return { drift: true, reasons }
  }

  // 3. services/docs prettier check. Use the package's `lint` script directly,
  //    not `make -C services/docs lint`, because that make target also typechecks
  //    and would be stricter than the formatter `make format-all` runs.
  if (
    checkMarkerProbe(run('pnpm', ['--dir', 'services/docs', 'run', 'lint']), {
      label: 'docs prettier',
      reason: 'docs-prettier',
      reasons,
      isDrift: prettierReportedDrift,
    })
  ) {
    return { drift: true, reasons }
  }

  // 4. services/web biome check. `biome check .` reports lint and format
  //    together, but the sweep can only rely on format drift being reconciled by
  //    `biome check --write .`. Treat only the formatter marker as drift; lint-only
  //    exits fail closed.
  if (
    checkMarkerProbe(run('pnpm', ['--dir', 'services/web', 'run', 'lint']), {
      label: 'web biome',
      reason: 'web-biome',
      reasons,
      isDrift: biomeReportedFormatDrift,
    })
  ) {
    return { drift: true, reasons }
  }

  // 5. Prettier `--check` over the docs/skills markdown globs (respects
  //    .prettierignore), via `pnpm run lint:docs`. Exit 0 = clean. Exit 1 is
  //    AMBIGUOUS: it is prettier's own drift signal, but `pnpm`/Corepack ALSO
  //    exit 1 when they fail *before* prettier runs — e.g. the gate's design
  //    target, a dependency-free worktree where Corepack can't fetch the pinned
  //    pnpm. Counting that tooling failure as drift would wake an agent on a
  //    problem `make format-all` can't fix (a guaranteed NO_CHANGE run), breaking the
  //    fail-closed contract. So exit 1 is drift ONLY when the output carries
  //    prettier's `--check` marker; a bare exit 1 (no marker) and every other
  //    non-zero exit (2 = config/parse error, 127 = missing binary) fail CLOSED.
  const prettier = run('pnpm', ['run', 'lint:docs'])
  if (
    checkMarkerProbe(prettier, {
      label: 'prettier',
      reason: 'prettier-docs',
      reasons,
      isDrift: prettierReportedDrift,
    })
  ) {
    return { drift: true, reasons }
  }

  // 6. gofmt -l over tracked Go files (batched). `gofmt -l` exits 0 and lists
  //    mis-formatted paths on stdout, so a non-zero exit is a genuine error
  //    (unreadable/unparseable file) → fail closed, not "no drift". Any listed
  //    path == drift. (`make format-all` applies `gofmt -w` per module but not
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
      return { drift: true, reasons }
    }
  }

  return { drift: false, reasons }
}
