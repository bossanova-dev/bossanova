#!/usr/bin/env node

// BOS-248 skill-generalization self-parity gate. Bossanova dogfoods its own
// generalized skill machinery as the reference consumer: this gate re-runs the
// four post-generalization deterministic skill signatures over the SAME
// committed fixtures and asserts they still match the committed
// pre-generalization baseline snapshots (BOS-249's capture-baseline.mjs), AND
// asserts bossanova's authored dogfood extension skills are discovered with the
// exact shapes the generalized cores expect, and that no authored extension
// directory is left unclaimed. Green ⇒ prints a line containing
// `parity: OK` and exits 0; any mismatch ⇒ prints one or more lines each
// containing the token DRIFT plus the affected skill name, and exits 1.
//
// Part A (deterministic exact-match) is the load-bearing check: it re-derives
// each signature via capture-baseline.mjs and byte-compares stableStringify()
// against the committed baseline file. Part B (extension-discovery) asserts the
// structural contract the generalized cores rely on: the authored lens/surface/
// agent-driver/draft extensions are discovered by name with zero skips, their roles
// all resolve in ROLE_SCHEMAS — behaviour-shipping roles included, since BOS-744 derived
// discovery and validation from one EXTENSION_ROLES table — and the two
// agent-driver extensions export valid driver records. Part C (directory
// coverage) supplies the independent, non-circular teeth for the specs whose
// membership is discovery-derived — see checkDirectoryCoverage.
//
// Node builtins only (no new deps).

import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

import { BASELINE_DIR, REPO_ROOT, computeAll, stableStringify } from './capture-baseline.mjs'
import { ROLE_SCHEMAS, discoverExtensions } from '../../skills-toolbox/skill-extensions.mjs'

/** Snapshot filename → human skill label used in DRIFT messages. */
const SNAPSHOT_SKILL_LABELS = {
  'review-lens.snapshot.json': 'boss-review',
  'proof-surface.snapshot.json': 'boss-proof',
  'dag-schedule.snapshot.json': 'boss-epic',
  'plan-sections.snapshot.json': 'boss-plan',
}

/** The authored dogfood extensions each generalized core must discover. */
const EXPECTED_EXTENSIONS = [
  {
    core: 'boss-review',
    role: 'lens',
    skill: 'boss-review',
    names: ['boss-review-golang', 'boss-review-tui', 'boss-review-web'],
    // BOS-678: a lens extension declares which `.boss-skills.json` lensMap id it serves through
    // the optional `lens:` key on its own `x-boss-extension` marker. That declaration IS the
    // binding boss-review Phase 1 dispatches on, so an absent or mistyped one silently unwires
    // the extension — discovery keeps listing it, and the lens quietly falls back to its config
    // skill. Only the `lens` role declares bindings; every other spec omits the key.
    bindings: {
      'boss-review-golang': 'go',
      'boss-review-tui': 'tui',
      'boss-review-web': 'web',
    },
  },
  {
    core: 'boss-review',
    role: 'round',
    skill: 'boss-review',
    // BOS-852: round membership is open-ended. The core dispatches EVERY discovered round
    // wholesale (boss-review/SKILL.md `discover --core boss-review --role round`) with no
    // config registry pinning the set — unlike `lens`, whose literal list encodes a binding to
    // a `.boss-skills.json` lensMap id, or `agent-driver`, whose list is pinned to a driver.mjs
    // export. So a literal list here is pure friction: it makes adding a round a gate edit.
    //
    // The expected set is therefore the discovered set. That is NOT vacuous, and the reason is
    // checkDirectoryCoverage below: it reads the `.claude/skills/boss-review-*` directories
    // straight off disk — a fact produced by an author creating a folder, never by
    // discoverExtensions — and drifts on any directory no spec claims. Coverage is what keeps
    // membership honest; delete that check and this spec asserts nothing.
    discovered: true,
  },
  {
    core: 'boss-proof',
    role: 'surface',
    skill: 'boss-proof',
    names: ['boss-proof-docs', 'boss-proof-marketing', 'boss-proof-web'],
  },
  {
    core: 'boss-proof',
    role: 'agent-driver',
    skill: 'boss-proof',
    names: ['boss-proof-tui', 'boss-proof-webagent'],
  },
  {
    core: 'boss-plan',
    role: 'draft',
    skill: 'boss-plan',
    names: ['boss-plan-compound-engineering'],
  },
  {
    core: 'boss-build',
    role: 'notes',
    skill: 'boss-build',
    names: ['boss-build-notes'],
  },
  {
    core: 'boss-build',
    role: 'methodology',
    skill: 'boss-build',
    names: ['boss-build-ce'],
  },
  {
    core: 'boss-build',
    role: 'knowledge',
    skill: 'boss-build',
    names: ['boss-build-knowledge'],
  },
  {
    core: 'boss-plan',
    role: 'notes',
    skill: 'boss-plan',
    names: ['boss-plan-notes'],
  },
  {
    core: 'boss-review',
    role: 'notes',
    skill: 'boss-review',
    names: ['boss-review-notes'],
  },
  {
    core: 'boss-epic',
    role: 'notes',
    skill: 'boss-epic',
    names: ['boss-epic-notes'],
  },
  {
    core: 'boss-repair',
    role: 'notes',
    skill: 'boss-repair',
    names: ['boss-repair-notes'],
  },
]

// --- Part A: deterministic exact-match --------------------------------------

/**
 * Re-derives each signature and byte-compares it against the committed baseline.
 * @param {{ baselineDir?: string, snapshots?: Record<string, unknown> }} [opts]
 * @returns {{ ok: boolean, findings: string[] }}
 */
export function checkDeterministicParity({
  baselineDir = BASELINE_DIR,
  snapshots = computeAll(),
} = {}) {
  const findings = []
  for (const [name, label] of Object.entries(SNAPSHOT_SKILL_LABELS)) {
    const fresh = stableStringify(snapshots[name])
    let committed
    try {
      committed = readFileSync(join(baselineDir, name), 'utf8')
    } catch (err) {
      findings.push(`DRIFT ${label} ${name}: baseline unreadable (${err.message})`)
      continue
    }
    if (fresh !== committed) {
      findings.push(
        `DRIFT ${label} ${name}: recomputed signature no longer matches committed baseline`,
      )
    }
  }
  return { ok: findings.length === 0, findings }
}

// --- Part B: extension-discovery structural assertions ----------------------

/**
 * Replicates the private isValidDriverRecord contract from proof-agent-drivers.mjs
 * (not exported there) so the gate can independently assert authored driver shapes.
 * @param {unknown} d
 * @returns {boolean}
 */
export function isValidDriverShape(d) {
  return (
    !!d &&
    typeof d === 'object' &&
    typeof d.surface === 'string' &&
    d.surface !== '' &&
    typeof d.budgetClass === 'string' &&
    d.budgetClass !== '' &&
    typeof d.usable === 'function' &&
    typeof d.preflightMissing === 'function' &&
    (d.prebuild === undefined || typeof d.prebuild === 'function') &&
    typeof d.run === 'function'
  )
}

/**
 * The extension names a spec expects: its literal `names` when it pins them, otherwise —
 * for a `discovered: true` spec — whatever discovery returns for that core/role. See the
 * round spec's comment above for why a discovery-derived expectation is not vacuous.
 * @param {{ core: string, role: string, names?: string[], discovered?: boolean }} spec
 * @param {string} root
 * @returns {string[]}
 */
function resolveExpectedNames(spec, root) {
  if (spec.names) return spec.names
  return discoverExtensions({ core: spec.core, role: spec.role, root }).extensions.map(
    (e) => e.name,
  )
}

function sortedNames(extensions) {
  return extensions.map((e) => e.name).sort()
}

/**
 * Asserts the authored dogfood extensions are discovered with the exact shapes
 * the generalized cores expect.
 * @param {{ root?: string }} [opts]
 * @returns {Promise<{ ok: boolean, findings: string[] }>}
 */
export async function checkExtensionParity({ root = REPO_ROOT } = {}) {
  const findings = []
  for (const spec of EXPECTED_EXTENSIONS) {
    const { core, role, skill } = spec
    const expected = resolveExpectedNames(spec, root)
    const { extensions, skipped } = discoverExtensions({ core, role, root })
    // In THIS repo's curated dogfood tree, every skip is drift. There is no exemption, and the
    // absence of one is deliberate — see the `no x-boss-extension marker still reports DRIFT` case
    // in parity-gate.test.mjs, which pins it.
    //
    // BOS-744 removed a cross-role exemption from here that could never fire: it required a
    // `wrong-role` skip whose NAME appeared among EXPECTED_EXTENSIONS' cross-role entries, but
    // `discoverExtensions` `continue`s a sibling declaring a KNOWN other role without recording it
    // at all, and pushes `wrong-role` ONLY for an unknown declared role — while every name it
    // matched against has a known role. The two halves were mutually exclusive. (It also keyed on a
    // `/^role "/` regex over `reason`, the re-derivation the skip-code contract exists to end.)
    //
    // Note this is the OPPOSITE policy to a core's own ledger, and correctly so. A core runs in an
    // arbitrary consumer checkout, where a markerless same-prefix skill is somebody else's helper
    // and reporting it cries wolf forever — hence `deliberate`. This gate runs over one known tree
    // whose contents are authored here, so an unexplained same-prefix directory IS the drift.
    const badSkips = skipped
    if (badSkips.length > 0) {
      const detail = badSkips.map((s) => `${s.name} (${s.reason})`).join(', ')
      findings.push(`DRIFT ${skill} ${core}/${role}: unexpected skipped extension(s): ${detail}`)
    }
    const got = sortedNames(extensions)
    if (JSON.stringify(got) !== JSON.stringify([...expected].sort())) {
      findings.push(
        `DRIFT ${skill} ${core}/${role}: discovered [${got.join(', ')}], expected [${[...expected].sort().join(', ')}]`,
      )
    }
    // A discovered-but-unbound extension is the failure this catches: the names check above is
    // blind to it, because the descriptor is still returned. An expected name that went missing
    // entirely is already reported by that check, so skip it here rather than double-reporting.
    for (const [name, want] of Object.entries(spec.bindings ?? {})) {
      const found = extensions.find((e) => e.name === name)
      if (!found) continue
      if (found.lens !== want) {
        const declared = found.lens === undefined ? '(none)' : JSON.stringify(found.lens)
        findings.push(
          `DRIFT ${skill} ${core}/${role}: ${name} declares lens ${declared}, expected ${JSON.stringify(want)}`,
        )
      }
    }
    if (!ROLE_SCHEMAS[role]) {
      findings.push(`DRIFT ${skill} ${core}/${role}: ROLE_SCHEMAS has no schema for role "${role}"`)
    }
  }

  // Agent-driver extensions must export a valid driver record from driver.mjs.
  const driverSpec = EXPECTED_EXTENSIONS.find((s) => s.role === 'agent-driver')
  const { extensions } = discoverExtensions({ core: driverSpec.core, role: 'agent-driver', root })
  const drivers = await Promise.all(
    extensions.map(async (ext) => {
      try {
        const mod = await import(pathToFileURL(join(ext.dir, 'driver.mjs')).href)
        return { name: ext.name, record: mod.default ?? mod.driver ?? null }
      } catch (err) {
        return { name: ext.name, record: null, error: err.message }
      }
    }),
  )
  for (const { name, record, error } of drivers) {
    if (!isValidDriverShape(record)) {
      const why = error
        ? `driver.mjs import failed (${error})`
        : 'default export is not a valid driver record'
      findings.push(`DRIFT ${driverSpec.skill} ${name}: ${why}`)
    }
  }

  return { ok: findings.length === 0, findings }
}

// --- Part C: directory coverage ---------------------------------------------

/**
 * BOS-852: asserts every authored `.claude/skills/<core>-*` directory is claimed by exactly
 * one EXPECTED_EXTENSIONS spec.
 *
 * This is the independent, non-circular source of truth that keeps a `discovered` spec from
 * being a tautology. The directory listing is produced by an author adding a folder; the
 * membership check's expectation is produced by discoverExtensions. The two can genuinely
 * disagree, and this is the ONLY check that catches the disagreement: a `boss-review-foo`
 * whose marker declares a valid but UNSPECCED known role (say `role: methodology` under
 * `extends: boss-review`) is silently `continue`d by discovery
 * (skills-toolbox/skill-extensions.mjs), so it never lands in `skipped` for badSkips to flag
 * and never lands in `extensions` for the names check to miss. Without this it is invisible.
 *
 * @param {{ root?: string }} [opts]
 * @returns {{ ok: boolean, findings: string[] }}
 */
export function checkDirectoryCoverage({ root = REPO_ROOT } = {}) {
  const findings = []
  let entries
  try {
    entries = readdirSync(join(root, '.claude', 'skills'), { withFileTypes: true })
  } catch {
    // No skills dir -> nothing to cover. Mirrors discoverExtensions' own no-op rather than
    // throwing, so a consumer checkout without repo-local extensions stays green.
    return { ok: true, findings }
  }
  for (const core of new Set(EXPECTED_EXTENSIONS.map((s) => s.core))) {
    // Claim COUNTS, not a set: "claimed by exactly one spec" also rules out two specs
    // listing the same directory, which would hide a role mismatch behind a passing name check.
    const claims = new Map()
    for (const spec of EXPECTED_EXTENSIONS) {
      if (spec.core !== core) continue
      for (const name of resolveExpectedNames(spec, root)) {
        claims.set(name, (claims.get(name) ?? 0) + 1)
      }
    }
    // The trailing hyphen in the prefix is load-bearing: the standalone `.claude/skills/boss-proof`
    // skill is not a `boss-proof-` extension, so it must not be demanded of any spec.
    for (const entry of entries) {
      if (!entry.isDirectory() || !entry.name.startsWith(`${core}-`)) continue
      const claimed = claims.get(entry.name) ?? 0
      if (claimed === 0) {
        findings.push(
          `DRIFT ${core} ${entry.name}: directory is not claimed by any EXPECTED_EXTENSIONS spec (add a spec for its role, or fix its x-boss-extension marker)`,
        )
      } else if (claimed > 1) {
        findings.push(
          `DRIFT ${core} ${entry.name}: directory is claimed by ${claimed} EXPECTED_EXTENSIONS specs, expected exactly one`,
        )
      }
    }
  }
  return { ok: findings.length === 0, findings }
}

// --- Part D: orchestration + CLI --------------------------------------------

/**
 * Runs the deterministic exact-match (A), extension-discovery (B), and directory-coverage (C)
 * checks over the wired tree.
 * @param {{ root?: string, baselineDir?: string }} [opts]
 * @returns {Promise<{ ok: boolean, findings: string[] }>}
 */
export async function runParityGate({ root = REPO_ROOT, baselineDir = BASELINE_DIR } = {}) {
  const deterministic = checkDeterministicParity({ baselineDir })
  const extension = await checkExtensionParity({ root })
  const coverage = checkDirectoryCoverage({ root })
  const findings = [...deterministic.findings, ...extension.findings, ...coverage.findings]
  return { ok: findings.length === 0, findings }
}

export async function main() {
  const { ok, findings } = await runParityGate()
  if (ok) {
    process.stdout.write('parity: OK\n')
    return 0
  }
  for (const finding of findings) process.stdout.write(`${finding}\n`)
  return 1
}

import { isMainModule } from '../../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  main().then((code) => {
    process.exitCode = code
  })
}
