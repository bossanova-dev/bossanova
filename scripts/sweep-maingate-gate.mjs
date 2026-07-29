// scripts/sweep-maingate-gate.mjs
// Pure-core decision logic for the bs-sweep-security "main-gate health probe".
// NO side effects in the decision core (selectStub / markerFor); the only I/O is
// the thin, injected Linear marker-fetch wrapper at the bottom, mirroring the
// established sweep-sentry-gate.mjs shape.
//
// The probe runs the two security-flavoured release-gate checks (nosec-metadata +
// gosec on the changed go.work modules) against the current `main` checkout, and
// hands the per-check results to selectStub, which picks ONE deduped Unplanned
// Linear stub to file. Detect + surface only — the sweep's Dependabot write path
// cannot auto-fix a gosec/nosec finding, so this feeds the bs-sweep-plan →
// bs-implement pipeline instead.
//
// Marker scheme (line-anchored `^MainGate: <id>$`, the exact last line of a stub
// body so the next run dedupes off it):
//   nosec-metadata → `MainGate: nosec-metadata`
//   gosec/<module> → `MainGate: gosec/<module>`

// The single marker for a check. A check with a `module` is a per-module gosec
// breakage (`MainGate: gosec/<module>`); a module-less check keys off its id alone
// (`MainGate: nosec-metadata`).
export function markerFor(check) {
  const id = String(check?.id ?? '')
  const module = check?.module ? String(check.module) : ''
  return module ? `MainGate: ${id}/${module}` : `MainGate: ${id}`
}

// Category rank: nosec-metadata is always filed before any gosec breakage. Within
// gosec, ties fall back to input (module) order — the skill passes checks in
// go.work module order, so the first changed module wins deterministically.
function categoryRank(check) {
  return check?.id === 'nosec-metadata' ? 0 : 1
}

// Human-readable title for the stub the pipeline will plan.
function titleFor(check) {
  if (check?.id === 'nosec-metadata') {
    return 'Release-gate drift on main: nosec-metadata suppression check failing'
  }
  return `Release-gate drift on main: gosec findings in ${check?.module ?? '(unknown module)'}`
}

// Render the stub body. The marker is the EXACT last line — no trailing newline —
// so the all-states dedupe scan keys off `^MainGate: <id>$` reliably. Findings are
// rendered as bounded bullets (bulk gosec JSON stays in the probe subagent; only a
// short per-check summary reaches here).
function bodyFor(check, marker) {
  const findings = Array.isArray(check?.findings) ? check.findings : []
  const lines = []
  if (check?.id === 'nosec-metadata') {
    lines.push(
      'The release-gate `nosec-metadata` check (`scripts/nosec-metadata-check.sh`) is red on ' +
        '`main`. A `#nosec` suppression is missing an inline `-- <reason>` and/or the ' +
        'owner/review-by/issue approval metadata SECURITY.md § Suppressions requires.',
    )
  } else {
    lines.push(
      'gosec found unsuppressed finding(s) (severity>=medium/confidence>=medium) in ' +
        '`' +
        (check?.module ?? '(unknown module)') +
        '` on `main`. This module changed vs `origin/staging`, so the next release PR ' +
        'will newly gate on it. Fix the finding, or add a metadata-complete `#nosec` per ' +
        'SECURITY.md.',
    )
  }
  lines.push('')
  lines.push('Detected by the `bs-sweep-security` daily main-gate health probe (BOS-491).')
  lines.push('Findings run through the exact CI flags from `.github/workflows/security.yml`.')
  if (findings.length > 0) {
    lines.push('')
    lines.push('Offenders (bounded sample):')
    for (const f of findings.slice(0, 20)) {
      // Collapse ALL Unicode line terminators (LF, CR, LS U+2028, PS U+2029) --
      // not just \r\n -- to a space. The tracked-marker scan's `^...$` runs with
      // the JS `m` flag, whose line boundaries include a bare CR and U+2028/U+2029;
      // a finding carrying one of those before `MainGate: ...` would otherwise forge
      // a phantom marker line and poison the dedupe (a real breakage then skipped as
      // already-tracked).
      lines.push(`- ${String(f).replace(/[\r\n\u2028\u2029]+/g, ' ')}`)
    }
  }
  lines.push('')
  lines.push(marker)
  return lines.join('\n')
}

/**
 * The single selection entry point — the skill's probe and the pure-core tests
 * both call this; neither re-implements the priority/dedupe logic.
 *
 * Input:  { checks: [{ id, module?, ok, findings: [...] }, ...],
 *           trackedMarkers: Set<string> | string[] }
 * Output: { toFile: { marker, title, body } | null, skipped: [{ marker, reason }] }
 *
 * Rules:
 *   - Only failing checks (`ok === false`) are breakages; passing checks are ignored.
 *   - A breakage whose marker is already in `trackedMarkers` is dropped
 *     (`already-tracked`) — a Linear stub already exists (any state, incl. archived).
 *   - Remaining breakages are ordered by [categoryRank, input order]; the first is
 *     filed (`toFile`), the rest are `deferred` (one stub per run — no silent caps).
 *   - All checks ok, or every breakage already tracked → `toFile: null`.
 */
export function selectStub({ checks, trackedMarkers } = {}) {
  const tracked = trackedMarkers instanceof Set ? trackedMarkers : new Set(trackedMarkers ?? [])
  const list = Array.isArray(checks) ? checks : []

  const skipped = []
  const eligible = []
  list.forEach((check, index) => {
    // Payload sanitization: a non-object or ok/missing check is not a breakage.
    if (!check || typeof check !== 'object' || check.ok !== false) return
    const marker = markerFor(check)
    if (tracked.has(marker)) {
      skipped.push({ marker, reason: 'already-tracked' })
      return
    }
    eligible.push({ check, marker, rank: categoryRank(check), index })
  })

  eligible.sort((a, b) => (a.rank !== b.rank ? a.rank - b.rank : a.index - b.index))

  if (eligible.length === 0) return { toFile: null, skipped }

  const [top, ...rest] = eligible
  for (const r of rest) skipped.push({ marker: r.marker, reason: 'deferred' })

  return {
    toFile: {
      marker: top.marker,
      title: titleFor(top.check),
      body: bodyFor(top.check, top.marker),
    },
    skipped,
  }
}

// ---------------------------------------------------------------------------
// Thin async I/O wrapper, kept out of the pure core. Reuses the shared paginated,
// fail-closed all-states marker scan (fetchMarkedLinearIssues) rather than
// re-implementing it — the same helper bs-sweep-sentry uses — parameterised to the
// `MainGate: ` marker namespace. `linearRequest` (scripts/linear-gate-lib.mjs) is
// injected so this module keeps a testable seam and no hidden network calls.
import { fetchMarkedLinearIssues } from './sweep-sentry-gate.mjs'

// The line-anchored marker regex used to lift `MainGate: <id>` lines out of a
// Linear issue description (case-sensitive, trailing whitespace tolerated). The
// content class excludes ALL four ECMAScript line terminators (CR, LF, LS U+2028,
// PS U+2029) — the same set bodyFor collapses on the write side — so the read side
// stays symmetric with the write side and cannot straddle an exotic boundary the
// `m`-flag `$` anchor would also break on.
const MAINGATE_MARKER_RE = /^MainGate: [^\r\n\u2028\u2029]+?[ \t]*$/gm

/**
 * Fetch every `MainGate: `-marked Linear issue (all states, incl. archived) and
 * return the exact Set of tracked marker lines for selectStub's dedupe. Bounded,
 * fail-closed pagination is inherited from fetchMarkedLinearIssues (a truncated
 * scan throws — a partial dedupe snapshot must never be triaged).
 */
export async function fetchTrackedMainGateMarkers({ apiKey, linearRequest, maxPages = 20 } = {}) {
  const nodes = await fetchMarkedLinearIssues({
    apiKey,
    linearRequest,
    maxPages,
    markerPrefix: 'MainGate: ',
  })
  const markers = new Set()
  for (const node of nodes) {
    const desc = String(node?.description ?? '')
    for (const line of desc.match(MAINGATE_MARKER_RE) ?? []) {
      markers.add(line.replace(/[ \t]+$/, ''))
    }
  }
  return markers
}

// CLI: `select <checks.json> <tracked.json>` prints the selectStub result. Mirrors
// sweep-sentry-gate.mjs's runCli so the skill can shell out to a single source of
// truth instead of re-implementing the decision inline.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

export function runCli(argv, { readFile = (f) => readFileSync(f, 'utf8') } = {}) {
  const [cmd, ...rest] = argv
  const json = (f) => JSON.parse(readFile(f))
  switch (cmd) {
    case 'select': {
      const checks = json(rest[0])
      const trackedMarkers = rest[1] !== undefined ? json(rest[1]) : []
      return JSON.stringify(selectStub({ checks, trackedMarkers }))
    }
    default:
      throw new Error(`unknown subcommand: ${cmd}`)
  }
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(runCli(process.argv.slice(2)))
    process.stdout.write('\n')
  } catch (err) {
    process.stderr.write(`sweep-maingate-gate: ${err.message}\n`)
    process.exit(1)
  }
}
