#!/usr/bin/env node

// BOS-248 skill-generalization self-parity gate. Bossanova dogfoods its own
// generalized skill machinery as the reference consumer: this gate re-runs the
// four post-generalization deterministic skill signatures over the SAME
// committed fixtures and asserts they still match the committed
// pre-generalization baseline snapshots (BOS-249's capture-baseline.mjs), AND
// asserts bossanova's authored dogfood extension skills are discovered with the
// exact shapes the generalized cores expect. Green ⇒ prints a line containing
// `parity: OK` and exits 0; any mismatch ⇒ prints one or more lines each
// containing the token DRIFT plus the affected skill name, and exits 1.
//
// Part A (deterministic exact-match) is the load-bearing check: it re-derives
// each signature via capture-baseline.mjs and byte-compares stableStringify()
// against the committed baseline file. Part B (extension-discovery) asserts the
// structural contract the generalized cores rely on: the authored lens/surface/
// agent-driver/draft extensions are discovered by name with zero skips, their roles
// resolve in ROLE_SCHEMAS except behavior-shipping roles, and the two
// agent-driver extensions export valid driver records.
//
// Node builtins only (no new deps).

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

import { BASELINE_DIR, REPO_ROOT, computeAll, stableStringify } from './capture-baseline.mjs'
import { ROLE_SCHEMAS, discoverExtensions } from '../skill-extensions.mjs'

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
  },
  {
    core: 'boss-review',
    role: 'round',
    skill: 'boss-review',
    names: ['boss-review-crossmodel', 'boss-review-requesting', 'boss-review-thermonuclear'],
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
    names: ['boss-plan-draft'],
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
 * Names of the authored extensions that legitimately belong to a DIFFERENT role
 * under the SAME core than the given spec — the only role-mismatch skips that
 * are expected cross-role filtering rather than a misconfiguration. Any other
 * role skip (e.g. a typo'd `role:` on an authored or new same-prefix extension)
 * must be surfaced as drift.
 * @param {{ core: string, role: string }} spec
 * @returns {Set<string>}
 */
function crossRoleSiblingNames({ core, role }) {
  const names = new Set()
  for (const other of EXPECTED_EXTENSIONS) {
    if (other.core === core && other.role !== role) {
      for (const name of other.names) names.add(name)
    }
  }
  return names
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
    const { core, role, skill, names: expected } = spec
    const { extensions, skipped } = discoverExtensions({ core, role, root })
    // A same-prefix sibling that legitimately belongs to a DIFFERENT role is
    // filtered out with a `role "X", not "<role>"` skip — expected cross-role
    // filtering, not drift (boss-proof- spans both surface and agent-driver).
    // Only the KNOWN cross-role siblings are exempt: a role skip for any other
    // name (e.g. a typo'd `role:` on an authored or new same-prefix extension)
    // is a genuine misconfiguration and must surface as drift, as is any OTHER
    // skip reason (no SKILL.md, unreadable/absent marker, wrong extends).
    const crossRoleSiblings = crossRoleSiblingNames(spec)
    const badSkips = skipped.filter(
      (s) => !(/^role "/.test(s.reason) && crossRoleSiblings.has(s.name)),
    )
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
    if (role !== 'agent-driver' && role !== 'draft' && !ROLE_SCHEMAS[role]) {
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

// --- Part C: orchestration + CLI --------------------------------------------

/**
 * Runs both the deterministic exact-match (A) and the extension-discovery (B)
 * checks over the wired tree.
 * @param {{ root?: string, baselineDir?: string }} [opts]
 * @returns {Promise<{ ok: boolean, findings: string[] }>}
 */
export async function runParityGate({ root = REPO_ROOT, baselineDir = BASELINE_DIR } = {}) {
  const deterministic = checkDeterministicParity({ baselineDir })
  const extension = await checkExtensionParity({ root })
  const findings = [...deterministic.findings, ...extension.findings]
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

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().then((code) => process.exit(code))
}
